package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
)

func TestRuntimeRefusesReservedAndNilOverrides(t *testing.T) {
	serve := func(context.Context, string, json.RawMessage) (any, error) { return nil, nil }
	for method, handler := range map[string]acp.Handler{
		schema.InitializeMethodName:    serve,
		schema.SessionPromptMethodName: serve,
		schema.SessionCancelMethodName: serve,
		schema.CancelRequestMethodName: serve,
		schema.SessionListMethodName:   nil,
	} {
		runtime := New(testAgent{})
		runtime.Overrides = map[string]acp.Handler{method: handler}
		agentSide, clientSide := net.Pipe()
		err := runtime.Serve(context.Background(), agentSide, agentSide)
		_ = clientSide.Close()
		if err == nil || !strings.Contains(err.Error(), method) {
			t.Fatalf("Serve with an override for %s = %v", method, err)
		}
	}
}

func TestRuntimeOverridesRunBehindTheGates(t *testing.T) {
	ctx := context.Background()
	calls := make(chan string, 4)
	override := func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		calls <- method
		return map[string]json.RawMessage{"params": raw}, nil
	}
	start := func(advertiseList bool) *acp.Connection {
		runtime := New(testAgent{})
		runtime.Overrides = map[string]acp.Handler{
			schema.SessionNewMethodName:  override,
			schema.SessionListMethodName: override,
			"_vendor/extension":          override,
		}
		runtime.Extensions = func(context.Context, string, json.RawMessage) (any, error) {
			return nil, errors.New("extensions must not see an overridden method")
		}
		if advertiseList {
			runtime.InitializeResult = func(response schema.InitializeResponse) (any, error) {
				response.AgentCapabilities = &schema.AgentCapabilities{SessionCapabilities: &schema.SessionCapabilities{List: &schema.SessionListCapabilities{}}}
				return response, nil
			}
		}
		return startExtendedRuntime(t, runtime)
	}

	connection := start(false)
	err := connection.Call(ctx, schema.SessionListMethodName, map[string]any{}, nil)
	requireInvalidRequest(t, err, "ACP initialization must complete before `session/list` can be used")
	initializeClient(t, connection, acp.Capabilities{})
	var rpcErr *acp.RPCError
	if err := connection.Call(ctx, schema.SessionListMethodName, map[string]any{}, nil); !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("overridden session/list without sessionCapabilities.list = %v", err)
	}
	select {
	case method := <-calls:
		t.Fatalf("override served %s before its gates passed", method)
	default:
	}
	var result struct {
		Params json.RawMessage `json:"params"`
	}
	if err := connection.Call(ctx, schema.SessionNewMethodName, map[string]any{"cwd": "/", "mcpServers": []any{}, "extra": 1}, &result); err != nil || string(result.Params) != `{"cwd":"/","extra":1,"mcpServers":[]}` {
		t.Fatalf("overridden session/new = %s, %v", result.Params, err)
	}
	if err := connection.Call(ctx, "_vendor/extension", map[string]any{}, nil); err != nil {
		t.Fatalf("overridden extension method: %v", err)
	}

	// With the capability advertised, the override serves session/list
	// although the Agent implements no SessionLister.
	connection = start(true)
	initializeClient(t, connection, acp.Capabilities{})
	if err := connection.Call(ctx, schema.SessionListMethodName, map[string]any{"archived": "only"}, &result); err != nil || string(result.Params) != `{"archived":"only"}` {
		t.Fatalf("overridden session/list = %s, %v", result.Params, err)
	}
	for _, want := range []string{schema.SessionNewMethodName, "_vendor/extension", schema.SessionListMethodName} {
		if method := <-calls; method != want {
			t.Fatalf("override served %s, want %s", method, want)
		}
	}
}
