package proxyrouter

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/acptest"
)

type recordingProxy struct {
	name    string
	started chan string
}

func (p recordingProxy) Serve(_ context.Context, in io.ReadCloser, out io.WriteCloser) error {
	defer in.Close()
	reader := bufio.NewReader(in)
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ProtocolVersion uint16 `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.NewDecoder(reader).Decode(&request); err != nil {
		return err
	}
	if request.Method != "_proxy/initialize" {
		return io.ErrUnexpectedEOF
	}
	p.started <- p.name
	return json.NewEncoder(out).Encode(map[string]any{
		"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{
			"protocolVersion": request.Params.ProtocolVersion,
		},
	})
}

func start(t *testing.T, router *Router) *bufio.ReadWriter {
	t.Helper()
	return acptest.Serve(t, router.Serve)
}

func TestProxyRouterRequiresExactVersion(t *testing.T) {
	v1 := make(chan string, 1)
	v2 := make(chan string, 1)
	rw := start(t, New().
		WithV1(recordingProxy{name: "v1", started: v1}).
		WithV2(recordingProxy{name: "v2", started: v2}))
	_ = json.NewEncoder(rw).Encode(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "_proxy/initialize",
		"params": map[string]any{
			"protocolVersion": 2,
			"info":            map[string]any{"name": "client", "version": "1"},
		},
	})
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			ProtocolVersion uint16 `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.NewDecoder(rw).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Result.ProtocolVersion != 2 {
		t.Fatalf("protocol = %d", response.Result.ProtocolVersion)
	}
	select {
	case name := <-v2:
		if name != "v2" {
			t.Fatalf("selected %q", name)
		}
	default:
		t.Fatal("v2 proxy did not run")
	}
	select {
	case name := <-v1:
		t.Fatalf("v1 proxy unexpectedly ran: %q", name)
	default:
	}
}

func TestProxyRouterPreservesInitialFrameAndFutureFields(t *testing.T) {
	seen := make(chan string, 1)
	proxy := acptest.ProxyFunc(func(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
		defer in.Close()
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil {
			return err
		}
		seen <- line
		return nil
	})
	rw := start(t, New().WithV2(proxy))
	initialize := `{"jsonrpc":"2.0","id":1,"method":"_proxy/initialize","params":{"protocolVersion":2,"info":{"name":"client","version":"1"},"future":true}}`
	if _, err := rw.WriteString(initialize + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-seen:
		if strings.TrimSpace(line) != initialize {
			t.Fatalf("replayed initialize = %s, want %s", line, initialize)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy did not receive initialize")
	}
}

// batchRejection is the exact line the reference SDK sends for any JSON-RPC
// batch array.
const batchRejection = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"JSON-RPC batches are not supported; send each message individually"}}`

// parseErrorLine is the exact line the reference SDK sends for a line that is
// not valid JSON; quoted is the line, without its terminator, as a JSON string.
func parseErrorLine(quoted string) string {
	return `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error","data":{"line":` + quoted + `}}}`
}

func TestProxyRouterAnswersUnreadableFirstFramesAndKeepsReading(t *testing.T) {
	v1 := make(chan string, 1)
	v2 := make(chan string, 1)
	rw := start(t, New().
		WithV1(recordingProxy{name: "v1", started: v1}).
		WithV2(recordingProxy{name: "v2", started: v2}))
	for _, frame := range []struct{ wire, answer string }{
		{"[]\n", batchRejection},
		{`[{"jsonrpc":"2.0","id":1,"method":"_proxy/initialize","params":{"protocolVersion":2,"info":{"name":"client","version":"1"}}},{"jsonrpc":"2.0","method":"future/notification","params":{}}]` + "\n", batchRejection},
		{`[{"jsonrpc":"2.0","id":1,"method":"_proxy/initialize","params":{"protocolVersion":1}}]` + "\n", batchRejection},
		{`[{"jsonrpc":"2.0","id":1,"result":{}}]` + "\n", batchRejection},
		{"[\n", parseErrorLine(`"["`)},
		{" [1,\r\n", parseErrorLine(`" [1,"`)},
		{"\n", parseErrorLine(`""`)},
		{`{"jsonrpc":"2.0","id":1,"method":"_proxy/initialize"` + "\n", parseErrorLine(`"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"_proxy/initialize\""`)},
		{"[1e400]\n", parseErrorLine(`"[1e400]"`)},
	} {
		if _, err := rw.WriteString(frame.wire); err != nil {
			t.Fatal(err)
		}
		if err := rw.Flush(); err != nil {
			t.Fatal(err)
		}
		line, err := rw.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSuffix(line, "\n"); got != frame.answer {
			t.Fatalf("frame %q response = %s, want %s", frame.wire, got, frame.answer)
		}
	}
	if len(v1) != 0 || len(v2) != 0 {
		t.Fatal("a proxy implementation ran for an unreadable frame")
	}

	// A single _proxy/initialize after the rejected batches is routed.
	if _, err := rw.WriteString(`{"jsonrpc":"2.0","id":2,"method":"_proxy/initialize","params":{"protocolVersion":1}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	line, err := rw.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(line); got != `{"id":2,"jsonrpc":"2.0","result":{"protocolVersion":1}}` {
		t.Fatalf("initialize after batches = %s", got)
	}
	select {
	case name := <-v1:
		if name != "v1" {
			t.Fatalf("selected %q", name)
		}
	default:
		t.Fatal("v1 proxy did not run")
	}
}

func TestProxyRouterRejectsFutureVersion(t *testing.T) {
	rw := start(t, New().WithV2(recordingProxy{name: "v2", started: make(chan string, 1)}))
	_ = json.NewEncoder(rw).Encode(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "_proxy/initialize",
		"params": map[string]any{"protocolVersion": 3},
	})
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rw).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != -32600 {
		t.Fatalf("response = %+v", response)
	}
}

func TestProxyRouterValidatesExactV2Schema(t *testing.T) {
	rw := start(t, New().WithV2(acptest.ProxyFunc(func(context.Context, io.ReadCloser, io.WriteCloser) error {
		t.Error("invalid v2 proxy initialize must not reach implementation")
		return nil
	})))
	_ = json.NewEncoder(rw).Encode(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "_proxy/initialize",
		"params": map[string]any{"protocolVersion": 2},
	})
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	var response struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rw).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != -32600 {
		t.Fatalf("response = %+v", response)
	}
}
