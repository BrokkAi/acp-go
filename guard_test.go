package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/BrokkAi/acp-go/internal/rpcctl"
)

// TestRequestGuardAnswersRefusalsInWireOrder: a refused request is answered
// before the next frame is handled, and is neither observed nor dispatched;
// notifications and null-id v2 requests bypass the guard.
func TestRequestGuardAnswersRefusalsInWireOrder(t *testing.T) {
	var mu sync.Mutex
	var guarded, observed, handled, notified []string
	record := func(list *[]string, method string) {
		mu.Lock()
		defer mu.Unlock()
		*list = append(*list, method)
	}
	guard := rpcctl.WithRequestGuard(func(method string, _ json.RawMessage) error {
		record(&guarded, method)
		switch method {
		case "refuse":
			return &RPCError{Code: -32600, Message: "Invalid request", Data: json.RawMessage(`"no"`)}
		case "plain":
			return errors.New("plain failure")
		case "explode":
			panic("guard panic")
		}
		return nil
	}).(ConnectOption)
	release := make(chan struct{})
	_, peer := pipeClient(t, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		record(&handled, method)
		<-release
		return "ok", nil
	}, func(method string, _ json.RawMessage) error {
		record(&notified, method)
		return nil
	}, guard, WithProtocolVersion(2), WithErrorHandler(func(error) {}), WithRequestObserver(func(method string, _ json.RawMessage) {
		record(&observed, method)
	}))

	frames := `{"jsonrpc":"2.0","id":1,"method":"admit"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"refuse"}` + "\n" +
		`{"jsonrpc":"2.0","method":"note"}` + "\n" +
		`{"jsonrpc":"2.0","id":null,"method":"admit"}` + "\n" +
		`{"jsonrpc":"2.0","id":"p","method":"plain"}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"explode"}` + "\n"
	go func() { _, _ = peer.Write([]byte(frames)) }()
	reader := bufio.NewReader(peer)
	for _, want := range []string{
		`{"jsonrpc":"2.0","id":2,"error":{"code":-32600,"message":"Invalid request","data":"no"}}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"ACP protocol v2 request ids must be a string or an integer, not null"}}`,
		`{"jsonrpc":"2.0","id":"p","error":{"code":-32603,"message":"plain failure"}}`,
		"\"id\":3,\"error\":{\"code\":-32603,\"message\":\"Internal error\"",
	} {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(line, want) {
			t.Fatalf("answer = %s, want %s", line, want)
		}
	}
	close(release)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, `"id":1,"result":"ok"`) {
		t.Fatalf("admitted request answer = %s, %v", line, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(guarded, ",") != "admit,refuse,plain,explode" ||
		strings.Join(observed, ",") != "admit" ||
		strings.Join(handled, ",") != "admit" ||
		strings.Join(notified, ",") != "note" {
		t.Fatalf("guarded %v, observed %v, handled %v, notified %v", guarded, observed, handled, notified)
	}
}
