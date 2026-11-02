package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
)

func TestAppendDeclarationsPreservesParams(t *testing.T) {
	declaration := Declaration{Name: "tools", JSON: json.RawMessage(`{"type":"acp","name":"tools","serverId":"id-1"}`)}
	params := json.RawMessage(`{"cwd":"/w","futureField":{"n":1.50},"mcpServers":[{"type":"http","name":"web","url":"u","extra":true}]}`)
	out, err := AppendDeclarations(params, []Declaration{declaration}, true)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if string(got["futureField"]) != `{"n":1.50}` || string(got["mcpServers"]) != `[{"type":"http","name":"web","url":"u","extra":true},{"type":"acp","name":"tools","serverId":"id-1"}]` {
		t.Fatalf("params = %s", out)
	}
	for _, raw := range []string{`{"cwd":"/w"}`, `{"cwd":"/w","mcpServers":null}`} {
		out, err := AppendDeclarations(json.RawMessage(raw), []Declaration{declaration}, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(out, &got); err != nil || string(got["mcpServers"]) != `[{"type":"acp","name":"tools","serverId":"id-1"}]` {
			t.Fatalf("%s -> %s", raw, out)
		}
	}
	collision := json.RawMessage(`{"mcpServers":[{"type":"stdio","name":"tools","command":"/bin/x"}]}`)
	var rpcErr *acp.RPCError
	if _, err := AppendDeclarations(collision, []Declaration{declaration}, true); !errors.As(err, &rpcErr) || rpcErr.Code != -32602 {
		t.Fatalf("collision = %v", err)
	}
	if _, err := AppendDeclarations(collision, []Declaration{declaration}, false); err != nil {
		t.Fatalf("v1 append rejected a shared name: %v", err)
	}
	for _, raw := range []string{`[]`, `null`, `{"mcpServers":{}}`} {
		if _, err := AppendDeclarations(json.RawMessage(raw), []Declaration{declaration}, false); !errors.As(err, &rpcErr) || rpcErr.Code != -32602 {
			t.Fatalf("%s = %v", raw, err)
		}
	}
	if err := CheckDeclaredNames([]string{"web"}, []Declaration{declaration, declaration}); err == nil {
		t.Fatal("two attached servers shared a name")
	}
}

func TestServeStandaloneAnswersADirectMCPClient(t *testing.T) {
	pair := acptest.NewPairWithDeadline(time.Now().Add(10 * time.Second))
	defer pair.Close()
	started := make(chan struct{})
	registry := NewToolRegistry()
	_ = registry.Register(greetTool())
	_ = registry.Register(Tool{Name: "progress", Handler: func(ctx context.Context, _ json.RawMessage, rc *RequestContext) (any, error) {
		if !rc.Standalone() || rc.ServerID() != "" {
			return nil, errors.New("not standalone")
		}
		if err := rc.Notify(ctx, "notifications/progress", map[string]any{"progress": 1}); err != nil {
			return nil, err
		}
		return "done", nil
	}})
	_ = registry.Register(Tool{Name: "hang", Handler: func(ctx context.Context, _ json.RawMessage, _ *RequestContext) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	server := NewToolServer("direct", "9", registry)
	served := make(chan error, 1)
	go func() { served <- server.Serve(context.Background(), pair.A, pair.A) }()

	notifications := make(chan string, 4)
	client := pair.B.Connect(nil, func(method string, _ json.RawMessage) error {
		notifications <- method
		return nil
	})
	defer client.Close()
	ctx := context.Background()

	var initialized map[string]any
	if err := client.Call(ctx, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}}, &initialized); err != nil {
		t.Fatal(err)
	}
	if initialized["serverInfo"].(map[string]any)["name"] != "direct" {
		t.Fatalf("initialize = %v", initialized)
	}
	var called map[string]any
	if err := client.Call(ctx, "tools/call", map[string]any{"name": "greet", "arguments": map[string]any{"name": "Ann"}}, &called); err != nil {
		t.Fatal(err)
	}
	if _, modern := called["resultType"]; modern || called["content"].([]any)[0].(map[string]any)["text"] != "Hello, Ann!" {
		t.Fatalf("legacy call = %v", called)
	}
	if err := client.Call(ctx, "tools/call", json.RawMessage(modern(map[string]any{"name": "progress"})), &called); err != nil {
		t.Fatal(err)
	}
	if method := <-notifications; method != "notifications/progress" {
		t.Fatalf("notification = %s", method)
	}
	err := client.Call(ctx, "tools/call", map[string]any{"name": "missing"}, nil)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 {
		t.Fatalf("unknown tool = %v", err)
	}

	// Closing the client cancels in-flight work and ends Serve cleanly.
	go func() { _ = client.Call(context.Background(), "tools/call", map[string]any{"name": "hang"}, nil) }()
	<-started
	_ = pair.B.Close()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after EOF")
	}
}
