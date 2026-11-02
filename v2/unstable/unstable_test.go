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
	acpunstable "github.com/BrokkAi/acp-go/v2/unstable"
)

type recorded struct {
	method string
	params json.RawMessage
}

// connect serves agent on one end of an in-memory pair and returns a v2 client
// connection on the other, plus every request the agent received in order.
func connect(t *testing.T, agent acp.Handler) (*acpv2.Connection, chan recorded) {
	t.Helper()
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	requests := make(chan recorded, 16)
	server := acp.Connect(pair.B, pair.B, func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		requests <- recorded{method: method, params: append(json.RawMessage(nil), raw...)}
		return agent(ctx, method, raw)
	}, nil)
	client := acpv2.Connect(pair.A, pair.A, nil, nil)
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		pair.Close()
	})
	return client, requests
}

func expectNoRequest(t *testing.T, requests chan recorded) {
	t.Helper()
	select {
	case request := <-requests:
		t.Fatalf("validation wrote %s to the wire", request.method)
	default:
	}
}

// fakeAgent is a typed unstable agent: it keeps provider configuration in
// memory and records fork requests.
type fakeAgent struct {
	mu        sync.Mutex
	providers map[schema.ProviderId]schema.ProviderCurrentConfig
	forks     []schema.ForkSessionRequest
	forkedID  schema.SessionId
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{providers: map[schema.ProviderId]schema.ProviderCurrentConfig{}, forkedID: "forked"}
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

// unstableInitialize answers initialize with the unstable response, which is
// the only one that can advertise providers and session/fork, and defers every
// other baseline method to the shared v2 fake agent.
func unstableInitialize(next acp.Handler) acp.Handler {
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if method != schema.InitializeMethodName {
			return next(ctx, method, raw)
		}
		return schema.InitializeResponse{
			ProtocolVersion: schema.ProtocolVersion(acpv2.Version),
			Info:            schema.Implementation{Name: "unstable-agent", Version: "1"},
			Capabilities: &schema.AgentCapabilities{
				Providers: &schema.ProvidersCapabilities{},
				Session: &schema.SessionCapabilities{
					Fork:                  &schema.SessionForkCapabilities{},
					AdditionalDirectories: &schema.SessionAdditionalDirectoriesCapabilities{},
				},
			},
		}, nil
	}
}

