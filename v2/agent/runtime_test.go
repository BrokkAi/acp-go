package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

type echoAgent struct {
	cancelled chan schema.CancelSessionNotification
}

func (a *echoAgent) Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion: version,
		Info:            schema.Implementation{Name: "echo-v2", Version: "test"},
		Capabilities:    &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}},
	}, nil
}

func (a *echoAgent) NewSession(_ context.Context, _ Client, request schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	return schema.NewSessionResponse{SessionID: "v2-session"}, nil
}

func (a *echoAgent) Prompt(_ context.Context, _ Client, request schema.PromptRequest, updates SessionUpdater) (schema.PromptResponse, error) {
	message := "no text"
	if len(request.Prompt) == 1 && request.Prompt[0].Text != nil {
		message = request.Prompt[0].Text.Text
	}
	content := []schema.ContentBlock{{Text: &schema.TextContent{Text: message}}}
	for _, update := range []schema.SessionUpdate{
		{UserMessage: &schema.UserMessage{MessageID: "user", Content: schema.Nullable[[]schema.ContentBlock]{Set: true, Value: request.Prompt}}},
		{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}},
		{AgentMessage: &schema.AgentMessage{
			MessageID: "agent",
			Content:   schema.Nullable[[]schema.ContentBlock]{Set: true, Value: content},
		}},
		{StateUpdate: &schema.StateUpdate{Idle: &schema.IdleStateUpdate{}}},
	} {
		if err := updates.Update(update); err != nil {
			return schema.PromptResponse{}, err
		}
	}
	return schema.PromptResponse{MessageID: "user"}, nil
}

func (a *echoAgent) ListSessions(context.Context, Client, schema.ListSessionsRequest) (schema.ListSessionsResponse, error) {
	return schema.ListSessionsResponse{Sessions: []schema.SessionInfo{{SessionID: "v2-session", Cwd: hostRoot + "/repo"}}}, nil
}

func (a *echoAgent) ResumeSession(context.Context, Client, schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	return schema.ResumeSessionResponse{}, nil
}

func (a *echoAgent) CloseSession(context.Context, Client, schema.CloseSessionRequest) (schema.CloseSessionResponse, error) {
	return schema.CloseSessionResponse{}, nil
}

func (a *echoAgent) CancelSession(notification schema.CancelSessionNotification) error {
	a.cancelled <- notification
	return nil
}

func startRuntime(t *testing.T, implementation Agent, notifications acp.Notifications) *acpv2.Connection {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- New(implementation).Serve(context.Background(), peer, peer)
	}()
	client := acpv2.Connect(local, local, nil, notifications)
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.Close()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	return client
}

