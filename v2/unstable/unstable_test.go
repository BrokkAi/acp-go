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
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	"github.com/BrokkAi/acp-go/v2/agent"
	acpunstable "github.com/BrokkAi/acp-go/v2/unstable"
)

// fakeAgent is the shared v2 fake agent extended with typed unstable
// handlers: it keeps provider configuration in memory and records forks.
type fakeAgent struct {
	*acptest.V2Agent
	mu        sync.Mutex
	providers map[schema.ProviderId]schema.ProviderCurrentConfig
	forks     []schema.ForkSessionRequest
	forkedID  schema.SessionId
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{
		V2Agent:   &acptest.V2Agent{},
		providers: map[schema.ProviderId]schema.ProviderCurrentConfig{},
		forkedID:  "forked",
	}
}

func (a *fakeAgent) ListProviders(context.Context, schema.ListProvidersRequest) (schema.ListProvidersResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	response := schema.ListProvidersResponse{Providers: []schema.ProviderInfo{}}
	for _, id := range []schema.ProviderId{"main", "fast"} {
		info := schema.ProviderInfo{ProviderID: id, Supported: []schema.LlmProtocol{"anthropic", "openai"}, Required: id == "main"}
		if current, ok := a.providers[id]; ok {
			info.Current = &current
		}
		response.Providers = append(response.Providers, info)
	}
	return response, nil
}

func (a *fakeAgent) SetProvider(_ context.Context, request schema.SetProviderRequest) (schema.SetProviderResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.providers[request.ProviderID] = schema.ProviderCurrentConfig{APIType: request.APIType, BaseURL: request.BaseURL}
	return schema.SetProviderResponse{}, nil
}

func (a *fakeAgent) DisableProvider(_ context.Context, request schema.DisableProviderRequest) (schema.DisableProviderResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.providers, request.ProviderID)
	return schema.DisableProviderResponse{}, nil
}

func (a *fakeAgent) ForkSession(_ context.Context, request schema.ForkSessionRequest) (schema.ForkSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forks = append(a.forks, request)
	return schema.ForkSessionResponse{SessionID: a.forkedID}, nil
}

// providersOnly implements the baseline and providers, but not session/fork.
type providersOnly struct {
	*acptest.V2Agent
	acpunstable.ProviderHandler
}

func withAdditionalDirectories(response *schema.InitializeResponse) error {
	response.Capabilities.Session.AdditionalDirectories = &schema.SessionAdditionalDirectoriesCapabilities{}
	return nil
}

// serve runs the v2 agent runtime with the unstable dispatch installed, as
// the package documentation describes, and returns a client connection to
// it plus each request that reached the dispatch, in order.
func serve(t *testing.T, implementation agent.Agent, next acp.Handler) (*acpv2.Connection, chan string) {
	t.Helper()
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	dispatched := make(chan string, 16)
	handle := acpunstable.Handle(next, implementation)
	runtime := agent.New(implementation)
	runtime.Extensions = func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		dispatched <- method
		return handle(ctx, method, raw)
	}
	runtime.InitializeResult = acpunstable.Advertise(implementation, withAdditionalDirectories)
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), pair.B, pair.B) }()
	client := acpv2.Connect(pair.A, pair.A, nil, nil)
	t.Cleanup(func() {
		_ = client.Close()
		pair.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	return client, dispatched
}

// initialize runs the validated v2 initialize and returns the unstable view.
func initialize(t *testing.T, connection *acpv2.Connection) schema.InitializeResponse {
	t.Helper()
	var initialization schema.InitializeResponse
	if _, err := connection.InitializeExtended(context.Background(), acpv2.Capabilities{}, acpv2.ClientInfo{Name: "client", Version: "1"}, &initialization); err != nil {
		t.Fatal(err)
	}
	return initialization
}

func expectNothingDispatched(t *testing.T, dispatched chan string) {
	t.Helper()
	select {
	case method := <-dispatched:
		t.Fatalf("validation wrote %s to the wire", method)
	default:
	}
}

func providersInitialization() schema.InitializeResponse {
	return schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{Providers: &schema.ProvidersCapabilities{}}}
}

func forkInitialization() schema.InitializeResponse {
	return schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{Session: &schema.SessionCapabilities{
		Fork:                  &schema.SessionForkCapabilities{},
		AdditionalDirectories: &schema.SessionAdditionalDirectoriesCapabilities{},
	}}}
}

func rpcError(t *testing.T, err error) *acp.RPCError {
	t.Helper()
	var rpc *acp.RPCError
	if !errors.As(err, &rpc) {
		t.Fatalf("error = %v, want an RPC error", err)
	}
	return rpc
}

