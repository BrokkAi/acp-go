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
	"github.com/BrokkAi/acp-go/agent"
	stable "github.com/BrokkAi/acp-go/schema"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
	acpunstable "github.com/BrokkAi/acp-go/unstable"
)

func boolPointer(value bool) *bool { return &value }

// lifecycleInitialization advertises every session lifecycle method plus the
// systemPrompt capability given.
func lifecycleInitialization(systemPrompt *schema.SessionSystemPromptCapabilities) schema.InitializeResponse {
	return schema.InitializeResponse{AgentCapabilities: &schema.AgentCapabilities{
		LoadSession: boolPointer(true),
		SessionCapabilities: &schema.SessionCapabilities{
			Resume:       &schema.SessionResumeCapabilities{},
			Fork:         &schema.SessionForkCapabilities{},
			SystemPrompt: systemPrompt,
		},
	}}
}

func withOverride() *schema.SessionSystemPromptCapabilities {
	return &schema.SessionSystemPromptCapabilities{Mode: &schema.SessionSystemPromptModeCapabilities{
		Override: &schema.SessionSystemPromptOverrideCapabilities{},
	}}
}

// lifecycleCall sends one lifecycle request through its facade.
type lifecycleCall func(context.Context, *acp.Connection, schema.InitializeResponse, *acpunstable.SystemPrompt) error