// initialize sends initialize and decodes the result into the unstable
// response, as the package documentation describes.
func initialize(t *testing.T, connection *acpv2.Connection) schema.InitializeResponse {
	t.Helper()
	var initialization schema.InitializeResponse
	if err := connection.Call(context.Background(), schema.InitializeMethodName, schema.InitializeRequest{
		ProtocolVersion: schema.ProtocolVersion(acpv2.Version),
		Info:            schema.Implementation{Name: "client", Version: "1"},
	}, &initialization); err != nil {
		t.Fatal(err)
	}
	return initialization
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

func TestProviderFacadesFailBeforeWriting(t *testing.T) {
	ctx := context.Background()
	connection, requests := connect(t, acpunstable.Handle(acptest.AgentHandler(2), newFakeAgent()))
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
	expectNoRequest(t, requests)
}

func TestProviderFacadesRoundTripThroughHandle(t *testing.T) {
	ctx := context.Background()
	connection, requests := connect(t, unstableInitialize(acpunstable.Handle(acptest.AgentHandler(2), newFakeAgent())))
	initialization := initialize(t, connection)
	<-requests

	if err := connection.Call(ctx, schema.SessionNewMethodName, schema.NewSessionRequest{Cwd: "/repo"}, new(json.RawMessage)); err != nil {
		t.Fatalf("baseline method was not delegated to the next handler: %v", err)
	}
	<-requests

	if _, err := acpunstable.SetProvider(ctx, connection, initialization, schema.SetProviderRequest{
		ProviderID: "main", APIType: "openai", BaseURL: "https://llm.example.test",
		Headers: map[string]string{"authorization": "Bearer token"},
	}); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	if request.method != schema.ProvidersSetMethodName {
		t.Fatalf("method = %s", request.method)
	}
	var set map[string]any
	if err := json.Unmarshal(request.params, &set); err != nil {
		t.Fatal(err)
	}
	if set["providerId"] != "main" || set["apiType"] != "openai" || set["baseUrl"] != "https://llm.example.test" {
		t.Fatalf("providers/set params = %s", request.params)
	}

	providers, err := acpunstable.ListProviders(ctx, connection, initialization)
	if err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.method != schema.ProvidersListMethodName || string(request.params) != "{}" {
		t.Fatalf("providers/list frame = %s %s", request.method, request.params)
	}
	if len(providers.Providers) != 2 || providers.Providers[0].Current == nil || providers.Providers[0].Current.BaseURL != "https://llm.example.test" || providers.Providers[1].Current != nil {
		t.Fatalf("providers after set = %+v", providers.Providers)
	}

	if _, err := acpunstable.DisableProvider(ctx, connection, initialization, "main"); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.method != schema.ProvidersDisableMethodName || !strings.Contains(string(request.params), `"providerId":"main"`) {
		t.Fatalf("providers/disable frame = %s %s", request.method, request.params)
	}
	providers, err = acpunstable.ListProviders(ctx, connection, initialization)
	if err != nil {
		t.Fatal(err)
	}
	<-requests
	if providers.Providers[0].Current != nil {
		t.Fatalf("provider still configured after disable: %+v", providers.Providers[0])
	}
}

func TestForkSessionFailsBeforeWriting(t *testing.T) {
	ctx := context.Background()
	connection, requests := connect(t, acpunstable.Handle(acptest.AgentHandler(2), newFakeAgent()))
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
	expectNoRequest(t, requests)
}

func TestForkSessionRoundTripThroughHandle(t *testing.T) {
	ctx := context.Background()
	agent := newFakeAgent()
	connection, requests := connect(t, unstableInitialize(acpunstable.Handle(acptest.AgentHandler(2), agent)))
	initialization := initialize(t, connection)
	<-requests

	request := schema.ForkSessionRequest{
		SessionID:             "source",
		Cwd:                   `C:\agent\workspace`,
		AdditionalDirectories: []schema.AbsolutePath{"/", `C:\agent\extra`},
		SystemPrompt: &schema.SystemPrompt{
			Mode:    schema.SystemPromptModeAppend,
			Content: []schema.ContentBlock{{Text: &schema.TextContent{Text: "Summarize only."}}},
		},
	}
	result, err := acpunstable.ForkSession(ctx, connection, initialization, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "forked" {
		t.Fatalf("forked session = %q", result.SessionID)
	}
	frame := <-requests
	if frame.method != schema.SessionForkMethodName {
		t.Fatalf("method = %s", frame.method)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(frame.params, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["mcpServers"]; ok {
		t.Fatalf("fork without servers sent mcpServers: %s", frame.params)
	}
	if string(wire["systemPrompt"]) != `{"content":[{"text":"Summarize only.","type":"text"}],"mode":"append"}` {
		t.Fatalf("systemPrompt was not passed through unchanged: %s", wire["systemPrompt"])
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.forks) != 1 || agent.forks[0].SessionID != "source" || agent.forks[0].SystemPrompt == nil || agent.forks[0].SystemPrompt.Mode != schema.SystemPromptModeAppend {
		t.Fatalf("agent received %+v", agent.forks)
	}
}

func TestForkSessionRejectsBlankForkedSessionID(t *testing.T) {
	// A raw agent, not Handle, so the blank ID reaches the client.
	connection, requests := connect(t, func(context.Context, string, json.RawMessage) (any, error) {
		return schema.ForkSessionResponse{SessionID: " "}, nil
	})
	_, err := acpunstable.ForkSession(context.Background(), connection, forkInitialization(), schema.ForkSessionRequest{SessionID: "source", Cwd: "/repo"})
	if err == nil || !strings.Contains(err.Error(), "forked session ID is required") {
		t.Fatalf("blank forked session ID error = %v", err)
	}
	if request := <-requests; request.method != schema.SessionForkMethodName {
		t.Fatalf("method = %s", request.method)
	}
}

func rpcError(t *testing.T, err error) *acp.RPCError {
	t.Helper()
	var rpc *acp.RPCError
	if !errors.As(err, &rpc) {
		t.Fatalf("error = %v, want an RPC error", err)
	}
	return rpc
}

func TestHandleDispatch(t *testing.T) {
	ctx := context.Background()
	next := func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		return "next:" + method, nil
	}

	t.Run("delegates methods it does not own", func(t *testing.T) {
		handler := acpunstable.Handle(next, newFakeAgent())
		// nes/* left the upstream schema, so it is just another foreign method.
		for _, method := range []string{schema.SessionNewMethodName, "nes/start", "_vendor/extension"} {
			result, err := handler(ctx, method, json.RawMessage(`{}`))
			if err != nil || result != "next:"+method {
				t.Fatalf("%s fallthrough = %#v, %v", method, result, err)
			}
		}
	})

	t.Run("missing handler answers method not found", func(t *testing.T) {
		providersOnly := struct{ acpunstable.ProviderHandler }{newFakeAgent()}
		handler := acpunstable.Handle(next, providersOnly)
		if _, err := handler(ctx, schema.ProvidersListMethodName, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		_, err := handler(ctx, schema.SessionForkMethodName, json.RawMessage(`{"sessionId":"s","cwd":"/repo"}`))
		rpc := rpcError(t, err)
		if rpc.Code != -32601 || rpc.Message != "Method not found" || string(rpc.Data) != `"session/fork"` {
			t.Fatalf("missing fork handler error = %+v", rpc)
		}
		for _, method := range []string{schema.ProvidersListMethodName, schema.ProvidersSetMethodName, schema.ProvidersDisableMethodName, schema.SessionForkMethodName} {
			_, err := acpunstable.Handle(next, nil)(ctx, method, json.RawMessage(`{}`))
			if rpc := rpcError(t, err); rpc.Code != -32601 {
				t.Fatalf("%s with no handler = %+v", method, rpc)
			}
		}
		_, err = acpunstable.Handle(nil, nil)(ctx, schema.SessionNewMethodName, json.RawMessage(`{}`))
		if rpc := rpcError(t, err); rpc.Code != -32601 || string(rpc.Data) != `"session/new"` {
			t.Fatalf("unhandled method without next = %+v", rpc)
		}
	})

	t.Run("malformed params never reach the handler", func(t *testing.T) {
		agent := newFakeAgent()
		handler := acpunstable.Handle(nil, agent)
		for _, method := range []string{schema.ProvidersSetMethodName, schema.SessionForkMethodName} {
			_, err := handler(ctx, method, json.RawMessage(`{"providerId":7,"sessionId":7}`))
			rpc := rpcError(t, err)
			if rpc.Code != -32602 || rpc.Message != "Invalid params" || !strings.Contains(string(rpc.Data), `"phase":"deserialization"`) {
				t.Fatalf("%s malformed params error = %+v (data %s)", method, rpc, rpc.Data)
			}
		}
		if len(agent.providers) != 0 || len(agent.forks) != 0 {
			t.Fatal("malformed params reached the handler")
		}
	})

	t.Run("fork response without session ID is an internal error", func(t *testing.T) {
		agent := newFakeAgent()
		agent.forkedID = ""
		_, err := acpunstable.Handle(nil, agent)(ctx, schema.SessionForkMethodName, json.RawMessage(`{"sessionId":"s","cwd":"/repo"}`))
		if rpc := rpcError(t, err); rpc.Code != -32603 {
			t.Fatalf("blank forked session error = %+v", rpc)
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
