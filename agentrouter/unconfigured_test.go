package agentrouter

import (
	"context"
	"io"
	"strings"
	"testing"
)

// TestRouterWithoutImplementationsKeepsAnswering checks that a router with no
// configured implementation answers like the reference SDK 5.0.0 router:
// every initialize gets -32022 listing no versions, any other request gets
// the usual first-request error, notifications are ignored, and it keeps
// reading.
func TestRouterWithoutImplementationsKeepsAnswering(t *testing.T) {
	rw := startRouter(t, New())
	for _, frame := range []struct{ wire, answer string }{
		{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":1,"supported":[]}}}`,
		},
		{
			`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":3,"info":{"name":"c","version":"1"}}}`,
			`{"jsonrpc":"2.0","id":2,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":3,"supported":[]}}}`,
		},
		{
			`{"jsonrpc":"2.0","method":"x/note"}` + "\n" + `{"jsonrpc":"2.0","id":3,"method":"session/new","params":{}}`,
			`{"jsonrpc":"2.0","id":3,"error":{"code":-32600,"message":"Invalid request","data":"first ACP request must be ` + "`initialize`" + `"}}`,
		},
	} {
		if got := exchange(t, rw, frame.wire); got != frame.answer {
			t.Fatalf("frame %s response = %s, want %s", frame.wire, got, frame.answer)
		}
	}
}

func TestRouterWithoutImplementationsStopsAtEndOfInput(t *testing.T) {
	if err := New().Serve(context.Background(), io.NopCloser(strings.NewReader("")), nopWriteCloser{}); err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