var lifecycleCalls = map[string]lifecycleCall{
	schema.SessionNewMethodName: func(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, prompt *acpunstable.SystemPrompt) error {
		_, err := acpunstable.NewSession(ctx, connection, initialization, schema.NewSessionRequest{Cwd: "/repo", SystemPrompt: prompt})
		return err
	},
	schema.SessionLoadMethodName: func(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, prompt *acpunstable.SystemPrompt) error {
		_, err := acpunstable.LoadSession(ctx, connection, initialization, schema.LoadSessionRequest{SessionID: "s", Cwd: "/repo", SystemPrompt: prompt})
		return err
	},
	schema.SessionResumeMethodName: func(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, prompt *acpunstable.SystemPrompt) error {
		_, err := acpunstable.ResumeSession(ctx, connection, initialization, schema.ResumeSessionRequest{SessionID: "s", Cwd: "/repo", SystemPrompt: prompt})
		return err
	},
	schema.SessionForkMethodName: func(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, prompt *acpunstable.SystemPrompt) error {
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
	if string(encoded) != `{"content":[{"text":"Review only.","type":"text"}],"mode":"append"}` {
		t.Fatalf("NewTextSystemPrompt = %s", encoded)
	}
	override := acpunstable.NewSystemPrompt(prompt.Content...)
	override.Mode = acpunstable.Override
	if override.Mode != schema.SystemPromptModeOverride || len(override.Content) != 1 {
		t.Fatalf("NewSystemPrompt = %+v", override)
	}
}

func TestLifecycleFacadesCarryTheSystemPrompt(t *testing.T) {
	ctx := context.Background()
	connection, requests := fixture(t, func(method string) any {
		switch method {
		case schema.SessionNewMethodName, schema.SessionForkMethodName:
			return map[string]any{"sessionId": "created"}
		}
		return map[string]any{}
	})
	initialization := lifecycleInitialization(withOverride())
	for _, mode := range []acpunstable.SystemPromptMode{acpunstable.Append, acpunstable.Override} {
		prompt := acpunstable.NewTextSystemPrompt("Review only.")
		prompt.Mode = mode
		for _, method := range []string{schema.SessionNewMethodName, schema.SessionLoadMethodName, schema.SessionResumeMethodName, schema.SessionForkMethodName} {
			if err := lifecycleCalls[method](ctx, connection, initialization, &prompt); err != nil {
				t.Fatalf("%s %s: %v", method, mode, err)
			}
			request := <-requests
			var params struct {
				SystemPrompt json.RawMessage  `json:"systemPrompt"`
				MCPServers   *json.RawMessage `json:"mcpServers"`
			}
			if err := json.Unmarshal(request.params, &params); err != nil {
				t.Fatal(err)
			}
			want := `{"content":[{"text":"Review only.","type":"text"}],"mode":"` + string(mode) + `"}`
			if request.method != method || string(params.SystemPrompt) != want {
				t.Fatalf("%s frame = %s %s", method, request.method, request.params)
			}
			// mcpServers is required on the v1 session/new and session/load.
			if (method == schema.SessionNewMethodName || method == schema.SessionLoadMethodName) && (params.MCPServers == nil || string(*params.MCPServers) != "[]") {
				t.Fatalf("%s did not send an empty mcpServers array: %s", method, request.params)
			}
		}
	}
	// Without a prompt, nothing about systemPrompt reaches the wire, and no
	// systemPrompt capability is needed.
	if err := lifecycleCalls[schema.SessionNewMethodName](ctx, connection, lifecycleInitialization(nil), nil); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; strings.Contains(string(request.params), "systemPrompt") {
		t.Fatalf("session/new without a prompt sent %s", request.params)
	}
}

func TestLifecycleFacadesRejectUnsupportedSystemPromptsBeforeWriting(t *testing.T) {
	ctx := context.Background()
	connection, requests := fixture(t, func(string) any { return map[string]any{"sessionId": "created"} })
	text := []schema.ContentBlock{{Text: &schema.TextContent{Text: "x"}}}
	for name, test := range map[string]struct {
		capability *schema.SessionSystemPromptCapabilities
		prompt     schema.SystemPrompt
		want       string
	}{
		"capability absent":              {nil, schema.SystemPrompt{Mode: acpunstable.Append, Content: text}, "did not advertise systemPrompt support"},
		"override without mode.override": {&schema.SessionSystemPromptCapabilities{}, schema.SystemPrompt{Mode: acpunstable.Override, Content: text}, "override mode"},
		"unknown mode":                   {withOverride(), schema.SystemPrompt{Mode: "replace", Content: text}, "unsupported systemPrompt mode"},
		"mode missing":                   {withOverride(), schema.SystemPrompt{Content: text}, "mode is required"},
		"empty content":                  {withOverride(), schema.SystemPrompt{Mode: acpunstable.Append}, "must not be empty"},
		"non-text content": {withOverride(), schema.SystemPrompt{Mode: acpunstable.Append, Content: []schema.ContentBlock{
			{Resource: &schema.EmbeddedResource{Resource: schema.EmbeddedResourceResource{TextResourceContents: &schema.TextResourceContents{URI: "file:///a", Text: "x"}}}},
		}}, "not text (resource)"},
	} {
		for method, call := range lifecycleCalls {
			prompt := test.prompt
			if err := call(ctx, connection, lifecycleInitialization(test.capability), &prompt); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("%s on %s: error = %v, want it to mention %q", name, method, err, test.want)
			}
		}
	}
	// The lifecycle gates themselves.
	bare := schema.InitializeResponse{AgentCapabilities: &schema.AgentCapabilities{}}
	if _, err := acpunstable.LoadSession(ctx, connection, bare, schema.LoadSessionRequest{SessionID: "s", Cwd: "/repo"}); err == nil || !strings.Contains(err.Error(), "session/load") {
		t.Fatalf("load without loadSession error = %v", err)
	}
	if _, err := acpunstable.ResumeSession(ctx, connection, bare, schema.ResumeSessionRequest{SessionID: "s", Cwd: "/repo"}); err == nil || !strings.Contains(err.Error(), "session/resume") {
		t.Fatalf("resume without the capability error = %v", err)
	}
	if _, err := acpunstable.NewSession(ctx, connection, bare, schema.NewSessionRequest{Cwd: "repo"}); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("relative cwd error = %v", err)
	}
	if _, err := acpunstable.NewSession(ctx, connection, bare, schema.NewSessionRequest{Cwd: "/repo", AdditionalDirectories: []string{"/extra"}}); err == nil || !strings.Contains(err.Error(), "additionalDirectories") {
		t.Fatalf("additional directories without the capability error = %v", err)
	}
	if _, err := acpunstable.NewSession(ctx, connection, bare, schema.NewSessionRequest{
		Cwd: "/repo", MCPServers: []schema.McpServer{{Stdio: &schema.McpServerStdio{Name: "docs", Command: "/bin/docs"}}},
	}); err == nil || !strings.Contains(err.Error(), "acp-go/mcp") {
		t.Fatalf("inline MCP servers error = %v", err)
	}
	if _, err := acpunstable.LoadSession(ctx, connection, lifecycleInitialization(nil), schema.LoadSessionRequest{Cwd: "/repo"}); err == nil || !strings.Contains(err.Error(), "session ID is required") {
		t.Fatalf("blank load session ID error = %v", err)
	}
	select {
	case request := <-requests:
		t.Fatalf("validation wrote %s to the wire", request.method)
	default:
	}
}