func TestAdvertiseAnnouncesImplementedCapabilities(t *testing.T) {
	connection, _ := serve(t, newFakeAgent(), nil)
	initialization := initialize(t, connection)
	capabilities := initialization.Capabilities
	if capabilities == nil || capabilities.Providers == nil || capabilities.Session == nil || capabilities.Session.Fork == nil || capabilities.Session.AdditionalDirectories == nil {
		t.Fatalf("capabilities = %+v", capabilities)
	}
	if initialization.Info.Name != "v2-router-agent" || initialization.ProtocolVersion != schema.ProtocolVersion(acpv2.Version) {
		t.Fatalf("stable fields were not carried over: %+v", initialization)
	}

	connection, _ = serve(t, providersOnly{&acptest.V2Agent{}, newFakeAgent()}, nil)
	capabilities = initialize(t, connection).Capabilities
	if capabilities.Providers == nil || capabilities.Session.Fork != nil {
		t.Fatalf("providers-only agent advertised %+v / fork %+v", capabilities, capabilities.Session.Fork)
	}
}

func TestUnstableMethodsAreRefusedBeforeInitialize(t *testing.T) {
	connection, dispatched := serve(t, newFakeAgent(), nil)
	for _, method := range []string{schema.ProvidersListMethodName, schema.SessionForkMethodName} {
		err := connection.Call(context.Background(), method, map[string]any{}, nil)
		rpc := rpcError(t, err)
		var detail string
		_ = json.Unmarshal(rpc.Data, &detail)
		if rpc.Code != -32600 || rpc.Message != "Invalid request" || detail != "ACP initialization must complete before `"+method+"` can be used" {
			t.Fatalf("%s before initialize = %+v (data %s)", method, rpc, rpc.Data)
		}
	}
	expectNothingDispatched(t, dispatched)
}

func TestProviderFacadesFailBeforeWriting(t *testing.T) {
	ctx := context.Background()
	connection, dispatched := serve(t, newFakeAgent(), nil)
	initialize(t, connection)
	withoutProviders := schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}}}
	for _, initialization := range []schema.InitializeResponse{{}, withoutProviders} {
		if _, err := acpunstable.ListProviders(ctx, connection, initialization); err == nil || !strings.Contains(err.Error(), "provider configuration") {
			t.Fatalf("providers/list without the capability error = %v", err)
		}
		if _, err := acpunstable.SetProvider(ctx, connection, initialization, schema.SetProviderRequest{ProviderID: "main"}); err == nil {
			t.Fatal("providers/set without the capability was allowed")
		}
		if _, err := acpunstable.DisableProvider(ctx, connection, initialization, "main"); err == nil {
			t.Fatal("providers/disable without the capability was allowed")
		}
	}
	initialization := providersInitialization()
	if _, err := acpunstable.SetProvider(ctx, connection, initialization, schema.SetProviderRequest{ProviderID: " "}); err == nil || !strings.Contains(err.Error(), "provider ID is required") {
		t.Fatalf("blank provider ID error = %v", err)
	}
	if _, err := acpunstable.DisableProvider(ctx, connection, initialization, ""); err == nil || !strings.Contains(err.Error(), "provider ID is required") {
		t.Fatalf("empty provider ID error = %v", err)
	}
	expectNothingDispatched(t, dispatched)
}

func TestProviderFacadesRoundTripThroughTheRuntime(t *testing.T) {
	ctx := context.Background()
	connection, dispatched := serve(t, newFakeAgent(), nil)
	initialization := initialize(t, connection)

	if _, err := connection.NewSession(ctx, "/repo"); err != nil {
		t.Fatalf("baseline session/new: %v", err)
	}
	if _, err := acpunstable.SetProvider(ctx, connection, initialization, schema.SetProviderRequest{
		ProviderID: "main", APIType: "openai", BaseURL: "https://llm.example.test",
		Headers: map[string]string{"authorization": "Bearer token"},
	}); err != nil {
		t.Fatal(err)
	}
	providers, err := acpunstable.ListProviders(ctx, connection, initialization)
	if err != nil {
		t.Fatal(err)
	}
	if len(providers.Providers) != 2 || providers.Providers[0].Current == nil || providers.Providers[0].Current.BaseURL != "https://llm.example.test" || providers.Providers[1].Current != nil {
		t.Fatalf("providers after set = %+v", providers.Providers)
	}
	if _, err := acpunstable.DisableProvider(ctx, connection, initialization, "main"); err != nil {
		t.Fatal(err)
	}
	providers, err = acpunstable.ListProviders(ctx, connection, initialization)
	if err != nil {
		t.Fatal(err)
	}
	if providers.Providers[0].Current != nil {
		t.Fatalf("provider still configured after disable: %+v", providers.Providers[0])
	}
	for _, want := range []string{schema.ProvidersSetMethodName, schema.ProvidersListMethodName, schema.ProvidersDisableMethodName, schema.ProvidersListMethodName} {
		if method := <-dispatched; method != want {
			t.Fatalf("dispatched %s, want %s (session/new must stay with the runtime)", method, want)
		}
	}
}

