package v2

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

func pipeClient(t *testing.T, handler acp.Handler, notifications acp.Notifications) (*Connection, *acp.Connection) {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	server := acp.Connect(peer, peer, handler, nil)
	client := Connect(local, local, nil, notifications)
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		_ = peer.Close()
	})
	return client, server
}

// readyClient is pipeClient with the connection initialized: the peer answers
// initialize with sessionInitialization and passes every other request to
// handler.
func readyClient(t *testing.T, handler acp.Handler, notifications acp.Notifications) (*Connection, *acp.Connection) {
	t.Helper()
	client, server := pipeClient(t, func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if method == schema.InitializeMethodName {
			return sessionInitialization(), nil
		}
		if handler == nil {
			return nil, &acp.RPCError{Code: -32601}
		}
		return handler(ctx, method, raw)
	}, notifications)
	if _, err := client.Initialize(context.Background(), Capabilities{}); err != nil {
		t.Fatal(err)
	}
	return client, server
}

// initializeRaw initializes a client whose peer is driven frame by frame:
// it answers the client's initialize request read from peer.
func initializeRaw(t *testing.T, client *Connection, peer io.ReadWriter, reader *bufio.Reader) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := client.Initialize(context.Background(), Capabilities{})
		done <- err
	}()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(line), &request); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(sessionInitialization())
	if _, err := io.WriteString(peer, `{"jsonrpc":"2.0","id":`+string(request.ID)+`,"result":`+string(result)+"}\n"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func isInitializationRequired(err error, method string) bool {
	var rpcErr *acp.RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == -32600 && rpcErr.Message == "Invalid request" &&
		string(rpcErr.Data) == "\"ACP initialization must complete before `"+method+"` can be used\""
}

// TestV2ClientFollowsTheInitiatorLifecycle matches the reference SDK's v2
// initiator: before initialize completes the client refuses its own requests
// and notifications without writing them, answers the agent's requests with
// -32600 without running the handler, drops the agent's notifications, and
// refuses an initialize from the agent.
func TestV2ClientFollowsTheInitiatorLifecycle(t *testing.T) {
	local, peer := net.Pipe()
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	handled := make(chan string, 4)
	notified := make(chan string, 4)
	client := Connect(local, local, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		handled <- method
		return map[string]bool{"ok": true}, nil
	}, func(method string, _ json.RawMessage) error {
		notified <- method
		return nil
	})
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	ctx := context.Background()
	reader := bufio.NewReader(peer)

	if err := client.Call(ctx, schema.SessionListMethodName, schema.ListSessionsRequest{}, nil); !isInitializationRequired(err, schema.SessionListMethodName) {
		t.Fatalf("request before initialize: %v", err)
	}
	if err := client.Notify(ctx, "_custom/note", map[string]any{}); !isInitializationRequired(err, "_custom/note") {
		t.Fatalf("notification before initialize: %v", err)
	}
	frames := `{"jsonrpc":"2.0","method":"session/update","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":1,"method":"fs/read_text_file","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":1}}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"protocolVersion":2}}` + "\n" +
		`{"jsonrpc":"2.0","id":4,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"a","version":"1"}}}` + "\n"
	go func() { _, _ = io.WriteString(peer, frames) }()
	// An initialize from the agent is negotiated before it is refused, as in
	// the reference SDK's incoming_initialize_request.
	for _, want := range []string{
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"Invalid request","data":"ACP initialization must complete before ` + "`fs/read_text_file`" + ` can be used"}}`,
		`{"jsonrpc":"2.0","id":2,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":1,"supported":[2]}}}`,
		`{"jsonrpc":"2.0","id":3,"error":{"code":-32602,"message":"Invalid params","data":{"error":"missing field ` + "`info`" + `","json":{"protocolVersion":2},"phase":"deserialization"}}}`,
		`{"jsonrpc":"2.0","id":4,"error":{"code":-32600,"message":"Invalid request","data":"ACP clients send ` + "`initialize`" + ` requests and ACP agents respond to them"}}`,
	} {
		line, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != want {
			t.Fatalf("answer = %q, %v, want %s", line, err, want)
		}
	}

	initializeRaw(t, client, peer, reader)
	go func() {
		_, _ = io.WriteString(peer, `{"jsonrpc":"2.0","method":"_custom/after","params":{}}`+"\n"+`{"jsonrpc":"2.0","id":3,"method":"fs/read_text_file","params":{}}`+"\n")
	}()
	if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != `{"jsonrpc":"2.0","id":3,"result":{"ok":true}}` {
		t.Fatalf("request after initialize = %q, %v", line, err)
	}
	if method := <-notified; method != "_custom/after" {
		t.Fatalf("notification before initialize reached the handler: %s", method)
	}
	if method := <-handled; method != "fs/read_text_file" {
		t.Fatalf("handled %s", method)
	}
	select {
	case method := <-handled:
		t.Fatalf("handler also ran for %s", method)
	default:
	}
}

