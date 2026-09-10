package v2

import (
	"context"
	"encoding/json"
	"net"
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
			if request.Cwd != "/fixture/workspace" {
				t.Errorf("cwd = %q", request.Cwd)
			}
			return schema.NewSessionResponse{SessionID: "v2-session"}, nil
		case schema.SessionPromptMethodName:
			request := decode[schema.PromptRequest](t, raw)
			prompts <- request
			return schema.PromptResponse{}, nil
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
	session, err := client.NewSessionWithOptions(ctx, initialization, "/fixture/workspace", NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if session.SessionID != "v2-session" {
		t.Fatalf("session ID = %q", session.SessionID)
	}
	if err := client.Prompt(ctx, initialization, session, "v2 hello"); err != nil {
		t.Fatal(err)
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
	unsupportedHTTP := schema.McpServer{HTTP: &schema.McpServerHttp{Name: "remote", URL: "https://example.test"}}
	tests := []struct {
		name string
		run  func(*Connection) error
	}{
		{
			name: "session surface",
			run: func(client *Connection) error {
				_, err := client.NewSession(context.Background(), "/repo")
				return err
			},
		},
		{
			name: "image prompt",
			run: func(client *Connection) error {
				return client.PromptContent(context.Background(), sessionInitialization(), session, image)
			},
		},
		{
			name: "additional directories",
			run: func(client *Connection) error {
				_, err := client.NewSessionWithOptions(context.Background(), sessionInitialization(), "/repo", NewSessionOptions{
					AdditionalDirectories: []string{"/additional"},
				})
				return err
			},
		},
		{
			name: "http MCP server",
			run: func(client *Connection) error {
				_, err := client.NewSessionWithOptions(context.Background(), sessionInitialization(), "/repo", NewSessionOptions{
					MCPServers: []schema.McpServer{unsupportedHTTP},
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
