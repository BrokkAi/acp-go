package unstable_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	stable "github.com/BrokkAi/acp-go/schema/v2"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	"github.com/BrokkAi/acp-go/v2/agent"
	acpunstable "github.com/BrokkAi/acp-go/v2/unstable"
)

func overrideCapability() *schema.SessionSystemPromptCapabilities {
	return &schema.SessionSystemPromptCapabilities{Mode: &schema.SessionSystemPromptModeCapabilities{
		Override: &schema.SessionSystemPromptOverrideCapabilities{},
	}}
}

// promptAgent applies client-provided system prompts: Install serves
// session/new and session/resume from its SystemPromptHandler methods and
// session/fork from ForkSession. None checks the prompt itself: the routes
// do. Its stable NewSession and ResumeSession must not be reached.
type promptAgent struct {
	*acptest.V2Agent
	capability *schema.SessionSystemPromptCapabilities
	applied    chan *acpunstable.SystemPrompt
	clients    chan bool
	requests   chan json.RawMessage
}

func (a *promptAgent) apply(ctx context.Context, prompt *acpunstable.SystemPrompt) error {
	_, ok := agent.ClientFromContext(ctx)
	a.clients <- ok
	a.applied <- prompt
	return nil
}

func (a *promptAgent) NewSession(context.Context, agent.Client, stable.NewSessionRequest) (stable.NewSessionResponse, error) {
	return stable.NewSessionResponse{}, errors.New("stable session/new reached")
}

func (a *promptAgent) ResumeSession(context.Context, agent.Client, stable.ResumeSessionRequest) (stable.ResumeSessionResponse, error) {
	return stable.ResumeSessionResponse{}, errors.New("stable session/resume reached")
}

func (a *promptAgent) NewSessionWithSystemPrompt(ctx context.Context, request schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	if err := a.apply(ctx, request.SystemPrompt); err != nil {
		return schema.NewSessionResponse{}, err
	}
	return schema.NewSessionResponse{SessionID: "created"}, nil
}

func (a *promptAgent) ResumeSessionWithSystemPrompt(ctx context.Context, request schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	return schema.ResumeSessionResponse{}, a.apply(ctx, request.SystemPrompt)
}

func (a *promptAgent) ForkSession(ctx context.Context, request schema.ForkSessionRequest) (schema.ForkSessionResponse, error) {
	if err := a.apply(ctx, request.SystemPrompt); err != nil {
		return schema.ForkSessionResponse{}, err
	}
	return schema.ForkSessionResponse{SessionID: "forked"}, nil
}

// servePromptAgent serves implementation with Install, recording the raw
// params the session/new and session/resume overrides receive.
func servePromptAgent(t *testing.T, capability *schema.SessionSystemPromptCapabilities) (*acpv2.Connection, *promptAgent) {
	t.Helper()
	implementation := &promptAgent{
		V2Agent:    &acptest.V2Agent{},
		capability: capability,
		applied:    make(chan *acpunstable.SystemPrompt, 8),
		clients:    make(chan bool, 8),
		requests:   make(chan json.RawMessage, 32),
	}
	runtime := agent.New(implementation)
	err := acpunstable.Install(runtime, implementation, func(response *schema.InitializeResponse) error {
		if capability != nil && capability.Mode != nil {
			response.Capabilities.Session.SystemPrompt.Mode = capability.Mode
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{schema.SessionNewMethodName, schema.SessionResumeMethodName} {
		serve := runtime.Overrides[method]
		if serve == nil {
			t.Fatalf("Install did not override %s", method)
		}
		runtime.Overrides[method] = func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
			implementation.requests <- raw
			return serve(ctx, method, raw)
		}
	}
	return serveInstalled(t, runtime), implementation
}

func serveInstalled(t *testing.T, runtime *agent.Runtime) *acpv2.Connection {
	t.Helper()
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), pair.B, pair.B) }()
	client := acpv2.Connect(pair.A, pair.A, nil, nil)
	t.Cleanup(func() {
		_ = client.Close()
		pair.Close()
		<-done
	})
	return client
}

type lifecycleCall func(context.Context, *acpv2.Connection, schema.InitializeResponse, *acpunstable.SystemPrompt) error

