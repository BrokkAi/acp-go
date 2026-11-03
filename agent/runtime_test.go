package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
)

type testAgent struct {
	prompt func(context.Context, Client, schema.PromptRequest, SessionUpdater) (schema.PromptResponse, error)
}

func (a testAgent) Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{ProtocolVersion: acp.Version}, nil
}

func (a testAgent) NewSession(context.Context, Client, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	return schema.NewSessionResponse{SessionID: "session"}, nil
}

func (a testAgent) Prompt(ctx context.Context, client Client, request schema.PromptRequest, updates SessionUpdater) (schema.PromptResponse, error) {
	if a.prompt == nil {
		if err := updates.Update(schema.SessionUpdate{
			AgentMessageChunk: &schema.ContentChunk{Content: acp.NewTextContent("working")},
		}); err != nil {
			return schema.PromptResponse{}, err
		}
		return schema.PromptResponse{StopReason: schema.StopReasonEndTurn}, nil
	}
	return a.prompt(ctx, client, request, updates)
}

func startRuntime(t *testing.T, implementation Agent, clientHost acp.Handler, notifications acp.Notifications) *acp.Connection {
	t.Helper()
	agentSide, clientSide := net.Pipe()
	runtime := New(implementation)
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), agentSide, agentSide) }()
	t.Cleanup(func() {
		_ = clientSide.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("agent runtime: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	return acp.Connect(clientSide, clientSide, clientHost, notifications)
}

func initializeClient(t *testing.T, connection *acp.Connection, capabilities acp.Capabilities) acp.Initialization {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initialization, err := connection.Initialize(ctx, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	return initialization
}

func TestRuntimeServesMandatoryLifecycleAndStreamsUpdates(t *testing.T) {
	updates := make(chan acp.Update, 1)
	connection := startRuntime(t, testAgent{}, nil, acp.SessionUpdates(func(update acp.Update) error {
		updates <- update
		return nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initialization := initializeClient(t, connection, acp.Capabilities{})
	session, err := connection.NewSession(ctx, hostRoot+"/tmp")
	if err != nil {
		t.Fatal(err)
	}
	reason, err := connection.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent("hello")})
	if err != nil || reason != schema.StopReasonEndTurn {
		t.Fatalf("prompt: %v (%s)", err, reason)
	}
	select {
	case update := <-updates:
		if update.SessionID != session.SessionID || update.Update.AgentMessageChunk == nil {
			t.Fatalf("unexpected update: %+v", update)
		}
	default:
		t.Fatal("session update was not delivered")
	}
}

type loadingAgent struct {
	testAgent
	loaded chan schema.LoadSessionRequest
}

type unadvertisedResumingAgent struct{ testAgent }

func (unadvertisedResumingAgent) ResumeSession(context.Context, Client, schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	return schema.ResumeSessionResponse{}, nil
}

func (loadingAgent) Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	enabled := true
	return schema.InitializeResponse{
		ProtocolVersion:   acp.Version,
		AgentCapabilities: &schema.AgentCapabilities{LoadSession: &enabled},
	}, nil
}

func (a loadingAgent) LoadSession(_ context.Context, _ Client, request schema.LoadSessionRequest) (schema.LoadSessionResponse, error) {
	a.loaded <- request
	return schema.LoadSessionResponse{}, nil
}

func TestRuntimeDispatchesOptionalMethodsAndReportsUnsupportedMethods(t *testing.T) {
	loaded := make(chan schema.LoadSessionRequest, 1)
	connection := startRuntime(t, loadingAgent{loaded: loaded}, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initializeClient(t, connection, acp.Capabilities{})

	var loadedResult schema.LoadSessionResponse
	if err := connection.Call(ctx, schema.SessionLoadMethodName, schema.LoadSessionRequest{
		SessionID: "old", Cwd: hostRoot + "/tmp",
	}, &loadedResult); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-loaded:
		if request.SessionID != "old" {
			t.Fatalf("loaded session = %+v", request)
		}
	default:
		t.Fatal("optional loader was not called")
	}

	err := connection.Call(ctx, schema.SessionResumeMethodName, schema.ResumeSessionRequest{
		SessionID: "old", Cwd: hostRoot + "/tmp",
	}, nil)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("unsupported method error = %v", err)
	}
}

// requireInvalidRequest checks that err is the reference SDK's -32600 error:
// the generic message, with the explanation as a JSON string in data.
func requireInvalidRequest(t *testing.T, err error, detail string) {
	t.Helper()
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32600 || rpcErr.Message != "Invalid request" {
		t.Fatalf("error = %v, want -32600 Invalid request", err)
	}
	var data string
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil || data != detail {
		t.Fatalf("error data = %s, want %q", rpcErr.Data, detail)
	}
}

const (
	initializeInProgress    = "ACP initialization is already in progress on this connection"
	initializeNotRepeatable = "ACP initialization cannot be repeated after any other request; reconnect to initialize again"
	sessionNewBeforeInit    = "ACP initialization must complete before `session/new` can be used"
)

func newSessionCall(ctx context.Context, connection *acp.Connection) error {
	return connection.Call(ctx, schema.SessionNewMethodName, schema.NewSessionRequest{
		Cwd: hostRoot + "/tmp", MCPServers: []schema.McpServer{},
	}, nil)
}

func TestRuntimeRequiresInitializeAndAcceptsRepeats(t *testing.T) {
	implementation := &recordingAgent{}
	connection := startRuntime(t, implementation, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	requireInvalidRequest(t, newSessionCall(ctx, connection), sessionNewBeforeInit)
	initializeClient(t, connection, acp.Capabilities{})
	if err := newSessionCall(ctx, connection); err != nil {
		t.Fatalf("session/new after initialize: %v", err)
	}
	// Like the reference SDK's unchecked v1 lifecycle, a repeat reaches the
	// agent at any time, even after other requests.
	var response schema.InitializeResponse
	if err := connection.Call(ctx, schema.InitializeMethodName, schema.InitializeRequest{ProtocolVersion: acp.Version}, &response); err != nil {
		t.Fatalf("repeated initialize: %v", err)
	}
	if response.ProtocolVersion != acp.Version {
		t.Fatalf("repeated initialize selected %d", response.ProtocolVersion)
	}
	if calls := implementation.calls.Load(); calls != 2 {
		t.Fatalf("Initialize calls = %d, want 2", calls)
	}
	if err := newSessionCall(ctx, connection); err != nil {
		t.Fatalf("session/new after repeated initialize: %v", err)
	}
}

type initializeOutcome struct {
	response schema.InitializeResponse
	err      error
}

// gatedInitializeAgent blocks every Initialize call until the test releases
// it with an outcome, so other requests can be sent while it is running.
type gatedInitializeAgent struct {
	testAgent
	calls   atomic.Int32
	entered chan struct{}
	release chan initializeOutcome
}

func newGatedInitializeAgent() *gatedInitializeAgent {
	return &gatedInitializeAgent{entered: make(chan struct{}, 2), release: make(chan initializeOutcome)}
}

func (a *gatedInitializeAgent) Initialize(ctx context.Context, _ Client, _ schema.InitializeRequest) (schema.InitializeResponse, error) {
	a.calls.Add(1)
	a.entered <- struct{}{}
	select {
	case outcome := <-a.release:
		return outcome.response, outcome.err
	case <-ctx.Done():
		return schema.InitializeResponse{}, ctx.Err()
	}
}

var initializeSucceeds = initializeOutcome{response: schema.InitializeResponse{ProtocolVersion: acp.Version}}

// startBlockedInitialize sends an initialize request and returns once the
// agent's Initialize handler is running and blocked. The returned channel
// yields the request's outcome after the handler is released.
func startBlockedInitialize(ctx context.Context, t *testing.T, connection *acp.Connection, implementation *gatedInitializeAgent) <-chan error {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		result <- connection.Call(ctx, schema.InitializeMethodName, schema.InitializeRequest{ProtocolVersion: acp.Version}, nil)
	}()
	select {
	case <-implementation.entered:
	case <-ctx.Done():
		t.Fatal("initialize did not reach the agent")
	}
	return result
}

func TestRuntimePassesConcurrentInitializeToAgent(t *testing.T) {
	implementation := newGatedInitializeAgent()
	connection := startRuntime(t, implementation, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	first := startBlockedInitialize(ctx, t, connection, implementation)
	// Other requests wait for the first successful initialize.
	requireInvalidRequest(t, newSessionCall(ctx, connection), sessionNewBeforeInit)
	// The reference SDK's v1 agent does not track an in-progress
	// initialization, so a concurrent one reaches the agent too.
	second := startBlockedInitialize(ctx, t, connection, implementation)
	implementation.release <- initializeSucceeds
	implementation.release <- initializeSucceeds
	for _, result := range []<-chan error{first, second} {
		if err := <-result; err != nil {
			t.Fatalf("initialize: %v", err)
		}
	}
	if calls := implementation.calls.Load(); calls != 2 {
		t.Fatalf("Initialize calls = %d, want 2", calls)
	}
	if err := newSessionCall(ctx, connection); err != nil {
		t.Fatalf("session/new after initialize: %v", err)
	}
}

func TestRuntimeAllowsInitializeRetryAfterFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure initializeOutcome
	}{
		{name: "handler error", failure: initializeOutcome{err: errors.New("initialize failed")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			implementation := newGatedInitializeAgent()
			connection := startRuntime(t, implementation, nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			first := startBlockedInitialize(ctx, t, connection, implementation)
			requireInvalidRequest(t, newSessionCall(ctx, connection), sessionNewBeforeInit)
			implementation.release <- test.failure
			if err := <-first; err == nil {
				t.Fatal("failed initialize returned no error")
			}
			requireInvalidRequest(t, newSessionCall(ctx, connection), sessionNewBeforeInit)

			retry := startBlockedInitialize(ctx, t, connection, implementation)
			implementation.release <- initializeSucceeds
			if err := <-retry; err != nil {
				t.Fatalf("retried initialize: %v", err)
			}
			if calls := implementation.calls.Load(); calls != 2 {
				t.Fatalf("Initialize calls = %d, want 2", calls)
			}
			if err := newSessionCall(ctx, connection); err != nil {
				t.Fatalf("session/new after retried initialize: %v", err)
			}

			// A failed repeat keeps the previous initialization.
			repeat := startBlockedInitialize(ctx, t, connection, implementation)
			implementation.release <- test.failure
			if err := <-repeat; err == nil {
				t.Fatal("failed repeat returned no error")
			}
			if err := newSessionCall(ctx, connection); err != nil {
				t.Fatalf("session/new after failed repeat: %v", err)
			}
		})
	}
}

// recordingAgent records each initialize request it receives.
type recordingAgent struct {
	testAgent
	calls    atomic.Int32
	requests chan schema.InitializeRequest
}

func (a *recordingAgent) Initialize(_ context.Context, _ Client, request schema.InitializeRequest) (schema.InitializeResponse, error) {
	a.calls.Add(1)
	if a.requests != nil {
		a.requests <- request
	}
	return schema.InitializeResponse{ProtocolVersion: acp.Version}, nil
}

func TestRuntimeNegotiatesV1ForEveryValidVersion(t *testing.T) {
	for _, test := range []struct {
		name   string
		params string
		check  func(*testing.T, schema.InitializeRequest)
	}{
		{
			name:   "older version",
			params: `{"protocolVersion":0,"clientInfo":{"name":"old","version":"0"}}`,
			check: func(t *testing.T, request schema.InitializeRequest) {
				if request.ClientInfo == nil || request.ClientInfo.Name != "old" {
					t.Fatalf("clientInfo = %+v", request.ClientInfo)
				}
			},
		},
		{
			name: "v2 request",
			params: `{"protocolVersion":2,"info":{"name":"v2-client","version":"2.0","title":7},` +
				`"capabilities":{"auth":{"terminal":{}},"elicitation":{"form":{}},"fs":{"readTextFile":true},"terminal":true},` +
				`"_meta":{"trace":"x"}}`,
			check: func(t *testing.T, request schema.InitializeRequest) {
				if request.ClientInfo == nil || request.ClientInfo.Name != "v2-client" || request.ClientInfo.Title != nil {
					t.Fatalf("clientInfo = %+v", request.ClientInfo)
				}
				capabilities := request.ClientCapabilities
				if capabilities == nil || capabilities.Auth == nil || capabilities.Auth.Terminal == nil || !*capabilities.Auth.Terminal ||
					capabilities.Elicitation == nil || capabilities.Elicitation.Form == nil ||
					capabilities.Session == nil || capabilities.Session.ConfigOptions == nil ||
					capabilities.Session.ConfigOptions.Boolean == nil {
					t.Fatalf("capabilities = %+v", capabilities)
				}
				// Unknown newer fields never enable v1 file-system or
				// terminal-execution capabilities.
				if capabilities.Fs != nil || capabilities.Terminal != nil {
					t.Fatalf("v1-only capabilities enabled: %+v", capabilities)
				}
				if request.Meta["trace"] != "x" {
					t.Fatalf("_meta = %v", request.Meta)
				}
			},
		},
		{
			name:   "newer version with malformed fields",
			params: `{"protocolVersion":7,"info":"malformed","capabilities":{"elicitation":"malformed"},"_meta":7}`,
			check: func(t *testing.T, request schema.InitializeRequest) {
				if request.ClientInfo != nil || request.Meta != nil {
					t.Fatalf("request = %+v", request)
				}
				capabilities := request.ClientCapabilities
				if capabilities == nil || capabilities.Elicitation != nil || capabilities.Session == nil {
					t.Fatalf("capabilities = %+v", capabilities)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			implementation := &recordingAgent{requests: make(chan schema.InitializeRequest, 1)}
			connection := startRuntime(t, implementation, nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var response schema.InitializeResponse
			if err := connection.Call(ctx, schema.InitializeMethodName, json.RawMessage(test.params), &response); err != nil {
				t.Fatal(err)
			}
			if response.ProtocolVersion != acp.Version {
				t.Fatalf("selected protocol = %d", response.ProtocolVersion)
			}
			request := <-implementation.requests
			if request.ProtocolVersion != acp.Version {
				t.Fatalf("agent saw protocol %d", request.ProtocolVersion)
			}
			test.check(t, request)
		})
	}
}

func TestRuntimeRejectsMalformedProtocolVersion(t *testing.T) {
	for _, params := range []string{
		`{}`, `{"protocolVersion":null}`, `{"protocolVersion":"1"}`, `{"protocolVersion":1.0}`,
		`{"protocolVersion":-1}`, `{"protocolVersion":65536}`, `[]`,
	} {
		t.Run(params, func(t *testing.T) {
			implementation := &recordingAgent{}
			connection := startRuntime(t, implementation, nil, nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := connection.Call(ctx, schema.InitializeMethodName, json.RawMessage(params), nil)
			requireInvalidParams(t, err, "initialize.protocolVersion must be a valid ACP protocol version")
			if calls := implementation.calls.Load(); calls != 0 {
				t.Fatalf("Initialize calls = %d, want 0", calls)
			}
		})
	}
}

// requireInvalidParams checks that err is the reference SDK's -32602 error
// with detail as a JSON string in data.
func requireInvalidParams(t *testing.T, err error, detail string) {
	t.Helper()
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 || rpcErr.Message != "Invalid params" {
		t.Fatalf("error = %v, want -32602 Invalid params", err)
	}
	var data string
	if err := json.Unmarshal(rpcErr.Data, &data); err != nil || data != detail {
		t.Fatalf("error data = %s, want %q", rpcErr.Data, detail)
	}
}

func TestRuntimeGatesOptionalMethodsOnAgentCapabilities(t *testing.T) {
	connection := startRuntime(t, unadvertisedResumingAgent{}, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initializeClient(t, connection, acp.Capabilities{})
	err := connection.Call(ctx, schema.SessionResumeMethodName, schema.ResumeSessionRequest{
		SessionID: "old", Cwd: hostRoot + "/tmp",
	}, nil)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 || rpcErr.Message != "Method not found" || string(rpcErr.Data) != `"session/resume"` {
		t.Fatalf("capability gate error = %v", err)
	}
}

type cancellingAgent struct {
	testAgent
	entered   chan struct{}
	cancelled chan struct{}
}

func (a cancellingAgent) Prompt(ctx context.Context, _ Client, _ schema.PromptRequest, _ SessionUpdater) (schema.PromptResponse, error) {
	close(a.entered)
	<-ctx.Done()
	return schema.PromptResponse{}, ctx.Err()
}

func (a cancellingAgent) CancelSession(context.Context, schema.CancelNotification) error {
	a.cancelled <- struct{}{}
	return nil
}

func TestSessionCancelNotificationCancelsActivePrompt(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	connection := startRuntime(t, cancellingAgent{entered: entered, cancelled: cancelled}, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initializeClient(t, connection, acp.Capabilities{})
	session, err := connection.NewSession(ctx, hostRoot+"/tmp")
	if err != nil {
		t.Fatal(err)
	}
	promptDone := make(chan error, 1)
	go func() {
		_, err := connection.Prompt(ctx, session, "wait")
		promptDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("prompt did not start")
	}
	if err := connection.CancelSession(ctx, session.SessionID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-promptDone:
		var rpcErr *acp.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32800 {
			t.Fatalf("cancelled prompt error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt was not cancelled")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("session cancellation hook was not called")
	}
}

func TestAgentClientMethodsAreGatedByAdvertisedCapabilities(t *testing.T) {
	prompt := func(ctx context.Context, client Client, request schema.PromptRequest, updates SessionUpdater) (schema.PromptResponse, error) {
		file, err := client.ReadTextFile(ctx, schema.ReadTextFileRequest{
			SessionID: request.SessionID, Path: hostRoot + "/tmp/source.txt",
		})
		if err != nil {
			return schema.PromptResponse{}, err
		}
		if err := updates.Update(schema.SessionUpdate{
			AgentMessageChunk: &schema.ContentChunk{Content: acp.NewTextContent(file.Content)},
		}); err != nil {
			return schema.PromptResponse{}, err
		}
		return schema.PromptResponse{StopReason: schema.StopReasonEndTurn}, nil
	}

	t.Run("advertised", func(t *testing.T) {
		updates := make(chan acp.Update, 1)
		connection := startRuntime(t, testAgent{prompt: prompt}, HandleFilesystem(nil, testClientHost{}), acp.SessionUpdates(func(update acp.Update) error {
			updates <- update
			return nil
		}))
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		initialization := initializeClient(t, connection, acp.WorkspaceCapabilities(true, false, false))
		session, err := connection.NewSession(ctx, hostRoot+"/tmp")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent("read")}); err != nil {
			t.Fatal(err)
		}
		select {
		case update := <-updates:
			if update.Update.AgentMessageChunk == nil || update.Update.AgentMessageChunk.Content.Text == nil || update.Update.AgentMessageChunk.Content.Text.Text != "read" {
				t.Fatalf("unexpected filesystem-backed update: %+v", update)
			}
		default:
			t.Fatal("filesystem-backed update was not delivered")
		}
	})

	t.Run("not advertised", func(t *testing.T) {
		connection := startRuntime(t, testAgent{prompt: prompt}, HandleFilesystem(nil, testClientHost{}), nil)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		initialization := initializeClient(t, connection, acp.Capabilities{})
		session, err := connection.NewSession(ctx, hostRoot+"/tmp")
		if err != nil {
			t.Fatal(err)
		}
		_, err = connection.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent("read")})
		var rpcErr *acp.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 || rpcErr.Message != "client did not advertise fs/read_text_file support" {
			t.Fatalf("capability gate error = %v", err)
		}
	})
}

// TestCreateElicitationSendsModeFields guards the mode-specific fields an
// elicitation/create request must carry: requestedSchema for form mode, and
// url plus elicitationId for URL mode.
func TestCreateElicitationSendsModeFields(t *testing.T) {
	wantURL := "https://example.test/login"
	prompt := func(ctx context.Context, client Client, request schema.PromptRequest, _ SessionUpdater) (schema.PromptResponse, error) {
		session := &schema.ElicitationSessionScope{SessionID: request.SessionID}
		if _, err := client.CreateElicitation(ctx, schema.CreateElicitationRequest{
			Message: "Which ticket?",
			Form: &schema.ElicitationFormMode{
				RequestedSchema: schema.ElicitationSchema{Required: []string{"ticket"}},
				Session:         session,
			},
		}); err != nil {
			return schema.PromptResponse{}, err
		}
		if _, err := client.CreateElicitation(ctx, schema.CreateElicitationRequest{
			Message: "Log in to continue",
			URL:     &schema.ElicitationUrlMode{ElicitationID: "el_1", URL: wantURL, Session: session},
		}); err != nil {
			return schema.PromptResponse{}, err
		}
		return schema.PromptResponse{StopReason: schema.StopReasonEndTurn}, nil
	}
	var sent []json.RawMessage
	elicitation := acp.HandleElicitation(nil, func(context.Context, schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error) {
		return acp.CancelElicitation(), nil
	})
	host := func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		sent = append(sent, append(json.RawMessage(nil), raw...))
		return elicitation(ctx, method, raw)
	}
	connection := startRuntime(t, testAgent{prompt: prompt}, host, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initialization := initializeClient(t, connection, acp.Capabilities{Elicitation: acp.ElicitationClientCapabilities(true, true)})
	session, err := connection.NewSession(ctx, hostRoot+"/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent("ask")}); err != nil {
		t.Fatal(err)
	}
	if len(sent) != 2 {
		t.Fatalf("elicitation requests = %d, want 2", len(sent))
	}
	var form, url map[string]any
	if err := json.Unmarshal(sent[0], &form); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(sent[1], &url); err != nil {
		t.Fatal(err)
	}
	if requested, ok := form["requestedSchema"].(map[string]any); form["mode"] != "form" || !ok || requested["required"] == nil {
		t.Fatalf("form request = %s", sent[0])
	}
	if url["mode"] != "url" || url["url"] != wantURL || url["elicitationId"] != "el_1" {
		t.Fatalf("url request = %s", sent[1])
	}
}