func TestNewSessionRequiresASessionIDInTheResponse(t *testing.T) {
	connection, _ := fixture(t, func(string) any { return map[string]any{"sessionId": " "} })
	if _, err := acpunstable.NewSession(context.Background(), connection, lifecycleInitialization(nil), schema.NewSessionRequest{Cwd: "/repo"}); err == nil || !strings.Contains(err.Error(), "session ID is required") {
		t.Fatalf("blank session ID error = %v", err)
	}
}

// promptAgent applies client-provided system prompts on every lifecycle
// request: Install serves session/new, session/load, and session/resume from
// its SystemPromptHandler, SystemPromptLoader, and SystemPromptResumer
// methods, and session/fork from ForkSession. None checks the prompt itself:
// the routes do. Its stable NewSession, LoadSession, and ResumeSession must
// not be reached.
type promptAgent struct {
	*acptest.V1Agent
	capability *schema.SessionSystemPromptCapabilities
	applied    chan *acpunstable.SystemPrompt
	clients    chan bool
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

func (a *promptAgent) LoadSession(context.Context, agent.Client, stable.LoadSessionRequest) (stable.LoadSessionResponse, error) {
	return stable.LoadSessionResponse{}, errors.New("stable session/load reached")
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

func (a *promptAgent) LoadSessionWithSystemPrompt(ctx context.Context, request schema.LoadSessionRequest) (schema.LoadSessionResponse, error) {
	return schema.LoadSessionResponse{}, a.apply(ctx, request.SystemPrompt)
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

// Initialize advertises loadSession and session/resume, as an agent
// implementing the stable lifecycle would.
func (a *promptAgent) Initialize(context.Context, agent.Client, stable.InitializeRequest) (stable.InitializeResponse, error) {
	return stable.InitializeResponse{ProtocolVersion: acp.Version, AgentCapabilities: &stable.AgentCapabilities{
		LoadSession:         boolPointer(true),
		SessionCapabilities: &stable.SessionCapabilities{Resume: &stable.SessionResumeCapabilities{}},
	}}, nil
}

func serveAgent(t *testing.T, implementation agent.Agent, handler any, customize ...func(*schema.InitializeResponse) error) *acp.Connection {
	t.Helper()
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	runtime := agent.New(implementation)
	if err := acpunstable.Install(runtime, handler, customize...); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), pair.A, pair.A) }()
	client := acp.Connect(pair.B, pair.B, nil, nil)
	t.Cleanup(func() {
		_ = client.Close()
		pair.Close()
		<-done
	})
	return client
}

func servePromptAgent(t *testing.T, capability *schema.SessionSystemPromptCapabilities) (*acp.Connection, *promptAgent) {
	t.Helper()
	implementation := &promptAgent{V1Agent: &acptest.V1Agent{}, capability: capability, applied: make(chan *acpunstable.SystemPrompt, 8), clients: make(chan bool, 8)}
	connection := serveAgent(t, implementation, implementation, func(response *schema.InitializeResponse) error {
		if capability != nil && capability.Mode != nil {
			response.AgentCapabilities.SessionCapabilities.SystemPrompt.Mode = capability.Mode
		}
		return nil
	})
	return connection, implementation
}