var lifecycleCalls = map[string]lifecycleCall{
	schema.SessionNewMethodName: func(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, prompt *acpunstable.SystemPrompt) error {
		_, err := acpunstable.NewSession(ctx, connection, initialization, schema.NewSessionRequest{Cwd: "/repo", SystemPrompt: prompt})
		return err
	},
	schema.SessionResumeMethodName: func(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, prompt *acpunstable.SystemPrompt) error {
		_, err := acpunstable.ResumeSession(ctx, connection, initialization, schema.ResumeSessionRequest{SessionID: "s", Cwd: "/repo", SystemPrompt: prompt})
		return err
	},
	schema.SessionForkMethodName: func(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, prompt *acpunstable.SystemPrompt) error {
		_, err := acpunstable.ForkSession(ctx, connection, initialization, schema.ForkSessionRequest{SessionID: "s", Cwd: "/repo", SystemPrompt: prompt})
		return err
	},
}

func TestSystemPromptConstructors(t *testing.T) {
	prompt := acpunstable.NewTextSystemPrompt("Review only.")
	encoded, err := json.Marshal(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"mode":"append","content":[{"type":"text","text":"Review only."}]}` {
		t.Fatalf("NewTextSystemPrompt = %s", encoded)
	}
	if prompt := acpunstable.NewSystemPrompt(); prompt.Mode != acpunstable.Append || prompt.Content != nil {
		t.Fatalf("NewSystemPrompt() = %+v", prompt)
	}
}

func TestLifecycleFacadesCarryAndApplyTheSystemPrompt(t *testing.T) {
	ctx := context.Background()
	connection, implementation := servePromptAgent(t, overrideCapability())
	initialization := initialize(t, connection)
	for _, mode := range []acpunstable.SystemPromptMode{acpunstable.Append, acpunstable.Override} {
		prompt := acpunstable.NewTextSystemPrompt("Review only.")
		prompt.Mode = mode
		for _, method := range []string{schema.SessionNewMethodName, schema.SessionResumeMethodName, schema.SessionForkMethodName} {
			if err := lifecycleCalls[method](ctx, connection, initialization, &prompt); err != nil {
				t.Fatalf("%s %s: %v", method, mode, err)
			}
			applied := <-implementation.applied
			if applied == nil || applied.Mode != mode || len(applied.Content) != 1 || applied.Content[0].Text.Text != "Review only." {
				t.Fatalf("%s %s applied %+v", method, mode, applied)
			}
			if !<-implementation.clients {
				t.Fatalf("%s handler had no agent.ClientFromContext", method)
			}
		}
	}
	// The wire form carries the mode explicitly.
	for range 4 {
		raw := <-implementation.requests
		if !strings.Contains(string(raw), `"systemPrompt":{"mode":"`) || !strings.Contains(string(raw), `"content":[{"type":"text","text":"Review only."}]}`) {
			t.Fatalf("lifecycle params = %s", raw)
		}
	}
	if err := lifecycleCalls[schema.SessionNewMethodName](ctx, connection, initialization, nil); err != nil {
		t.Fatal(err)
	}
	<-implementation.clients
	if applied := <-implementation.applied; applied != nil {
		t.Fatalf("session/new without a prompt applied %+v", applied)
	}
}