func TestForkSessionFailsBeforeWriting(t *testing.T) {
	ctx := context.Background()
	connection, dispatched := serve(t, newFakeAgent(), nil)
	initialize(t, connection)
	valid := schema.ForkSessionRequest{SessionID: "source", Cwd: "/repo"}
	withoutAdditional := forkInitialization()
	withoutAdditional.Capabilities.Session.AdditionalDirectories = nil
	for name, test := range map[string]struct {
		initialization schema.InitializeResponse
		request        schema.ForkSessionRequest
		want           string
	}{
		"no capabilities":    {schema.InitializeResponse{}, valid, "session/fork"},
		"no fork capability": {schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}}}, valid, "session/fork"},
		"blank session ID":   {forkInitialization(), schema.ForkSessionRequest{SessionID: " ", Cwd: "/repo"}, "session ID is required"},
		"relative cwd":       {forkInitialization(), schema.ForkSessionRequest{SessionID: "source", Cwd: "repo"}, "must be absolute"},
		"relative additional directory": {forkInitialization(), schema.ForkSessionRequest{
			SessionID: "source", Cwd: "/repo", AdditionalDirectories: []schema.AbsolutePath{"/extra", "extra"},
		}, "must be absolute"},
		"additional directories without capability": {withoutAdditional, schema.ForkSessionRequest{
			SessionID: "source", Cwd: "/repo", AdditionalDirectories: []schema.AbsolutePath{"/extra"},
		}, "additionalDirectories"},
		"inline MCP servers": {forkInitialization(), schema.ForkSessionRequest{
			SessionID: "source", Cwd: "/repo",
			MCPServers: []schema.McpServer{{HTTP: &schema.McpServerHttp{Name: "docs", URL: "https://mcp.example.test"}}},
		}, "v2/mcp"},
	} {
		if _, err := acpunstable.ForkSession(ctx, connection, test.initialization, test.request); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: error = %v, want it to mention %q", name, err, test.want)
		}
	}
	expectNothingDispatched(t, dispatched)
}