func TestV2ConnectRejectsNullRequestIDs(t *testing.T) {
	local, peer := net.Pipe()
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	ran := make(chan string, 2)
	client := Connect(local, local, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		ran <- method
		return map[string]bool{"ok": true}, nil
	}, nil, acp.WithProtocolVersion(1))
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	reader := bufio.NewReader(peer)
	initializeRaw(t, client, peer, reader)
	// The v2 rule applies even when an earlier option names another version.
	if _, err := io.WriteString(peer, `{"jsonrpc":"2.0","id":null,"method":"fs/read_text_file","params":{}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"ACP protocol v2 request ids must be a string or an integer, not null"}}`; strings.TrimSpace(line) != want {
		t.Fatalf("answer = %s, want %s", line, want)
	}
	if _, err := io.WriteString(peer, `{"jsonrpc":"2.0","id":1,"method":"after","params":{}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if line, err = reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` {
		t.Fatalf("response after null id = %q, %v", line, err)
	}
	if method := <-ran; method != "after" {
		t.Fatalf("handler ran for %s", method)
	}
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func sessionInitialization() Initialization {
	return Initialization{
		ProtocolVersion: Version,
		Info:            schema.Implementation{Name: "test-agent", Version: "1.0"},
		Capabilities:    &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}},
	}
}

