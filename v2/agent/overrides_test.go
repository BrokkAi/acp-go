package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
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
		runtime := New(&echoAgent{})
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
	start := func(advertiseDelete bool) *acpv2.Connection {
		runtime := New(&echoAgent{})
		runtime.Overrides = map[string]acp.Handler{
			schema.SessionListMethodName:   override,
			schema.SessionDeleteMethodName: override,
		}
		if advertiseDelete {
			// echoAgent implements no SessionDeleter; the override stands in.
			runtime.InitializeResult = func(response schema.InitializeResponse) (any, error) {
				response.Capabilities.Session.Delete = &schema.SessionDeleteCapabilities{}
				return response, nil
			}
		}
		return startExtendedRuntime(t, runtime)
	}

	client := start(false)
	err := client.Call(ctx, schema.SessionListMethodName, map[string]any{}, nil)
	requireInvalidRequest(t, err, "ACP initialization must complete before `session/list` can be used")
	if _, err := client.Initialize(ctx, acpv2.Capabilities{}); err != nil {
		t.Fatal(err)
	}
	var rpcErr *acp.RPCError
	if err := client.Call(ctx, schema.SessionDeleteMethodName, map[string]any{"sessionId": "s"}, nil); !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("overridden session/delete without session.delete = %v", err)
	}
	select {
	case method := <-calls:
		t.Fatalf("override served %s before its gates passed", method)
	default:
	}
	var result struct {
		Params json.RawMessage `json:"params"`
	}
	if err := client.Call(ctx, schema.SessionListMethodName, map[string]any{"archived": "only"}, &result); err != nil || string(result.Params) != `{"archived":"only"}` {
		t.Fatalf("overridden session/list = %s, %v", result.Params, err)
	}

	client = start(true)
	if _, err := client.Initialize(ctx, acpv2.Capabilities{}); err != nil {
		t.Fatalf("initialize with session.delete served by an override: %v", err)
	}
	if err := client.Call(ctx, schema.SessionDeleteMethodName, map[string]any{"sessionId": "s"}, &result); err != nil || string(result.Params) != `{"sessionId":"s"}` {
		t.Fatalf("overridden session/delete = %s, %v", result.Params, err)
	}
	for _, want := range []string{schema.SessionListMethodName, schema.SessionDeleteMethodName} {
		if method := <-calls; method != want {
			t.Fatalf("override served %s, want %s", method, want)
		}
	}
}
