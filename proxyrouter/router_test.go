package proxyrouter

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"
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
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() { done <- router.Serve(context.Background(), local, local) }()
	t.Cleanup(func() {
		_ = peer.Close()
		_ = local.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("proxy router did not stop")
		}
	})
	return bufio.NewReadWriter(bufio.NewReader(peer), bufio.NewWriter(peer))
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

func TestProxyRouterPreservesInitialBatchAndFutureFields(t *testing.T) {
	started := make(chan string, 1)
	seen := make(chan map[string]json.RawMessage, 1)
	proxy := proxyFunc(func(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
		defer in.Close()
		var batch []map[string]json.RawMessage
		if err := json.NewDecoder(in).Decode(&batch); err != nil {
			return err
		}
		seen <- batch[0]
		started <- "done"
		return nil
	})
	rw := start(t, New().WithV2(proxy))
	batch, err := json.Marshal([]map[string]any{
		{
			"jsonrpc": "2.0", "id": 1, "method": "_proxy/initialize",
			"params": map[string]any{
				"protocolVersion": 2,
				"info":            map[string]any{"name": "client", "version": "1"},
				"future":          true,
			},
		},
		{"jsonrpc": "2.0", "method": "future/notification", "params": map[string]bool{"preserved": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Write(append(batch, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("proxy did not receive batch")
	}
	params := <-seen
	var initializeParams map[string]json.RawMessage
	if err := json.Unmarshal(params["params"], &initializeParams); err != nil {
		t.Fatal(err)
	}
	if _, exists := initializeParams["future"]; !exists {
		t.Fatalf("future initialize field was dropped: %s", initializeParams)
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
	rw := start(t, New().WithV2(proxyFunc(func(context.Context, io.ReadCloser, io.WriteCloser) error {
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

type proxyFunc func(context.Context, io.ReadCloser, io.WriteCloser) error

func (f proxyFunc) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	return f(ctx, in, out)
}