func TestLifecycleFacadesRejectUnsupportedSystemPromptsBeforeWriting(t *testing.T) {
	ctx := context.Background()
	connection, dispatched := serve(t, newFakeAgent(), nil)
	initialize(t, connection)
	withCapability := func(capability *schema.SessionSystemPromptCapabilities) schema.InitializeResponse {
		initialization := forkInitialization()
		initialization.Capabilities.Session.SystemPrompt = capability
		return initialization
	}
	text := []schema.ContentBlock{{Text: &schema.TextContent{Text: "x"}}}
	var unknown schema.ContentBlock
	if err := json.Unmarshal([]byte(`{"type":"video","uri":"file:///clip.mp4"}`), &unknown); err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		capability *schema.SessionSystemPromptCapabilities
		prompt     schema.SystemPrompt
		want       string
	}{
		"capability absent":              {nil, schema.SystemPrompt{Mode: acpunstable.Append, Content: text}, "did not advertise systemPrompt support"},
		"override without mode.override": {&schema.SessionSystemPromptCapabilities{}, schema.SystemPrompt{Mode: acpunstable.Override, Content: text}, "override mode"},
		"unknown mode":                   {overrideCapability(), schema.SystemPrompt{Mode: "replace", Content: text}, "unsupported systemPrompt mode"},
		"mode missing":                   {overrideCapability(), schema.SystemPrompt{Content: text}, "mode is required"},
		"empty content":                  {overrideCapability(), schema.SystemPrompt{Mode: acpunstable.Append, Content: []schema.ContentBlock{}}, "must not be empty"},
		"non-text content": {overrideCapability(), schema.SystemPrompt{Mode: acpunstable.Append, Content: []schema.ContentBlock{
			{Image: &schema.ImageContent{Data: "AA==", MimeType: "image/png"}},
		}}, "not text (image)"},
		"unknown content type": {overrideCapability(), schema.SystemPrompt{Mode: acpunstable.Append, Content: []schema.ContentBlock{unknown}}, `unknown type "video"`},
	} {
		for method, call := range lifecycleCalls {
			prompt := test.prompt
			if err := call(ctx, connection, withCapability(test.capability), &prompt); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("%s on %s: error = %v, want it to mention %q", name, method, err, test.want)
			}
		}
	}
	if _, err := acpunstable.NewSession(ctx, connection, schema.InitializeResponse{}, schema.NewSessionRequest{Cwd: "/repo"}); err == nil || !strings.Contains(err.Error(), "session method surface") {
		t.Fatalf("session/new without session capabilities error = %v", err)
	}
	if _, err := acpunstable.ResumeSession(ctx, connection, forkInitialization(), schema.ResumeSessionRequest{Cwd: "/repo"}); err == nil || !strings.Contains(err.Error(), "session ID is required") {
		t.Fatalf("blank resume session ID error = %v", err)
	}
	for method, call := range map[string]func() error{
		schema.SessionNewMethodName: func() error {
			_, err := acpunstable.NewSession(ctx, connection, forkInitialization(), schema.NewSessionRequest{
				Cwd: "/repo", MCPServers: []schema.McpServer{{HTTP: &schema.McpServerHttp{Name: "docs", URL: "https://mcp.example.test"}}},
			})
			return err
		},
		schema.SessionResumeMethodName: func() error {
			_, err := acpunstable.ResumeSession(ctx, connection, forkInitialization(), schema.ResumeSessionRequest{
				SessionID: "s", Cwd: "/repo", MCPServers: []schema.McpServer{{HTTP: &schema.McpServerHttp{Name: "docs", URL: "https://mcp.example.test"}}},
			})
			return err
		},
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "v2/mcp") {
			t.Fatalf("%s with inline MCP servers error = %v", method, err)
		}
	}
	expectNothingDispatched(t, dispatched)
}

func TestNewSessionRequiresASessionIDInTheResponse(t *testing.T) {
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	server := acp.Connect(pair.B, pair.B, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "initialize" {
			return json.RawMessage(`{"protocolVersion":2,"info":{"name":"agent","version":"1"},"capabilities":{"session":{}}}`), nil
		}
		return schema.NewSessionResponse{SessionID: " "}, nil
	}, nil)
	connection := acpv2.Connect(pair.A, pair.A, nil, nil)
	t.Cleanup(func() {
		_ = connection.Close()
		_ = server.Close()
		pair.Close()
	})
	if _, err := connection.Initialize(context.Background(), acpv2.Capabilities{}); err != nil {
		t.Fatal(err)
	}
	_, err := acpunstable.NewSession(context.Background(), connection, forkInitialization(), schema.NewSessionRequest{Cwd: "/repo"})
	if err == nil || !strings.Contains(err.Error(), "session ID is required") {
		t.Fatalf("blank session ID error = %v", err)
	}
}