func TestV2LifecycleAndSessionUpdates(t *testing.T) {
	updates := make(chan Update, 1)
	prompts := make(chan schema.PromptRequest, 1)
	client, _ := pipeClient(t, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case schema.InitializeMethodName:
			request := decode[schema.InitializeRequest](t, raw)
			if request.ProtocolVersion != Version {
				t.Errorf("protocol version = %d", request.ProtocolVersion)
			}
			if request.Info.Name != "fixture-client" {
				t.Errorf("client name = %q", request.Info.Name)
			}
			return sessionInitialization(), nil
		case schema.SessionNewMethodName:
			request := decode[schema.NewSessionRequest](t, raw)
			if request.Cwd != hostRoot+"/fixture/workspace" {
				t.Errorf("cwd = %q", request.Cwd)
			}
			return schema.NewSessionResponse{SessionID: "v2-session"}, nil
		case schema.SessionPromptMethodName:
			request := decode[schema.PromptRequest](t, raw)
			prompts <- request
			return schema.PromptResponse{MessageID: "user-1"}, nil
		default:
			t.Errorf("unexpected method %q", method)
			return nil, &acp.RPCError{Code: -32601}
		}
	}, SessionUpdates(func(update Update) error {
		updates <- update
		return nil
	}))

	ctx := context.Background()
	initialization, err := client.InitializeWithInfo(ctx, Capabilities{}, ClientInfo{Name: "fixture-client", Version: "2.0"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSessionWithOptions(ctx, initialization, hostRoot+"/fixture/workspace", NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if session.SessionID != "v2-session" {
		t.Fatalf("session ID = %q", session.SessionID)
	}
	messageID, err := client.Prompt(ctx, initialization, session, "v2 hello")
	if err != nil {
		t.Fatal(err)
	}
	if messageID != "user-1" {
		t.Fatalf("user message ID = %q", messageID)
	}
	prompt := <-prompts
	if len(prompt.Prompt) != 1 || prompt.Prompt[0].Text == nil || prompt.Prompt[0].Text.Text != "v2 hello" {
		t.Fatalf("prompt = %+v", prompt.Prompt)
	}
}

func TestV2SessionUpdateAdapter(t *testing.T) {
	updates := make(chan Update, 1)
	_, server := readyClient(t, nil, SessionUpdates(func(value Update) error {
		updates <- value
		return nil
	}))
	raw := []byte(`{
		"sessionId":"s",
		"update":{
			"sessionUpdate":"agent_message",
			"messageId":"m",
			"content":[{"type":"text","text":"hello"}]
		}
	}`)
	if err := server.Notify(context.Background(), schema.SessionUpdateMethodName, json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-updates:
		if update.SessionID != "s" || update.Update.AgentMessage == nil || update.Update.AgentMessage.MessageID != "m" {
			t.Fatalf("update = %+v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("session update was not delivered")
	}
}

func TestV2CapabilityGates(t *testing.T) {
	session := Session{SessionID: "s"}
	image := []Content{NewImageContent("aGk=", "image/png")}
	tests := []struct {
		name string
		run  func(*Connection) error
	}{
		{
			name: "session surface",
			run: func(client *Connection) error {
				_, err := client.NewSession(context.Background(), hostRoot+"/repo")
				return err
			},
		},
		{
			name: "image prompt",
			run: func(client *Connection) error {
				_, err := client.PromptContent(context.Background(), sessionInitialization(), session, image)
				return err
			},
		},
		{
			name: "additional directories",
			run: func(client *Connection) error {
				_, err := client.NewSessionWithOptions(context.Background(), sessionInitialization(), hostRoot+"/repo", NewSessionOptions{
					AdditionalDirectories: []string{hostRoot + "/additional"},
				})
				return err
			},
		},
		{
			name: "session delete",
			run: func(client *Connection) error {
				return client.DeleteSession(context.Background(), sessionInitialization(), "s")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, _ := pipeClient(t, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
				t.Errorf("capability gate did not reject method %q before writing", method)
				return nil, &acp.RPCError{Code: -32601}
			}, nil)
			if err := test.run(client); err == nil {
				t.Fatal("expected capability rejection")
			}
		})
	}
}

// TestV2PromptContentValidation covers the draft-v2 facade's media-type and URI
// checks, which mirror the Rust semantic newtypes and run before the wire.
func TestV2PromptContentValidation(t *testing.T) {
	initialization := sessionInitialization()
	initialization.Capabilities.Session.Prompt = &schema.PromptCapabilities{
		Image:           &schema.PromptImageCapabilities{},
		Audio:           &schema.PromptAudioCapabilities{},
		EmbeddedContext: &schema.PromptEmbeddedContextCapabilities{},
	}
	session := Session{SessionID: "s"}
	tests := []struct {
		name    string
		content Content
		want    string
	}{
		{
			name:    "resource link relative uri",
			content: Content{ResourceLink: &schema.ResourceLink{Name: "name", URI: "relative/path"}},
			want:    "resource link URI must be an absolute URI",
		},
		{
			name:    "image invalid media type",
			content: Content{Image: &schema.ImageContent{Data: "aGk=", MimeType: "png"}},
			want:    "image content media type must be a media type",
		},
		{
			name:    "audio invalid media type",
			content: Content{Audio: &schema.AudioContent{Data: "aGk=", MimeType: "audio"}},
			want:    "audio content media type must be a media type",
		},
		{
			name: "embedded resource relative uri",
			content: Content{Resource: &schema.EmbeddedResource{Resource: schema.EmbeddedResourceResource{
				TextResourceContents: &schema.TextResourceContents{URI: "notes.txt", Text: "hi"},
			}}},
			want: "embedded resource URI must be an absolute URI",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, _ := pipeClient(t, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
				t.Errorf("validation wrote %s to the wire", method)
				return nil, &acp.RPCError{Code: -32601}
			}, nil)
			if _, err := client.PromptContent(context.Background(), initialization, session, []Content{test.content}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

// TestV2ClientRepeatsInitializeUntilAnotherRequest follows the reference
// SDK's initiator: a completed initialization may be repeated until the
// client sends another request, a failed repeat keeps the previous
// initialization, and a repeat after another request fails locally.
func TestV2ClientRepeatsInitializeUntilAnotherRequest(t *testing.T) {
	calls := 0
	client, _ := pipeClient(t, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case schema.InitializeMethodName:
			calls++
			if calls == 2 {
				return nil, &acp.RPCError{Code: -32603, Message: "repeat failed"}
			}
			var request schema.InitializeRequest
			_ = json.Unmarshal(raw, &request)
			initialization := sessionInitialization()
			initialization.Info.Name = request.Info.Name
			return initialization, nil
		case schema.SessionListMethodName:
			return schema.ListSessionsResponse{}, nil
		}
		return nil, &acp.RPCError{Code: -32601}
	}, nil)
	ctx := context.Background()
	recorded := func() string {
		initialization, ok := client.initializationSnapshot()
		if !ok {
			t.Fatal("connection lost its initialization")
		}
		return initialization.Info.Name
	}
	if _, err := client.InitializeWithInfo(ctx, Capabilities{}, ClientInfo{Name: "first", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.InitializeWithInfo(ctx, Capabilities{}, ClientInfo{Name: "failed", Version: "1"}); err == nil {
		t.Fatal("failed repeat succeeded")
	}
	if got := recorded(); got != "first" {
		t.Fatalf("after a failed repeat the initialization is %q, want first", got)
	}
	if _, err := client.InitializeWithInfo(ctx, Capabilities{}, ClientInfo{Name: "second", Version: "1"}); err != nil {
		t.Fatalf("repeat after a failed repeat: %v", err)
	}
	if got := recorded(); got != "second" {
		t.Fatalf("initialization = %q, want second", got)
	}
	if err := client.Call(ctx, schema.SessionListMethodName, schema.ListSessionsRequest{}, nil); err != nil {
		t.Fatal(err)
	}
	_, err := client.InitializeWithInfo(ctx, Capabilities{}, ClientInfo{Name: "third", Version: "1"})
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32600 ||
		string(rpcErr.Data) != `"ACP initialization cannot be repeated after any other request; reconnect to initialize again"` {
		t.Fatalf("repeat after another request: %v", err)
	}
	if calls != 3 {
		t.Fatalf("initialize reached the agent %d times, want 3", calls)
	}
}

func TestV2ClientCanRetryMalformedInitializeResponse(t *testing.T) {
	calls := 0
	client, _ := pipeClient(t, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method != schema.InitializeMethodName {
			return nil, &acp.RPCError{Code: -32601}
		}
		calls++
		if calls == 1 {
			return json.RawMessage(`{"protocolVersion":2,"Info":{"name":"x","version":"1"}}`), nil
		}
		return sessionInitialization(), nil
	}, nil)
	// As in the reference SDK's json_cast, a v2 response without info is a
	// -32700 with the serde reason; "Info" is not "info".
	_, err := client.InitializeWithInfo(context.Background(), Capabilities{}, ClientInfo{Name: "retry", Version: "1"})
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32700 ||
		string(rpcErr.Data) != `{"error":"missing field `+"`info`"+`","json":{"protocolVersion":2,"Info":{"name":"x","version":"1"}},"phase":"deserialization"}` {
		t.Fatalf("malformed initialize response error = %v", err)
	}
	initialization, err := client.InitializeWithInfo(context.Background(), Capabilities{}, ClientInfo{Name: "retry", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if initialization.Info.Name != "test-agent" {
		t.Fatalf("agent info = %+v", initialization.Info)
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestV2AuthLoginUsesAdvertisedAgentMethod(t *testing.T) {
	seen := make(chan schema.LoginAuthRequest, 1)
	client, _ := readyClient(t, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		if method != schema.AuthLoginMethodName {
			t.Errorf("method = %q", method)
			return nil, &acp.RPCError{Code: -32601}
		}
		seen <- decode[schema.LoginAuthRequest](t, raw)
		return schema.LoginAuthResponse{}, nil
	}, nil)
	initialization := sessionInitialization()
	initialization.AuthMethods = []schema.AuthMethod{{
		Agent: &schema.AuthMethodAgent{MethodID: "api-key", Name: "API Key"},
	}}
	if err := client.AuthLogin(context.Background(), initialization, "api-key"); err != nil {
		t.Fatal(err)
	}
	if request := <-seen; request.MethodID != "api-key" {
		t.Fatalf("method ID = %q", request.MethodID)
	}
	terminal := sessionInitialization()
	terminal.AuthMethods = []schema.AuthMethod{{
		Terminal: &schema.AuthMethodTerminal{MethodID: "console", Name: "Console"},
	}}
	if err := client.AuthLogin(context.Background(), terminal, "console"); err == nil {
		t.Fatal("terminal authentication method was sent over auth/login")
	}
}

// TestV2PromptRejectsMissingUserMessageID covers schema-v2.0.0-alpha.7, which
// makes PromptResponse.messageId required. An agent that omits it has not
// identified the inserted user message, so acceptance cannot be reported as a
// success.
func TestV2PromptRejectsMissingUserMessageID(t *testing.T) {
	client, _ := pipeClient(t, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case schema.InitializeMethodName:
			return sessionInitialization(), nil
		case schema.SessionNewMethodName:
			return schema.NewSessionResponse{SessionID: "v2-session"}, nil
		case schema.SessionPromptMethodName:
			return json.RawMessage(`{}`), nil
		default:
			return nil, &acp.RPCError{Code: -32601}
		}
	}, nil)

	ctx := context.Background()
	initialization, err := client.InitializeWithInfo(ctx, Capabilities{}, ClientInfo{Name: "fixture-client", Version: "2.0"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSessionWithOptions(ctx, initialization, hostRoot+"/fixture/workspace", NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	messageID, err := client.Prompt(ctx, initialization, session, "v2 hello")
	if err == nil {
		t.Fatalf("prompt accepted without a user message ID: %q", messageID)
	}
	// The response decoder rejects the missing required field before the
	// client checks for an empty ID.
	if !strings.Contains(err.Error(), `missing field "messageId"`) {
		t.Fatalf("prompt error = %v", err)
	}
}

// TestV2CancelRequestReusesTransportHelper covers the acceptance criterion that
// the typed cancellation helper is available on both the v1 and draft-v2
// transports: the v2 client embeds the shared root Connection, so its promoted
// CancelRequest must emit the same $/cancel_request notification.
func TestV2CancelRequestReusesTransportHelper(t *testing.T) {
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	client := Connect(local, local, nil, nil)
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	// Before initialization the reference initiator refuses even a cancel.
	if err := client.CancelRequest(context.Background(), "early"); !isInitializationRequired(err, schema.CancelRequestMethodName) {
		t.Fatalf("CancelRequest before initialize: %v", err)
	}
	initializeRaw(t, client, peer, bufio.NewReader(peer))

	seen := make(chan struct {
		method string
		id     json.RawMessage
	}, 1)
	go func() {
		var frame struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(peer).Decode(&frame); err != nil {
			return
		}
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if err := json.Unmarshal(frame.Params, &params); err != nil {
			return
		}
		seen <- struct {
			method string
			id     json.RawMessage
		}{method: frame.Method, id: params.RequestID}
	}()

	if err := client.CancelRequest(context.Background(), "v2-wait"); err != nil {
		t.Fatalf("CancelRequest: %v", err)
	}
	select {
	case frame := <-seen:
		if frame.method != schema.CancelRequestMethodName {
			t.Fatalf("method = %q", frame.method)
		}
		if string(frame.id) != `"v2-wait"` {
			t.Fatalf("requestId = %s", frame.id)
		}
	case <-time.After(time.Second):
		t.Fatal("missing $/cancel_request frame")
	}
}

// TestV2NewSessionUsesStoredInitialization covers issue #46: the convenience
// wrapper must use the initialization from Initialize, and a POSIX-absolute
// cwd must reach the wire from a Windows client.
func TestV2NewSessionUsesStoredInitialization(t *testing.T) {
	client, _ := pipeClient(t, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case schema.InitializeMethodName:
			return sessionInitialization(), nil
		case schema.SessionNewMethodName:
			request := decode[schema.NewSessionRequest](t, raw)
			if request.Cwd != "/" {
				t.Errorf("cwd = %q", request.Cwd)
			}
			return schema.NewSessionResponse{SessionID: "v2-session"}, nil
		default:
			return nil, &acp.RPCError{Code: -32601}
		}
	}, nil)

	ctx := context.Background()
	if _, err := client.NewSession(ctx, "/"); err == nil {
		t.Fatal("NewSession before Initialize was accepted")
	}
	if _, err := client.InitializeWithInfo(ctx, Capabilities{}, ClientInfo{Name: "fixture-client", Version: "2.0"}); err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession(ctx, "/")
	if err != nil {
		t.Fatal(err)
	}
	if session.SessionID != "v2-session" {
		t.Fatalf("session ID = %q", session.SessionID)
	}
}

// TestV2SessionPathsAcceptEitherPlatformAbsolutePaths covers issue #47 for the
// draft-v2 session entry points: every client-side validator must accept a path
// that is absolute on the agent's platform, whichever platform the client runs
// on, including the additional directories resume carries.
func TestV2SessionPathsAcceptEitherPlatformAbsolutePaths(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		cwd  string
	}{
		{name: "posix root", cwd: "/"},
		{name: "windows drive", cwd: `C:\agent\workspace`},
		{name: "windows unc", cwd: `\\server\share`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := readyClient(t, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
				switch method {
				case schema.SessionNewMethodName:
					request := decode[schema.NewSessionRequest](t, raw)
					if string(request.Cwd) != tc.cwd {
						t.Errorf("new cwd = %q", request.Cwd)
					}
					return schema.NewSessionResponse{SessionID: "v2-session"}, nil
				case schema.SessionResumeMethodName:
					request := decode[schema.ResumeSessionRequest](t, raw)
					if string(request.Cwd) != tc.cwd {
						t.Errorf("resume cwd = %q", request.Cwd)
					}
					if len(request.AdditionalDirectories) != 2 ||
						string(request.AdditionalDirectories[0]) != "/" ||
						string(request.AdditionalDirectories[1]) != `C:\agent\extra` {
						t.Errorf("additional directories = %v", request.AdditionalDirectories)
					}
					return schema.ResumeSessionResponse{}, nil
				case schema.SessionListMethodName:
					request := decode[schema.ListSessionsRequest](t, raw)
					if request.Cwd == nil || string(*request.Cwd) != tc.cwd {
						t.Errorf("list cwd = %v", request.Cwd)
					}
					return schema.ListSessionsResponse{}, nil
				default:
					return nil, &acp.RPCError{Code: -32601}
				}
			}, nil)

			initialization := sessionInitialization()
			initialization.Capabilities.Session.AdditionalDirectories = &schema.SessionAdditionalDirectoriesCapabilities{}
			if _, err := client.NewSessionWithOptions(ctx, initialization, tc.cwd, NewSessionOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := client.ResumeSession(ctx, initialization, schema.ResumeSessionRequest{
				SessionID:             "old",
				Cwd:                   schema.AbsolutePath(tc.cwd),
				AdditionalDirectories: []schema.AbsolutePath{"/", `C:\agent\extra`},
			}); err != nil {
				t.Fatal(err)
			}
			cwd := schema.AbsolutePath(tc.cwd)
			if _, err := client.ListSessions(ctx, initialization, schema.ListSessionsRequest{Cwd: &cwd}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
