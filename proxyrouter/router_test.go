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
	if request.Method != "proxy/initialize" {
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

func TestProxyRouterSelectsRequestedV2(t *testing.T) {
	v1 := make(chan string, 1)
	v2 := make(chan string, 1)
	rw := start(t, New().
		WithV1(recordingProxy{name: "v1", started: v1}).
		WithV2(recordingProxy{name: "v2", started: v2}))
	_ = json.NewEncoder(rw).Encode(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "proxy/initialize",
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
	initialize := `{"jsonrpc":"2.0","id":1,"method":"proxy/initialize","params":{"protocolVersion":2,"info":{"name":"client","version":"1"},"future":true}}`
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
		{`[{"jsonrpc":"2.0","id":1,"method":"proxy/initialize","params":{"protocolVersion":2,"info":{"name":"client","version":"1"}}},{"jsonrpc":"2.0","method":"future/notification","params":{}}]` + "\n", batchRejection},
		{`[{"jsonrpc":"2.0","id":1,"method":"proxy/initialize","params":{"protocolVersion":1}}]` + "\n", batchRejection},
		{`[{"jsonrpc":"2.0","id":1,"result":{}}]` + "\n", batchRejection},
		{"[\n", parseErrorLine(`"["`)},
		{" [1,\r\n", parseErrorLine(`" [1,"`)},
		{"\n", parseErrorLine(`""`)},
		{`{"jsonrpc":"2.0","id":1,"method":"proxy/initialize"` + "\n", parseErrorLine(`"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"proxy/initialize\""`)},
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

	// A single proxy/initialize after the rejected batches is routed.
	if _, err := rw.WriteString(`{"jsonrpc":"2.0","id":2,"method":"proxy/initialize","params":{"protocolVersion":1}}` + "\n"); err != nil {
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

// replayingProxy reports the initial frame it is handed, then answers it
// with the protocolVersion that frame carries.
func replayingProxy(seen chan<- string) acptest.ProxyFunc {
	return func(_ context.Context, in io.ReadCloser, out io.WriteCloser) error {
		defer in.Close()
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil {
			return err
		}
		seen <- strings.TrimSpace(line)
		var request struct {
			ID     json.RawMessage `json:"id"`
			Params struct {
				ProtocolVersion uint16 `json:"protocolVersion"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{
			"jsonrpc": "2.0", "id": request.ID,
			"result": map[string]any{"protocolVersion": request.Params.ProtocolVersion},
		})
	}
}

func exchange(t *testing.T, rw *bufio.ReadWriter, line string) string {
	t.Helper()
	if _, err := rw.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	response, err := rw.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(response, "\n")
}

// TestProxyRouterNegotiatesLikeAgentRouter checks that proxy/initialize
// selects the highest configured version at or below the requested one and
// normalizes a request for another version for the selected implementation.
func TestProxyRouterNegotiatesLikeAgentRouter(t *testing.T) {
	for _, test := range []struct {
		name      string
		v1, v2    bool
		params    string
		selected  uint16
		wantFrame string
	}{
		{
			name: "newer version selects v2", v1: true, v2: true,
			params:    `{"protocolVersion":3,"info":{"name":"conductor","version":"3"},"_future":true}`,
			selected:  2,
			wantFrame: `{"id":1,"jsonrpc":"2.0","method":"proxy/initialize","params":{"protocolVersion":2,"info":{"name":"conductor","version":"3"},"capabilities":{}}}`,
		},
		{
			name: "newer version without info selects v2", v2: true,
			params:    `{"protocolVersion":3}`,
			selected:  2,
			wantFrame: `{"id":1,"jsonrpc":"2.0","method":"proxy/initialize","params":{"protocolVersion":2,"info":{"name":"unknown","version":"unknown"},"capabilities":{}}}`,
		},
		{
			name: "v2 request selects configured v1", v1: true,
			params:    `{"protocolVersion":2,"info":{"name":"conductor","version":"2"}}`,
			selected:  1,
			wantFrame: `{"id":1,"jsonrpc":"2.0","method":"proxy/initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false,"session":{"configOptions":{"boolean":{}}},"auth":{"terminal":false}},"clientInfo":{"name":"conductor","version":"2"}}}`,
		},
		{
			name: "exact version is handed over unchanged", v1: true, v2: true,
			params:    `{"protocolVersion":1,"future":true}`,
			selected:  1,
			wantFrame: `{"jsonrpc":"2.0","id":1,"method":"proxy/initialize","params":{"protocolVersion":1,"future":true}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			seen := make(chan string, 1)
			router := New()
			if test.v1 {
				router.WithV1(replayingProxy(seen))
			}
			if test.v2 {
				router.WithV2(replayingProxy(seen))
			}
			rw := start(t, router)
			answer := exchange(t, rw, `{"jsonrpc":"2.0","id":1,"method":"proxy/initialize","params":`+test.params+`}`)
			if frame := <-seen; frame != test.wantFrame {
				t.Fatalf("replayed frame = %s, want %s", frame, test.wantFrame)
			}
			want := `{"id":1,"jsonrpc":"2.0","result":{"protocolVersion":` + string(rune('0'+test.selected)) + `}}`
			if answer != want {
				t.Fatalf("answer = %s, want %s", answer, want)
			}
		})
	}
}

// TestProxyRouterRejectionsKeepReading checks that a v2-only proxy router
// answers a request below v2 with -32022 and an invalid exact v2 request with
// -32602, without starting the proxy, and keeps reading.
func TestProxyRouterRejectionsKeepReading(t *testing.T) {
	seen := make(chan string, 1)
	rw := start(t, New().WithV2(replayingProxy(seen)))
	for _, frame := range []struct{ wire, answer string }{
		{
			`{"jsonrpc":"2.0","id":1,"method":"proxy/initialize","params":{"protocolVersion":1}}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":1,"supported":[2]}}}`,
		},
		{
			`{"jsonrpc":"2.0","id":2,"method":"proxy/initialize","params":{"protocolVersion":2}}`,
			`{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"Invalid params","data":"invalid initialize params: missing field ` + "`info`" + `"}}`,
		},
		{
			`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":2}}`,
			`{"jsonrpc":"2.0","id":3,"error":{"code":-32600,"message":"Invalid request","data":"first ACP request must be ` + "`proxy/initialize`" + `"}}`,
		},
	} {
		if got := exchange(t, rw, frame.wire); got != frame.answer {
			t.Fatalf("frame %s response = %s, want %s", frame.wire, got, frame.answer)
		}
	}
	if _, err := rw.WriteString(`{"jsonrpc":"2.0","method":"proxy/initialize","params":{"protocolVersion":2}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	answer := exchange(t, rw, `{"jsonrpc":"2.0","id":4,"method":"proxy/initialize","params":{"protocolVersion":2,"info":{"name":"c","version":"1"}}}`)
	if answer != `{"id":4,"jsonrpc":"2.0","result":{"protocolVersion":2}}` {
		t.Fatalf("answer = %s", answer)
	}
	if len(seen) != 1 {
		t.Fatalf("proxy started %d times, want 1", len(seen))
	}
}

// rustConductorInitialize is the frame a reference SDK 5.0.0 conductor sends
// a v1 proxy: the client's initialize renamed to proxy/initialize, with the
// params a Rust v1 client serializes and a string request id.
const rustConductorInitialize = `{"jsonrpc":"2.0","id":"988f7d5d-a63f-4796-99fb-8711424a02e4","method":"proxy/initialize","params":{"protocolVersion":1,"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false,"session":{"configOptions":{"boolean":{}}},"auth":{"terminal":false}},"clientInfo":{"name":"c","version":"1"}}}`

func TestProxyRouterAcceptsTheReferenceConductorsProxyInitialize(t *testing.T) {
	seen := make(chan string, 1)
	rw := start(t, New().WithV1(replayingProxy(seen)).WithV2(replayingProxy(seen)))
	answer := exchange(t, rw, rustConductorInitialize)
	if frame := <-seen; frame != rustConductorInitialize {
		t.Fatalf("replayed frame = %s, want it unchanged", frame)
	}
	if answer != `{"id":"988f7d5d-a63f-4796-99fb-8711424a02e4","jsonrpc":"2.0","result":{"protocolVersion":1}}` {
		t.Fatalf("answer = %s", answer)
	}
}

// TestProxyRouterRejectsThePreRFDMethodName checks that the router knows only
// the proxy-chains RFD's name: the old _proxy/initialize selects nothing on a
// v2-only router, is answered like any other wrong first request, and the
// conductor can still send proxy/initialize. The answer is the reference SDK
// 5.0.0 router's.
func TestProxyRouterRejectsThePreRFDMethodName(t *testing.T) {
	seen := make(chan string, 1)
	rw := start(t, New().WithV2(replayingProxy(seen)))
	want := `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"Invalid request","data":"first ACP request must be ` + "`proxy/initialize`" + `"}}`
	if got := exchange(t, rw, `{"jsonrpc":"2.0","id":1,"method":"_proxy/initialize","params":{"protocolVersion":2,"info":{"name":"c","version":"1"}}}`); got != want {
		t.Fatalf("answer = %s, want %s", got, want)
	}
	if len(seen) != 0 {
		t.Fatal("the proxy started for the pre-RFD method")
	}
	if got := exchange(t, rw, `{"jsonrpc":"2.0","id":2,"method":"proxy/initialize","params":{"protocolVersion":2,"info":{"name":"c","version":"1"}}}`); got != `{"id":2,"jsonrpc":"2.0","result":{"protocolVersion":2}}` {
		t.Fatalf("answer = %s", got)
	}
}

// TestProxyRouterWithoutImplementationsKeepsAnswering checks that a router
// with no configured implementation answers every proxy/initialize with
// -32022 listing no versions and keeps reading, as the reference SDK's router
// does.
func TestProxyRouterWithoutImplementationsKeepsAnswering(t *testing.T) {
	rw := start(t, New())
	for _, frame := range []struct{ wire, answer string }{
		{
			`{"jsonrpc":"2.0","id":1,"method":"proxy/initialize","params":{"protocolVersion":1}}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":1,"supported":[]}}}`,
		},
		{
			`{"jsonrpc":"2.0","id":2,"method":"proxy/initialize","params":{"protocolVersion":3,"info":{"name":"c","version":"1"}}}`,
			`{"jsonrpc":"2.0","id":2,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":3,"supported":[]}}}`,
		},
	} {
		if got := exchange(t, rw, frame.wire); got != frame.answer {
			t.Fatalf("frame %s response = %s, want %s", frame.wire, got, frame.answer)
		}
	}
}

func TestProxyRouterWithoutImplementationsStopsAtEndOfInput(t *testing.T) {
	if err := New().Serve(context.Background(), io.NopCloser(strings.NewReader("")), nopWriteCloser{}); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
