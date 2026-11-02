package unstable_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	"github.com/BrokkAi/acp-go/agent"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
	acpunstable "github.com/BrokkAi/acp-go/unstable"
)

// unstableAgent is the shared v1 fake agent extended with typed unstable
// handlers.
type unstableAgent struct {
	*acptest.V1Agent
	mu       sync.Mutex
	set      []schema.SetProviderRequest
	forkedID schema.SessionId
}

func newUnstableAgent() *unstableAgent {
	return &unstableAgent{V1Agent: &acptest.V1Agent{}, forkedID: "dispatched"}
}

func (a *unstableAgent) ListProviders(context.Context, schema.ListProvidersRequest) (schema.ListProvidersResponse, error) {
	return schema.ListProvidersResponse{Providers: []schema.ProviderInfo{{ProviderID: "dispatched"}}}, nil
}

func (a *unstableAgent) SetProvider(_ context.Context, request schema.SetProviderRequest) (schema.SetProviderResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.set = append(a.set, request)
	return schema.SetProviderResponse{}, nil
}

func (a *unstableAgent) DisableProvider(context.Context, schema.DisableProviderRequest) (schema.DisableProviderResponse, error) {
	return schema.DisableProviderResponse{}, nil
}

func (a *unstableAgent) ForkSession(context.Context, schema.ForkSessionRequest) (schema.ForkSessionResponse, error) {
	return schema.ForkSessionResponse{SessionID: a.forkedID}, nil
}

type providersOnlyAgent struct {
	*acptest.V1Agent
	acpunstable.ProviderHandler
}

// serveRuntime runs the v1 agent runtime with the unstable dispatch installed
// and returns a client connection to it.
func serveRuntime(t *testing.T, implementation agent.Agent, next acp.Handler) *acp.Connection {
	t.Helper()
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	runtime := agent.New(implementation)
	runtime.Extensions = acpunstable.Handle(next, implementation)
	runtime.InitializeResult = acpunstable.Advertise(implementation)
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), pair.A, pair.A) }()
	client := acp.Connect(pair.B, pair.B, nil, nil)
	t.Cleanup(func() {
		_ = client.Close()
		pair.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	return client
}

func initializeUnstable(t *testing.T, connection *acp.Connection) schema.InitializeResponse {
	t.Helper()
	var initialization schema.InitializeResponse
	if err := connection.Call(context.Background(), schema.InitializeMethodName, schema.InitializeRequest{ProtocolVersion: schema.ProtocolVersion(acp.Version)}, &initialization); err != nil {
		t.Fatal(err)
	}
	return initialization
}

func rpcError(t *testing.T, err error) *acp.RPCError {
	t.Helper()
	var rpc *acp.RPCError
	if !errors.As(err, &rpc) {
		t.Fatalf("error = %v, want an RPC error", err)
	}
	return rpc
}

func TestRuntimeServesUnstableMethodsAfterInitialize(t *testing.T) {
	ctx := context.Background()
	implementation := newUnstableAgent()
	connection := serveRuntime(t, implementation, nil)

	rpc := rpcError(t, connection.Call(ctx, schema.ProvidersListMethodName, schema.ListProvidersRequest{}, nil))
	var detail string
	_ = json.Unmarshal(rpc.Data, &detail)
	if rpc.Code != -32600 || rpc.Message != "Invalid request" || detail != "ACP initialization must complete before `providers/list` can be used" {
		t.Fatalf("providers/list before initialize = %+v (data %s)", rpc, rpc.Data)
	}

	initialization := initializeUnstable(t, connection)
	capabilities := initialization.AgentCapabilities
	if capabilities == nil || capabilities.Providers == nil || capabilities.SessionCapabilities == nil || capabilities.SessionCapabilities.Fork == nil {
		t.Fatalf("advertised capabilities = %+v", capabilities)
	}
	providers, err := acpunstable.ListProviders(ctx, connection, initialization)
	if err != nil || len(providers.Providers) != 1 || providers.Providers[0].ProviderID != "dispatched" {
		t.Fatalf("providers/list = %+v, %v", providers, err)
	}
	if _, err := acpunstable.SetProvider(ctx, connection, initialization, schema.SetProviderRequest{ProviderID: "alpha", APIType: "openai", BaseURL: "https://example.test"}); err != nil {
		t.Fatal(err)
	}
	fork, err := acpunstable.ForkSession(ctx, connection, initialization, schema.ForkSessionRequest{Cwd: "/", SessionID: "old"})
	if err != nil || fork.SessionID != "dispatched" {
		t.Fatalf("session/fork = %+v, %v", fork, err)
	}
	implementation.mu.Lock()
	defer implementation.mu.Unlock()
	if len(implementation.set) != 1 || implementation.set[0].ProviderID != "alpha" {
		t.Fatalf("providers/set reached the agent as %+v", implementation.set)
	}
}

func TestHandleFallsThroughToNext(t *testing.T) {
	ctx := context.Background()
	next := func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		return map[string]string{"next": method}, nil
	}
	connection := serveRuntime(t, providersOnlyAgent{&acptest.V1Agent{}, newUnstableAgent()}, next)
	initialization := initializeUnstable(t, connection)
	if initialization.AgentCapabilities.SessionCapabilities != nil && initialization.AgentCapabilities.SessionCapabilities.Fork != nil {
		t.Fatal("session/fork advertised without a ForkHandler")
	}
	for _, method := range []string{schema.SessionForkMethodName, "_vendor/extension"} {
		var result map[string]string
		if err := connection.Call(ctx, method, map[string]any{"sessionId": "old", "cwd": "/"}, &result); err != nil || result["next"] != method {
			t.Fatalf("%s fallthrough = %v, %v", method, result, err)
		}
	}

	connection = serveRuntime(t, providersOnlyAgent{&acptest.V1Agent{}, newUnstableAgent()}, nil)
	initializeUnstable(t, connection)
	rpc := rpcError(t, connection.Call(ctx, schema.SessionForkMethodName, map[string]any{"sessionId": "old", "cwd": "/"}, nil))
	if rpc.Code != -32601 || rpc.Message != "Method not found" || string(rpc.Data) != `"session/fork"` {
		t.Fatalf("missing fork handler without next = %+v (data %s)", rpc, rpc.Data)
	}
}

func TestHandleErrorShapes(t *testing.T) {
	ctx := context.Background()
	implementation := newUnstableAgent()
	handler := acpunstable.Handle(nil, implementation)
	rpc := rpcError(t, func() error {
		_, err := handler(ctx, schema.ProvidersSetMethodName, json.RawMessage(`{"providerId":7}`))
		return err
	}())
	if rpc.Code != -32602 || rpc.Message != "Invalid params" || !strings.Contains(string(rpc.Data), `"json":{"providerId":7}`) || !strings.Contains(string(rpc.Data), `"phase":"deserialization"`) {
		t.Fatalf("malformed params error = %+v (data %s)", rpc, rpc.Data)
	}
	if len(implementation.set) != 0 {
		t.Fatal("malformed params reached the handler")
	}
	implementation.forkedID = " "
	rpc = rpcError(t, func() error {
		_, err := handler(ctx, schema.SessionForkMethodName, json.RawMessage(`{"sessionId":"s","cwd":"/"}`))
		return err
	}())
	if rpc.Code != -32603 || rpc.Message != "Internal error" {
		t.Fatalf("blank forked session error = %+v", rpc)
	}
}
