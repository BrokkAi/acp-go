package v2

import (
	"context"
	"encoding/json"
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
	_, server := pipeClient(t, nil, SessionUpdates(func(value Update) error {
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

func TestV2ClientAllowsOnlyOneSuccessfulInitialize(t *testing.T) {
	client, server := pipeClient(t, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		if method != schema.InitializeMethodName {
			return nil, &acp.RPCError{Code: -32601}
		}
		return sessionInitialization(), nil
	}, nil)
	_ = server
	if _, err := client.InitializeWithInfo(context.Background(), Capabilities{}, ClientInfo{Name: "once", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.InitializeWithInfo(context.Background(), Capabilities{}, ClientInfo{Name: "twice", Version: "1"}); err == nil ||
		!strings.Contains(err.Error(), "may only be initialized once") {
		t.Fatalf("duplicate initialize error = %v", err)
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
			return schema.InitializeResponse{ProtocolVersion: Version}, nil
		}
		return sessionInitialization(), nil
	}, nil)
	if _, err := client.InitializeWithInfo(context.Background(), Capabilities{}, ClientInfo{Name: "retry", Version: "1"}); err == nil {
		t.Fatal("malformed initialize response accepted")
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
	client, _ := pipeClient(t, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
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
	if !strings.Contains(err.Error(), "user message ID") {
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
			client, _ := pipeClient(t, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
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