func TestAgentRejectsUnsupportedSystemPromptsWithInvalidParams(t *testing.T) {
	ctx := context.Background()
	connection, implementation := servePromptAgent(t, &schema.SessionSystemPromptCapabilities{})
	initialize(t, connection)
	text := `[{"type":"text","text":"x"}]`
	for name, test := range map[string]struct {
		prompt string
		want   string
	}{
		"override not advertised": {`{"mode":"override","content":` + text + `}`, "override mode"},
		"unknown mode":            {`{"mode":"_vendor","content":` + text + `}`, `unsupported systemPrompt mode \"_vendor\"`},
		"empty content":           {`{"mode":"append","content":[]}`, "must not be empty"},
		"unknown content type":    {`{"mode":"append","content":[{"type":"video","uri":"file:///clip.mp4"}]}`, `unknown type \"video\"`},
	} {
		for _, method := range []string{schema.SessionNewMethodName, schema.SessionResumeMethodName, schema.SessionForkMethodName} {
			params := json.RawMessage(`{"sessionId":"s","cwd":"/repo","systemPrompt":` + test.prompt + `}`)
			rpc := rpcError(t, connection.Call(ctx, method, params, nil))
			if rpc.Code != -32602 || rpc.Message != "Invalid params" || !strings.HasPrefix(string(rpc.Data), `"`) || !strings.Contains(string(rpc.Data), test.want) {
				t.Fatalf("%s on %s = %+v (data %s)", name, method, rpc, rpc.Data)
			}
		}
	}
	// A systemPrompt the reference SDK does not deserialize answers in the
	// deserialization shape Handle uses for malformed params, with the
	// reference SDK's error text where encoding/json alone would accept it.
	untagged := "data did not match any variant of untagged enum ContentBlock"
	for prompt, want := range map[string]string{
		`{"mode":"append","content":"x"}`:                           "",
		`{"content":` + text + `}`:                                  "missing field `mode`",
		`{"mode":null,"content":` + text + `}`:                      "data did not match any variant of untagged enum SystemPromptMode",
		`{"mode":"append"}`:                                         "missing field `content`",
		`{"mode":"append","content":null}`:                          "invalid type: null, expected a sequence",
		`{"mode":"append","content":[{"type":"text"}]}`:             untagged,
		`{"mode":"append","content":[{"type":"text","text":null}]}`: untagged,
	} {
		for _, method := range []string{schema.SessionNewMethodName, schema.SessionResumeMethodName, schema.SessionForkMethodName} {
			params := `{"sessionId":"s","cwd":"/repo","systemPrompt":` + prompt + `}`
			rpc := rpcError(t, connection.Call(ctx, method, json.RawMessage(params), nil))
			var data struct {
				Error string          `json:"error"`
				JSON  json.RawMessage `json:"json"`
				Phase string          `json:"phase"`
			}
			if err := json.Unmarshal(rpc.Data, &data); err != nil || rpc.Code != -32602 || rpc.Message != "Invalid params" || data.Phase != "deserialization" || string(data.JSON) != params || (want != "" && data.Error != want) {
				t.Fatalf("malformed systemPrompt %s on %s = %+v (data %s)", prompt, method, rpc, rpc.Data)
			}
		}
	}
	select {
	case applied := <-implementation.applied:
		t.Fatalf("a rejected prompt was applied: %+v", applied)
	default:
	}
}

// promptOnly serves session/new and session/resume with the prompt, but not
// session/fork.
type promptOnly struct{}

func (promptOnly) NewSessionWithSystemPrompt(context.Context, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	return schema.NewSessionResponse{SessionID: "created"}, nil
}

func (promptOnly) ResumeSessionWithSystemPrompt(context.Context, schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	return schema.ResumeSessionResponse{}, nil
}

func installed(t *testing.T, handler any, customize ...func(*schema.InitializeResponse) error) *acpv2.Connection {
	t.Helper()
	runtime := agent.New(&acptest.V2Agent{})
	if err := acpunstable.Install(runtime, handler, customize...); err != nil {
		t.Fatal(err)
	}
	return serveInstalled(t, runtime)
}