func TestRuntimeServesBaselineV2Lifecycle(t *testing.T) {
	updates := make(chan acpv2.Update, 4)
	cancelled := make(chan schema.CancelSessionNotification, 1)
	client := startRuntime(t, &echoAgent{cancelled: cancelled}, acpv2.SessionUpdates(func(update acpv2.Update) error {
		updates <- update
		return nil
	}))
	ctx := context.Background()
	initialization, err := client.InitializeWithInfo(ctx, acpv2.Capabilities{}, acpv2.ClientInfo{Name: "test-client", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSessionWithOptions(ctx, initialization, hostRoot+"/repo", acpv2.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := client.Prompt(ctx, initialization, session, "hello v2")
	if err != nil {
		t.Fatal(err)
	}
	if messageID != "user" {
		t.Fatalf("user message ID = %q", messageID)
	}
	var sawAgentMessage, sawIdle bool
	for i := 0; i < 4; i++ {
		select {
		case update := <-updates:
			sawAgentMessage = sawAgentMessage || update.Update.AgentMessage != nil
			sawIdle = sawIdle || (update.Update.StateUpdate != nil && update.Update.StateUpdate.Idle != nil)
		case <-time.After(time.Second):
			t.Fatal("missing session update")
		}
	}
	if !sawAgentMessage || !sawIdle {
		t.Fatalf("missing expected updates: agent=%t idle=%t", sawAgentMessage, sawIdle)
	}
	if err := client.CancelSession(ctx, session.SessionID); err != nil {
		t.Fatal(err)
	}
	select {
	case notification := <-cancelled:
		if notification.SessionID != session.SessionID {
			t.Fatalf("cancelled session = %q", notification.SessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel notification was not delivered")
	}
	list, err := client.ListSessions(ctx, initialization, schema.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != "v2-session" {
		t.Fatalf("sessions = %+v", list.Sessions)
	}
	if _, err := client.ResumeSession(ctx, initialization, schema.ResumeSessionRequest{
		SessionID: session.SessionID, Cwd: hostRoot + "/repo",
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseSession(ctx, initialization, session.SessionID); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRejectsInvalidAdvertisements(t *testing.T) {
	t.Run("missing sessions", func(t *testing.T) {
		client := startRuntime(t, &noSessionAgent{echoAgent: &echoAgent{}}, nil)
		_, err := client.Initialize(context.Background(), acpv2.Capabilities{})
		if err == nil {
			t.Fatal("runtime accepted an agent without session capabilities")
		}
	})
}

// recordingAgent records each initialize request it receives.
type recordingAgent struct {
	*echoAgent
	calls    atomic.Int32
	requests chan schema.InitializeRequest
}

func newRecordingAgent() *recordingAgent {
	return &recordingAgent{echoAgent: &echoAgent{cancelled: make(chan schema.CancelSessionNotification, 1)}, requests: make(chan schema.InitializeRequest, 4)}
}

func (a *recordingAgent) Initialize(ctx context.Context, client Client, request schema.InitializeRequest) (schema.InitializeResponse, error) {
	a.calls.Add(1)
	a.requests <- request
	return a.echoAgent.Initialize(ctx, client, request)
}

func TestRuntimeRejectsV1InitializeOnV2Endpoint(t *testing.T) {
	implementation := newRecordingAgent()
	client := startRawClient(t, implementation)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, params := range []string{
		`{"protocolVersion":1,"clientCapabilities":{}}`,
		`{"protocolVersion":0}`,
	} {
		err := client.Connection.Call(ctx, schema.InitializeMethodName, json.RawMessage(params), nil)
		var rpcErr *acp.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32022 || rpcErr.Message != "Unsupported protocol version" {
			t.Fatalf("%s: error = %v, want -32022", params, err)
		}
		data, ok := acpv2.UnsupportedProtocolVersionData(err)
		if !ok || len(data.Supported) != 1 || data.Supported[0] != 2 {
			t.Fatalf("%s: error data = %s", params, rpcErr.Data)
		}
	}
	err := client.Connection.Call(ctx, schema.InitializeMethodName, json.RawMessage(`{"protocolVersion":1}`), nil)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || string(rpcErr.Data) != `{"requested":1,"supported":[2]}` {
		t.Fatalf("error = %v, data %s", err, rpcErr.Data)
	}
	if calls := implementation.calls.Load(); calls != 0 {
		t.Fatalf("Initialize calls = %d, want 0", calls)
	}
	// The rejection leaves the connection uninitialized: v2 still works.
	if _, err := client.Initialize(ctx, acpv2.Capabilities{}); err != nil {
		t.Fatalf("initialize after rejection: %v", err)
	}
}

func TestRuntimeAcceptsNewerVersionAsV2(t *testing.T) {
	for _, test := range []struct {
		name   string
		params string
		check  func(*testing.T, schema.InitializeRequest)
	}{
		{
			name:   "with info",
			params: `{"protocolVersion":3,"info":{"name":"future-client","version":"3.0.0"},"_meta":{"kept":true},"_futureField":true}`,
			check: func(t *testing.T, request schema.InitializeRequest) {
				if request.Info.Name != "future-client" || request.Info.Version != "3.0.0" {
					t.Fatalf("info = %+v", request.Info)
				}
				if kept, _ := request.Meta.Value.Get("kept"); !request.Meta.Set || kept != true {
					t.Fatalf("_meta = %+v", request.Meta)
				}
			},
		},
		{
			name: "without info",
			params: `{"protocolVersion":65535,"info":{"name":"future-client"},` +
				`"capabilities":{"auth":"malformed","elicitation":{},"_futureCapability":{"dropped":true}},"_meta":"malformed"}`,
			check: func(t *testing.T, request schema.InitializeRequest) {
				if request.Info.Name != "unknown" || request.Info.Version != "unknown" {
					t.Fatalf("info = %+v", request.Info)
				}
				if request.Capabilities == nil || request.Capabilities.Auth != nil || request.Capabilities.Elicitation == nil {
					t.Fatalf("capabilities = %+v", request.Capabilities)
				}
				if request.Meta.Set {
					t.Fatalf("_meta = %+v", request.Meta)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			implementation := newRecordingAgent()
			client := startRawClient(t, implementation)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var response schema.InitializeResponse
			if err := client.Connection.Call(ctx, schema.InitializeMethodName, json.RawMessage(test.params), &response); err != nil {
				t.Fatal(err)
			}
			if response.ProtocolVersion != version {
				t.Fatalf("selected protocol = %d", response.ProtocolVersion)
			}
			request := <-implementation.requests
			if request.ProtocolVersion != version {
				t.Fatalf("agent saw protocol %d", request.ProtocolVersion)
			}
			test.check(t, request)
		})
	}
}

func TestRuntimeRejectsMalformedInitialize(t *testing.T) {
	for _, test := range []struct {
		params string
		detail string
	}{
		{`{"info":{"name":"c","version":"1"}}`, "initialize.protocolVersion must be a valid ACP protocol version"},
		{`{"protocolVersion":null}`, "initialize.protocolVersion must be a valid ACP protocol version"},
		{`{"protocolVersion":"2"}`, "initialize.protocolVersion must be a valid ACP protocol version"},
		{`{"protocolVersion":2.5}`, "initialize.protocolVersion must be a valid ACP protocol version"},
		{`{"protocolVersion":100000}`, "initialize.protocolVersion must be a valid ACP protocol version"},
		// An exact v2 request must carry an info that parses. Like the
		// reference responder, the data is the deserialization failure.
		{`{"protocolVersion":2,"info":{"name":"client"}}`, `{"error":"missing field ` + "`version`" + `","json":{"protocolVersion":2,"info":{"name":"client"}},"phase":"deserialization"}`},
		{`{"protocolVersion":2}`, `{"error":"missing field ` + "`info`" + `","json":{"protocolVersion":2},"phase":"deserialization"}`},
		{`{"protocolVersion":2,"info":"bad"}`, `{"error":"invalid type: string \"bad\", expected struct Implementation","json":{"protocolVersion":2,"info":"bad"},"phase":"deserialization"}`},
		{`{"protocolVersion":2,"info":{"name":5,"version":"1"}}`, `{"error":"invalid type: integer ` + "`5`" + `, expected a string","json":{"protocolVersion":2,"info":{"name":5,"version":"1"}},"phase":"deserialization"}`},
	} {
		t.Run(test.params, func(t *testing.T) {
			implementation := newRecordingAgent()
			client := startRawClient(t, implementation)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := client.Connection.Call(ctx, schema.InitializeMethodName, json.RawMessage(test.params), nil)
			var rpcErr *acp.RPCError
			if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 || rpcErr.Message != "Invalid params" {
				t.Fatalf("error = %v, want -32602", err)
			}
			if strings.HasPrefix(test.detail, "{") {
				if string(rpcErr.Data) != test.detail {
					t.Fatalf("error data = %s, want %s", rpcErr.Data, test.detail)
				}
			} else {
				var data string
				if err := json.Unmarshal(rpcErr.Data, &data); err != nil || data != test.detail {
					t.Fatalf("error data = %s, want %q", rpcErr.Data, test.detail)
				}
			}
			if calls := implementation.calls.Load(); calls != 0 {
				t.Fatalf("Initialize calls = %d, want 0", calls)
			}
			requireInvalidRequest(t, listSessionsCall(ctx, client), sessionListBeforeInit)
		})
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
	sessionListBeforeInit   = "ACP initialization must complete before `session/list` can be used"
)

func listSessionsCall(ctx context.Context, client caller) error {
	return client.Call(ctx, schema.SessionListMethodName, schema.ListSessionsRequest{}, nil)
}

type initializeOutcome struct {
	response schema.InitializeResponse
	err      error
}

// gatedInitializeAgent blocks every Initialize call until the test releases
// it with an outcome, so other requests can be sent while it is running.
type gatedInitializeAgent struct {
	*echoAgent
	calls   atomic.Int32
	entered chan struct{}
	release chan initializeOutcome
}

func newGatedInitializeAgent() *gatedInitializeAgent {
	return &gatedInitializeAgent{
		echoAgent: &echoAgent{cancelled: make(chan schema.CancelSessionNotification, 1)},
		entered:   make(chan struct{}, 2),
		release:   make(chan initializeOutcome),
	}
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

func initializeSucceeds() initializeOutcome {
	return initializeOutcome{response: schema.InitializeResponse{
		ProtocolVersion: version,
		Info:            schema.Implementation{Name: "gated-v2", Version: "test"},
		Capabilities:    &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}},
	}}
}

var v2InitializeRequest = schema.InitializeRequest{
	ProtocolVersion: version,
	Info:            schema.Implementation{Name: "client", Version: "1"},
}

// startBlockedInitialize sends an initialize request and returns once the
// agent's Initialize handler is running and blocked. The returned channel
// yields the request's outcome after the handler is released.
func startBlockedInitialize(ctx context.Context, t *testing.T, client caller, implementation *gatedInitializeAgent) <-chan error {
	t.Helper()
	result := make(chan error, 1)
	go func() {
		result <- client.Call(ctx, schema.InitializeMethodName, v2InitializeRequest, nil)
	}()
	select {
	case <-implementation.entered:
	case <-ctx.Done():
		t.Fatal("initialize did not reach the agent")
	}
	return result
}

// requireInitializeInProgress checks the refusals a connection gives while an
// initialize is still running: another initialize, and any other request.
func requireInitializeInProgress(ctx context.Context, t *testing.T, client caller) {
	t.Helper()
	requireInvalidRequest(t, client.Call(ctx, schema.InitializeMethodName, v2InitializeRequest, nil), initializeInProgress)
	requireInvalidRequest(t, listSessionsCall(ctx, client), sessionListBeforeInit)
}

func TestRuntimeRefusesRequestsBeforeAndRepeatsAfterInitialize(t *testing.T) {
	implementation := newRecordingAgent()
	client := startRawClient(t, implementation)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	requireInvalidRequest(t, listSessionsCall(ctx, client), sessionListBeforeInit)
	if _, err := client.Initialize(ctx, acpv2.Capabilities{}); err != nil {
		t.Fatal(err)
	}
	// Notifications do not close the repeat window.
	if err := client.Notify(ctx, schema.SessionCancelMethodName, schema.CancelSessionNotification{SessionID: "none"}); err != nil {
		t.Fatal(err)
	}
	// A repeat before any other request is negotiated like the first one,
	// including the newer-version leniency, and reaches the agent.
	for _, params := range []string{
		`{"protocolVersion":2,"info":{"name":"client","version":"2"}}`,
		`{"protocolVersion":3}`,
	} {
		var response schema.InitializeResponse
		if err := client.Connection.Call(ctx, schema.InitializeMethodName, json.RawMessage(params), &response); err != nil {
			t.Fatalf("repeated initialize %s: %v", params, err)
		}
		if response.ProtocolVersion != version {
			t.Fatalf("repeat selected %d", response.ProtocolVersion)
		}
	}
	if calls := implementation.calls.Load(); calls != 3 {
		t.Fatalf("Initialize calls = %d, want 3", calls)
	}
	// The first other request closes the window; the connection stays ready.
	if err := listSessionsCall(ctx, client); err != nil {
		t.Fatal(err)
	}
	requireInvalidRequest(t, client.Call(ctx, schema.InitializeMethodName, v2InitializeRequest, nil), initializeNotRepeatable)
	if err := listSessionsCall(ctx, client); err != nil {
		t.Fatalf("session/list after refused repeat: %v", err)
	}
	if calls := implementation.calls.Load(); calls != 3 {
		t.Fatalf("Initialize calls = %d, want 3", calls)
	}
}

func TestRuntimeFailedRepeatKeepsPreviousInitialization(t *testing.T) {
	implementation := newGatedInitializeAgent()
	client := startRawClient(t, implementation)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first := startBlockedInitialize(ctx, t, client, implementation)
	implementation.release <- initializeSucceeds()
	if err := <-first; err != nil {
		t.Fatal(err)
	}

	// A repeat in progress refuses other requests and a concurrent repeat.
	repeat := startBlockedInitialize(ctx, t, client, implementation)
	requireInitializeInProgress(ctx, t, client)
	implementation.release <- initializeOutcome{err: errors.New("repeat failed")}
	if err := <-repeat; err == nil {
		t.Fatal("failed repeat returned no error")
	}

	// A repeat requesting v1 is answered like a first initialize, without
	// reaching the agent, and a malformed repeat is refused before it begins.
	err := client.Connection.Call(ctx, schema.InitializeMethodName, json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{}}`), nil)
	if data, ok := acpv2.UnsupportedProtocolVersionData(err); !ok || data.Requested != 1 || len(data.Supported) != 1 || data.Supported[0] != 2 {
		t.Fatalf("v1 repeat error = %v", err)
	}
	err = client.Connection.Call(ctx, schema.InitializeMethodName, json.RawMessage(`{"protocolVersion":2}`), nil)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 {
		t.Fatalf("malformed repeat error = %v", err)
	}
	if calls := implementation.calls.Load(); calls != 2 {
		t.Fatalf("Initialize calls = %d, want 2", calls)
	}

	// The window is still open after the failures.
	again := startBlockedInitialize(ctx, t, client, implementation)
	implementation.release <- initializeSucceeds()
	if err := <-again; err != nil {
		t.Fatalf("repeat after failures: %v", err)
	}
	if err := listSessionsCall(ctx, client); err != nil {
		t.Fatalf("session/list after repeat: %v", err)
	}
}

func TestRuntimeRejectsConcurrentInitialize(t *testing.T) {
	implementation := newGatedInitializeAgent()
	client := startRuntime(t, implementation, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	first := startBlockedInitialize(ctx, t, client, implementation)
	requireInitializeInProgress(ctx, t, client)
	implementation.release <- initializeSucceeds()
	if err := <-first; err != nil {
		t.Fatalf("first initialize: %v", err)
	}
	if calls := implementation.calls.Load(); calls != 1 {
		t.Fatalf("Initialize calls = %d, want 1", calls)
	}
	if err := listSessionsCall(ctx, client); err != nil {
		t.Fatal(err)
	}
	requireInvalidRequest(t, client.Call(ctx, schema.InitializeMethodName, v2InitializeRequest, nil), initializeNotRepeatable)
}

func TestRuntimeAllowsInitializeRetryAfterFailure(t *testing.T) {
	missingCapabilities := initializeSucceeds()
	missingCapabilities.response.Capabilities = nil
	for _, test := range []struct {
		name    string
		failure initializeOutcome
	}{
		{name: "handler error", failure: initializeOutcome{err: errors.New("initialize failed")}},
		{name: "missing capabilities", failure: missingCapabilities},
	} {
		t.Run(test.name, func(t *testing.T) {
			implementation := newGatedInitializeAgent()
			client := startRuntime(t, implementation, nil)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			first := startBlockedInitialize(ctx, t, client, implementation)
			requireInitializeInProgress(ctx, t, client)
			implementation.release <- test.failure
			if err := <-first; err == nil {
				t.Fatal("failed initialize returned no error")
			}
			requireInvalidRequest(t, listSessionsCall(ctx, client), sessionListBeforeInit)

			retry := startBlockedInitialize(ctx, t, client, implementation)
			implementation.release <- initializeSucceeds()
			if err := <-retry; err != nil {
				t.Fatalf("retried initialize: %v", err)
			}
			if calls := implementation.calls.Load(); calls != 2 {
				t.Fatalf("Initialize calls = %d, want 2", calls)
			}
			if err := listSessionsCall(ctx, client); err != nil {
				t.Fatalf("session/list after retried initialize: %v", err)
			}
		})
	}
}

func TestRuntimeGatesClientElicitationByCapability(t *testing.T) {
	client := startRuntime(t, &gatingAgent{echoAgent: &echoAgent{}}, nil)
	ctx := context.Background()
	initialization, err := client.InitializeWithInfo(ctx, acpv2.Capabilities{}, acpv2.ClientInfo{Name: "test", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSessionWithOptions(ctx, initialization, hostRoot+"/repo", acpv2.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Prompt(ctx, initialization, session, "hello")
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("prompt error = %v", err)
	}
}

type noSessionAgent struct{ *echoAgent }

func (a *noSessionAgent) Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{ProtocolVersion: version}, nil
}

type gatingAgent struct{ *echoAgent }

func (a *gatingAgent) Prompt(ctx context.Context, client Client, request schema.PromptRequest, _ SessionUpdater) (schema.PromptResponse, error) {
	_, err := client.CreateElicitation(ctx, schema.CreateElicitationRequest{
		Message: "Log in to continue",
		URL: &schema.ElicitationUrlMode{
			ElicitationID: "el_1",
			URL:           "https://example.test/login",
			Session:       &schema.ElicitationSessionScope{SessionID: request.SessionID},
		},
	})
	if err != nil {
		return schema.PromptResponse{}, err
	}
	return schema.PromptResponse{MessageID: "user"}, nil
}

type unidentifiedPromptAgent struct{ *echoAgent }

func (a *unidentifiedPromptAgent) Prompt(context.Context, Client, schema.PromptRequest, SessionUpdater) (schema.PromptResponse, error) {
	return schema.PromptResponse{}, nil
}

// TestRuntimeRejectsPromptResponseWithoutUserMessageID keeps an invalid
// acceptance off the wire. schema-v2.0.0-alpha.7 requires
// PromptResponse.messageId, so an implementation that leaves it empty must
// fail loudly instead of sending "messageId": "".
func TestRuntimeRejectsPromptResponseWithoutUserMessageID(t *testing.T) {
	client := startRuntime(t, &unidentifiedPromptAgent{&echoAgent{}}, nil)
	ctx := context.Background()
	initialization, err := client.InitializeWithInfo(ctx, acpv2.Capabilities{}, acpv2.ClientInfo{Name: "test-client", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSessionWithOptions(ctx, initialization, hostRoot+"/repo", acpv2.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Prompt(ctx, initialization, session, "hello")
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32603 {
		t.Fatalf("prompt error = %v", err)
	}
	if !strings.Contains(rpcErr.Message, "user message ID") {
		t.Fatalf("prompt error message = %q", rpcErr.Message)
	}
}