func TestAgentAppliesSystemPromptsThroughTheRuntime(t *testing.T) {
	ctx := context.Background()
	connection, implementation := servePromptAgent(t, withOverride())
	initialization := initializeUnstable(t, connection)
	if capability := initialization.AgentCapabilities.SessionCapabilities.SystemPrompt; capability == nil || capability.Mode == nil || capability.Mode.Override == nil {
		t.Fatalf("advertised systemPrompt = %+v", capability)
	}
	prompt := acpunstable.NewTextSystemPrompt("Review only.")
	prompt.Mode = acpunstable.Override
	for _, method := range []string{schema.SessionNewMethodName, schema.SessionLoadMethodName, schema.SessionResumeMethodName, schema.SessionForkMethodName} {
		if err := lifecycleCalls[method](ctx, connection, initialization, &prompt); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		applied := <-implementation.applied
		if applied == nil || applied.Mode != acpunstable.Override || applied.Content[0].Text.Text != "Review only." {
			t.Fatalf("%s applied %+v", method, applied)
		}
		if !<-implementation.clients {
			t.Fatalf("%s handler had no agent.ClientFromContext", method)
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

func TestAgentRejectsUnsupportedSystemPromptsWithInvalidParams(t *testing.T) {
	ctx := context.Background()
	connection, implementation := servePromptAgent(t, &schema.SessionSystemPromptCapabilities{})
	initializeUnstable(t, connection)
	text := `[{"type":"text","text":"x"}]`
	for name, test := range map[string]struct {
		prompt string
		want   string
	}{
		"override not advertised": {`{"mode":"override","content":` + text + `}`, "override mode"},
		"unknown mode":            {`{"mode":"replace","content":` + text + `}`, `unsupported systemPrompt mode \"replace\"`},
		"empty content":           {`{"mode":"append","content":[]}`, "must not be empty"},
		"non-text content":        {`{"mode":"append","content":[{"type":"image","data":"AA==","mimeType":"image/png"}]}`, "not text (image)"},
	} {
		for _, method := range []string{schema.SessionNewMethodName, schema.SessionLoadMethodName, schema.SessionResumeMethodName, schema.SessionForkMethodName} {
			params := json.RawMessage(`{"sessionId":"s","cwd":"/repo","mcpServers":[],"systemPrompt":` + test.prompt + `}`)
			rpc := rpcError(t, connection.Call(ctx, method, params, nil))
			if rpc.Code != -32602 || rpc.Message != "Invalid params" || !strings.HasPrefix(string(rpc.Data), `"`) || !strings.Contains(string(rpc.Data), test.want) {
				t.Fatalf("%s on %s = %+v (data %s)", name, method, rpc, rpc.Data)
			}
		}
	}
	// A systemPrompt the reference SDK does not deserialize answers in the
	// deserialization shape Handle uses for malformed params, with the
	// reference SDK's error text where encoding/json alone would accept it.
	for prompt, want := range map[string]string{
		`"instructions"`:                                            "",
		`{"content":` + text + `}`:                                  "missing field `mode`",
		`{"mode":null,"content":` + text + `}`:                      "data did not match any variant of untagged enum SystemPromptMode",
		`{"mode":"append"}`:                                         "missing field `content`",
		`{"mode":"append","content":null}`:                          "invalid type: null, expected a sequence",
		`{"mode":"append","content":[{"type":"text"}]}`:             "missing field `text`",
		`{"mode":"append","content":[{"type":"text","text":null}]}`: "invalid type: null, expected a string",
	} {
		for _, method := range []string{schema.SessionNewMethodName, schema.SessionLoadMethodName, schema.SessionResumeMethodName, schema.SessionForkMethodName} {
			params := `{"sessionId":"s","cwd":"/repo","mcpServers":[],"systemPrompt":` + prompt + `}`
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

// newOnly serves session/new with the prompt but not session/load or
// session/resume.
type newOnly struct{}

func (newOnly) NewSessionWithSystemPrompt(context.Context, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	return schema.NewSessionResponse{SessionID: "created"}, nil
}

func TestInstallAnnouncesSystemPromptOnlyWhenEveryLifecycleRequestIsServed(t *testing.T) {
	lifecycle := &promptAgent{V1Agent: &acptest.V1Agent{}}
	// A handler that serves session/new alone, on an agent that advertises
	// loadSession and session/resume: load and resume would drop the prompt.
	connection := serveAgent(t, lifecycle, newOnly{})
	initialization := initializeUnstable(t, connection)
	if session := initialization.AgentCapabilities.SessionCapabilities; session.SystemPrompt != nil {
		t.Fatalf("systemPrompt announced without load and resume handlers: %+v", session.SystemPrompt)
	}
	// newOnly checks nothing, yet a prompt the connection was never told it
	// may send still never reaches it.
	params := json.RawMessage(`{"cwd":"/repo","mcpServers":[],"systemPrompt":{"mode":"append","content":[{"type":"text","text":"x"}]}}`)
	if rpc := rpcError(t, connection.Call(context.Background(), schema.SessionNewMethodName, params, nil)); rpc.Code != -32602 || !strings.Contains(string(rpc.Data), "did not advertise systemPrompt support") {
		t.Fatalf("unannounced systemPrompt = %+v (data %s)", rpc, rpc.Data)
	}
	if err := connection.Call(context.Background(), schema.SessionNewMethodName, json.RawMessage(`{"cwd":"/repo","mcpServers":[]}`), nil); err != nil {
		t.Fatalf("session/new without a prompt: %v", err)
	}
	// The same handler on an agent with neither: the prompt is served
	// everywhere it can arrive, so {} (append only) is announced.
	connection = serveAgent(t, &acptest.V1Agent{}, newOnly{})
	initialization = initializeUnstable(t, connection)
	encoded, _ := json.Marshal(initialization.AgentCapabilities)
	if !strings.Contains(string(encoded), `"systemPrompt":{}`) {
		t.Fatalf("agentCapabilities = %s, want systemPrompt {}", encoded)
	}
	// A customize function that announces the capability anyway fails
	// initialize instead of sending it.
	connection = serveAgent(t, lifecycle, newOnly{}, func(response *schema.InitializeResponse) error {
		response.AgentCapabilities.SessionCapabilities.SystemPrompt = &schema.SessionSystemPromptCapabilities{}
		return nil
	})
	var response schema.InitializeResponse
	if err := connection.Call(context.Background(), schema.InitializeMethodName, schema.InitializeRequest{ProtocolVersion: schema.ProtocolVersion(acp.Version)}, &response); err == nil {
		t.Fatalf("initialize with an unserved systemPrompt succeeded: %+v", response)
	}
}

func TestAdvertiseNeverAnnouncesSystemPrompt(t *testing.T) {
	hook := acpunstable.Advertise(newOnly{})
	result, err := hook(stable.InitializeResponse{ProtocolVersion: acp.Version})
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(result); strings.Contains(string(encoded), "systemPrompt") {
		t.Fatalf("Advertise = %s", encoded)
	}
}

func TestInstallRefusesAConflictingSessionNewOverride(t *testing.T) {
	runtime := agent.New(&acptest.V1Agent{})
	runtime.Overrides = map[string]acp.Handler{schema.SessionNewMethodName: func(context.Context, string, json.RawMessage) (any, error) { return nil, nil }}
	if err := acpunstable.Install(runtime, newOnly{}); err == nil || !strings.Contains(err.Error(), schema.SessionNewMethodName) {
		t.Fatalf("Install = %v", err)
	}
	if runtime.InitializeResult != nil || runtime.Extensions != nil {
		t.Fatal("a refused Install changed the runtime")
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

const forkWithPrompt = `{"sessionId":"s","cwd":"/repo","systemPrompt":{"mode":"append","content":[{"type":"text","text":"x"}]}}`

func TestUnannouncedSystemPromptsNeverReachTheHandler(t *testing.T) {
	ctx := context.Background()
	handler := forkOnly{applied: make(chan *acpunstable.SystemPrompt, 2)}
	connection := serveAgent(t, &acptest.V1Agent{}, handler)
	var raw json.RawMessage
	if err := connection.Call(ctx, schema.InitializeMethodName, schema.InitializeRequest{ProtocolVersion: schema.ProtocolVersion(acp.Version)}, &raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "systemPrompt") || !strings.Contains(string(raw), `"fork":{}`) {
		t.Fatalf("initialize = %s, want fork without systemPrompt", raw)
	}
	if rpc := rpcError(t, connection.Call(ctx, schema.SessionForkMethodName, json.RawMessage(forkWithPrompt), nil)); rpc.Code != -32602 || !strings.Contains(string(rpc.Data), "did not advertise systemPrompt support") {
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

func TestHandleOutsideARuntimeRefusesSystemPrompts(t *testing.T) {
	// No runtime recorded an initialize response, so no capability was
	// announced and any prompt is refused.
	handler := forkOnly{applied: make(chan *acpunstable.SystemPrompt, 1)}
	serve := acpunstable.Handle(nil, handler)
	_, err := serve(context.Background(), schema.SessionForkMethodName, json.RawMessage(forkWithPrompt))
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