func TestInstallAnnouncesSystemPromptOnlyWhenEveryLifecycleRequestIsServed(t *testing.T) {
	ctx := context.Background()
	var raw json.RawMessage
	connection := installed(t, promptOnly{})
	if err := connection.Call(ctx, schema.InitializeMethodName, schema.InitializeRequest{ProtocolVersion: 2}, &raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"systemPrompt":{}`) {
		t.Fatalf("initialize = %s, want systemPrompt {}", raw)
	}
	// Advertising session/fork without a ForkHandler would drop the prompt
	// on fork, so initialize fails instead of announcing it.
	connection = installed(t, promptOnly{}, func(response *schema.InitializeResponse) error {
		response.Capabilities.Session.Fork = &schema.SessionForkCapabilities{}
		return nil
	})
	if err := connection.Call(ctx, schema.InitializeMethodName, schema.InitializeRequest{ProtocolVersion: 2}, &raw); err == nil {
		t.Fatalf("initialize with an unserved systemPrompt on fork = %s", raw)
	}
	// Without a SystemPromptHandler, a customize function cannot announce it.
	connection = installed(t, struct{}{}, func(response *schema.InitializeResponse) error {
		response.Capabilities.Session.SystemPrompt = &schema.SessionSystemPromptCapabilities{}
		return nil
	})
	if err := connection.Call(ctx, schema.InitializeMethodName, schema.InitializeRequest{ProtocolVersion: 2}, &raw); err == nil {
		t.Fatalf("initialize with an unserved systemPrompt = %s", raw)
	}
}

// forkOnly serves session/fork without checking the systemPrompt it receives.
type forkOnly struct {
	applied chan *acpunstable.SystemPrompt
}

func (f forkOnly) ForkSession(_ context.Context, request schema.ForkSessionRequest) (schema.ForkSessionResponse, error) {
	f.applied <- request.SystemPrompt
	return schema.ForkSessionResponse{SessionID: "forked"}, nil
}

func TestUnannouncedSystemPromptsNeverReachTheHandler(t *testing.T) {
	ctx := context.Background()
	handler := forkOnly{applied: make(chan *acpunstable.SystemPrompt, 2)}
	connection := installed(t, handler)
	var raw json.RawMessage
	if err := connection.Call(ctx, schema.InitializeMethodName, schema.InitializeRequest{ProtocolVersion: 2}, &raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "systemPrompt") {
		t.Fatalf("initialize = %s, want no systemPrompt", raw)
	}
	params := json.RawMessage(`{"sessionId":"s","cwd":"/repo","systemPrompt":{"mode":"append","content":[{"type":"text","text":"x"}]}}`)
	if rpc := rpcError(t, connection.Call(ctx, schema.SessionForkMethodName, params, nil)); rpc.Code != -32602 || !strings.Contains(string(rpc.Data), "did not advertise systemPrompt support") {
		t.Fatalf("unannounced systemPrompt = %+v (data %s)", rpc, rpc.Data)
	}
	if err := connection.Call(ctx, schema.SessionForkMethodName, json.RawMessage(`{"sessionId":"s","cwd":"/repo"}`), nil); err != nil {
		t.Fatalf("session/fork without a prompt: %v", err)
	}
	if applied := <-handler.applied; applied != nil {
		t.Fatalf("fork applied %+v", applied)
	}
	select {
	case applied := <-handler.applied:
		t.Fatalf("a rejected prompt was applied: %+v", applied)
	default:
	}
}

func TestAdvertiseNeverAnnouncesSystemPrompt(t *testing.T) {
	hook := acpunstable.Advertise(promptOnly{})
	result, err := hook(stable.InitializeResponse{ProtocolVersion: 2, Capabilities: &stable.AgentCapabilities{Session: &stable.SessionCapabilities{}}})
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(result); strings.Contains(string(encoded), "systemPrompt") {
		t.Fatalf("Advertise = %s", encoded)
	}
}

func TestInstallRefusesAConflictingSessionResumeOverride(t *testing.T) {
	runtime := agent.New(&acptest.V2Agent{})
	runtime.Overrides = map[string]acp.Handler{schema.SessionResumeMethodName: func(context.Context, string, json.RawMessage) (any, error) { return nil, nil }}
	if err := acpunstable.Install(runtime, promptOnly{}); err == nil || !strings.Contains(err.Error(), schema.SessionResumeMethodName) {
		t.Fatalf("Install = %v", err)
	}
	if runtime.InitializeResult != nil || runtime.Extensions != nil {
		t.Fatal("a refused Install changed the runtime")
	}
}

func TestHandleOutsideARuntimeRefusesSystemPrompts(t *testing.T) {
	// No runtime recorded an initialize response, so no capability was
	// announced and any prompt is refused.
	handler := forkOnly{applied: make(chan *acpunstable.SystemPrompt, 1)}
	serve := acpunstable.Handle(nil, handler)
	params := json.RawMessage(`{"sessionId":"s","cwd":"/repo","systemPrompt":{"mode":"append","content":[{"type":"text","text":"x"}]}}`)
	_, err := serve(context.Background(), schema.SessionForkMethodName, params)
	if rpc := rpcError(t, err); rpc.Code != -32602 || !strings.Contains(string(rpc.Data), "did not advertise systemPrompt support") {
		t.Fatalf("prompt outside a runtime = %+v (data %s)", rpc, rpc.Data)
	}
	select {
	case applied := <-handler.applied:
		t.Fatalf("a rejected prompt was applied: %+v", applied)
	default:
	}
	if _, err := serve(context.Background(), schema.SessionForkMethodName, json.RawMessage(`{"sessionId":"s","cwd":"/repo"}`)); err != nil {
		t.Fatalf("session/fork without a prompt: %v", err)
	}
}