func TestForkSessionRoundTripThroughTheRuntime(t *testing.T) {
	ctx := context.Background()
	implementation := newFakeAgent()
	connection, dispatched := serve(t, implementation, nil)
	initialization := initialize(t, connection)

	result, err := acpunstable.ForkSession(ctx, connection, initialization, schema.ForkSessionRequest{
		SessionID:             "source",
		Cwd:                   `C:\agent\workspace`,
		AdditionalDirectories: []schema.AbsolutePath{"/", `C:\agent\extra`},
		SystemPrompt: &schema.SystemPrompt{
			Mode:    schema.SystemPromptModeAppend,
			Content: []schema.ContentBlock{{Text: &schema.TextContent{Text: "Summarize only."}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "forked" {
		t.Fatalf("forked session = %q", result.SessionID)
	}
	if method := <-dispatched; method != schema.SessionForkMethodName {
		t.Fatalf("dispatched %s", method)
	}
	implementation.mu.Lock()
	defer implementation.mu.Unlock()
	if len(implementation.forks) != 1 {
		t.Fatalf("agent received %+v", implementation.forks)
	}
	fork := implementation.forks[0]
	if fork.SessionID != "source" || len(fork.AdditionalDirectories) != 2 || fork.MCPServers != nil {
		t.Fatalf("agent received %+v", fork)
	}
	if fork.SystemPrompt == nil || fork.SystemPrompt.Mode != schema.SystemPromptModeAppend || len(fork.SystemPrompt.Content) != 1 || fork.SystemPrompt.Content[0].Text.Text != "Summarize only." {
		t.Fatalf("systemPrompt was not passed through unchanged: %+v", fork.SystemPrompt)
	}
}

func TestForkSessionRejectsBlankForkedSessionID(t *testing.T) {
	// A raw agent, not Handle, so the blank ID reaches the client.
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	server := acp.Connect(pair.B, pair.B, func(context.Context, string, json.RawMessage) (any, error) {
		return schema.ForkSessionResponse{SessionID: " "}, nil
	}, nil)
	connection := acpv2.Connect(pair.A, pair.A, nil, nil)
	t.Cleanup(func() {
		_ = connection.Close()
		_ = server.Close()
		pair.Close()
	})
	_, err := acpunstable.ForkSession(context.Background(), connection, forkInitialization(), schema.ForkSessionRequest{SessionID: "source", Cwd: "/repo"})
	if err == nil || !strings.Contains(err.Error(), "forked session ID is required") {
		t.Fatalf("blank forked session ID error = %v", err)
	}
}

func TestHandleFallsThroughInTheRuntime(t *testing.T) {
	ctx := context.Background()
	next := func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == schema.SessionForkMethodName {
			return schema.ForkSessionResponse{SessionID: "from-next"}, nil
		}
		return map[string]string{"next": method}, nil
	}
	connection, _ := serve(t, providersOnly{&acptest.V2Agent{}, newFakeAgent()}, next)
	initialize(t, connection)

	// session/fork is an unstable method this handler does not implement, so
	// it falls through to next, as in the reference SDK's handler chain.
	var fork schema.ForkSessionResponse
	if err := connection.Call(ctx, schema.SessionForkMethodName, schema.ForkSessionRequest{SessionID: "source", Cwd: "/repo"}, &fork); err != nil || fork.SessionID != "from-next" {
		t.Fatalf("session/fork fallthrough = %+v, %v", fork, err)
	}
	// nes/* left the upstream schema, so it is just another foreign method.
	for _, method := range []string{"nes/start", "_vendor/extension"} {
		var result map[string]string
		if err := connection.Call(ctx, method, map[string]any{}, &result); err != nil || result["next"] != method {
			t.Fatalf("%s fallthrough = %v, %v", method, result, err)
		}
	}
	var providers schema.ListProvidersResponse
	if err := connection.Call(ctx, schema.ProvidersListMethodName, schema.ListProvidersRequest{}, &providers); err != nil || len(providers.Providers) != 2 {
		t.Fatalf("implemented method was not served here: %+v, %v", providers, err)
	}

	connection, _ = serve(t, providersOnly{&acptest.V2Agent{}, newFakeAgent()}, nil)
	initialize(t, connection)
	for _, method := range []string{schema.SessionForkMethodName, "_vendor/extension"} {
		rpc := rpcError(t, connection.Call(ctx, method, map[string]any{"sessionId": "source", "cwd": "/repo"}, nil))
		if rpc.Code != -32601 || rpc.Message != "Method not found" || string(rpc.Data) != `"`+method+`"` {
			t.Fatalf("%s without next = %+v (data %s)", method, rpc, rpc.Data)
		}
	}
}

func TestHandleErrorShapes(t *testing.T) {
	ctx := context.Background()

	t.Run("malformed params echo the params and never reach the handler", func(t *testing.T) {
		implementation := newFakeAgent()
		handler := acpunstable.Handle(nil, implementation)
		for _, method := range []string{schema.ProvidersSetMethodName, schema.SessionForkMethodName} {
			_, err := handler(ctx, method, json.RawMessage(`{"providerId":7,"sessionId":7}`))
			rpc := rpcError(t, err)
			var data struct {
				Error string          `json:"error"`
				JSON  json.RawMessage `json:"json"`
				Phase string          `json:"phase"`
			}
			if err := json.Unmarshal(rpc.Data, &data); err != nil {
				t.Fatal(err)
			}
			if rpc.Code != -32602 || rpc.Message != "Invalid params" || data.Error == "" || string(data.JSON) != `{"providerId":7,"sessionId":7}` || data.Phase != "deserialization" {
				t.Fatalf("%s malformed params error = %+v (data %s)", method, rpc, rpc.Data)
			}
		}
		_, err := handler(ctx, schema.ProvidersSetMethodName, nil)
		if rpc := rpcError(t, err); rpc.Code != -32602 || !strings.Contains(string(rpc.Data), `"json":null`) {
			t.Fatalf("absent params error = %+v (data %s)", rpc, rpc.Data)
		}
		if len(implementation.providers) != 0 || len(implementation.forks) != 0 {
			t.Fatal("malformed params reached the handler")
		}
	})

	t.Run("fork response without session ID is an internal error", func(t *testing.T) {
		implementation := newFakeAgent()
		implementation.forkedID = ""
		_, err := acpunstable.Handle(nil, implementation)(ctx, schema.SessionForkMethodName, json.RawMessage(`{"sessionId":"s","cwd":"/repo"}`))
		rpc := rpcError(t, err)
		var detail string
		_ = json.Unmarshal(rpc.Data, &detail)
		if rpc.Code != -32603 || rpc.Message != "Internal error" || !strings.Contains(detail, "forked session ID") {
			t.Fatalf("blank forked session error = %+v (data %s)", rpc, rpc.Data)
		}
	})

	t.Run("handler errors pass through", func(t *testing.T) {
		failure := &acp.RPCError{Code: -32002, Message: "Resource not found"}
		handler := acpunstable.Handle(nil, failingFork{err: failure})
		if _, err := handler(ctx, schema.SessionForkMethodName, json.RawMessage(`{"sessionId":"s","cwd":"/repo"}`)); !errors.Is(err, failure) {
			t.Fatalf("handler error = %v", err)
		}
	})
}

type failingFork struct{ err error }

func (f failingFork) ForkSession(context.Context, schema.ForkSessionRequest) (schema.ForkSessionResponse, error) {
	return schema.ForkSessionResponse{}, f.err
}
