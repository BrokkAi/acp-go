package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/BrokkAi/acp-go/schema"
)

func TestGeneratedClientSurfaceWire(t *testing.T) {
	c, peer := pipeClient(t, nil, nil)
	requests := make(chan map[string]json.RawMessage, 16)
	go func() {
		defer close(requests)
		decoder := json.NewDecoder(peer)
		encoder := json.NewEncoder(peer)
		replies := map[string]string{
			schema.InitializeMethodName:    `{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"promptCapabilities":{"image":true,"audio":true,"embeddedContext":true},"mcpCapabilities":{"http":true,"sse":true},"sessionCapabilities":{"additionalDirectories":{},"resume":{},"close":{},"list":{},"delete":{}},"auth":{"logout":{}}}}`,
			schema.SessionNewMethodName:    `{"sessionId":"new-session"}`,
			schema.SessionPromptMethodName: `{"stopReason":"end_turn"}`,
			schema.SessionLoadMethodName:   `{"configOptions":[]}`,
			schema.SessionResumeMethodName: `{"configOptions":[]}`,
			schema.SessionListMethodName:   `{"sessions":[{"sessionId":"listed","cwd":"/tmp"}]}`,
			schema.SessionCloseMethodName:  `{}`,
			schema.SessionDeleteMethodName: `{}`,
			schema.LogoutMethodName:        `{}`,
		}
		for range replies {
			var request packet
			if err := decoder.Decode(&request); err != nil {
				return
			}
			var params map[string]json.RawMessage
			if len(request.Params) > 0 {
				if err := json.Unmarshal(request.Params, &params); err != nil {
					return
				}
			}
			params["method"] = json.RawMessage(fmt.Sprintf("%q", request.Method))
			requests <- params
			if err := encoder.Encode(packet{Version: "2.0", ID: request.ID, Result: json.RawMessage(replies[request.Method])}); err != nil {
				return
			}
		}
	}()

	ctx := context.Background()
	init, err := c.Initialize(ctx, WorkspaceCapabilities(false, false, false))
	if err != nil {
		t.Fatal(err)
	}
	session, err := c.NewSessionWithOptions(ctx, init, "/tmp", NewSessionOptions{
		AdditionalDirectories: []string{"/tmp/extra"},
		MCPServers: []schema.McpServer{
			NewStdioMCPServer("local", "/usr/bin/mcp", []string{"--stdio"}, map[string]string{"MCP_ENV": "1"}),
			NewHTTPMCPServer("remote", "https://example.test/mcp"),
			NewSSEMCPServer("events", "https://example.test/sse"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reason, err := c.PromptContent(ctx, init, session, []Content{
		NewTextContent("inspect"),
		NewResourceLinkContent("spec", "https://example.test/spec"),
		NewImageContent("image/png", "aGk="),
		NewAudioContent("audio/ogg", "aGk="),
		NewTextResourceContent("file:///tmp/readme", "hello"),
		NewBlobResourceContent("file:///tmp/binary", "aGk="),
	})
	if err != nil || reason != schema.StopReasonEndTurn {
		t.Fatalf("prompt: %v (%s)", err, reason)
	}
	if _, err = c.LoadSession(ctx, init, schema.LoadSessionRequest{
		SessionID: "old", Cwd: "/tmp", AdditionalDirectories: []string{"/tmp/extra"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.ResumeSession(ctx, init, schema.ResumeSessionRequest{SessionID: "old", Cwd: "/tmp"}); err != nil {
		t.Fatal(err)
	}
	cursor := "next"
	if _, err = c.ListSessions(ctx, init, schema.ListSessionsRequest{Cursor: &cursor, Cwd: strPtr("/tmp")}); err != nil {
		t.Fatal(err)
	}
	if err = c.CloseSession(ctx, init, "new-session"); err != nil {
		t.Fatal(err)
	}
	if err = c.DeleteSession(ctx, init, "old"); err != nil {
		t.Fatal(err)
	}
	if err = c.Logout(ctx, init); err != nil {
		t.Fatal(err)
	}

	var params map[string]json.RawMessage
	seen := map[string]map[string]json.RawMessage{}
	for range 9 {
		select {
		case params = <-requests:
			var method string
			if err := json.Unmarshal(params["method"], &method); err != nil {
				t.Fatal(err)
			}
			seen[method] = params
		default:
			t.Fatal("client did not issue every lifecycle request")
		}
	}
	assertJSONKey(t, seen[schema.SessionNewMethodName], "additionalDirectories", `["/tmp/extra"]`)
	assertJSONEqual(t, seen[schema.SessionNewMethodName], "mcpServers", `[
		{"name":"local","command":"/usr/bin/mcp","args":["--stdio"],"env":[{"name":"MCP_ENV","value":"1"}]},
		{"type":"http","name":"remote","url":"https://example.test/mcp","headers":null},
		{"type":"sse","name":"events","url":"https://example.test/sse","headers":null}
	]`)
	assertJSONEqual(t, seen[schema.SessionPromptMethodName], "prompt", `[
		{"type":"text","text":"inspect"},
		{"type":"resource_link","name":"spec","uri":"https://example.test/spec"},
		{"type":"image","mimeType":"image/png","data":"aGk="},
		{"type":"audio","mimeType":"audio/ogg","data":"aGk="},
		{"type":"resource","resource":{"uri":"file:///tmp/readme","text":"hello"}},
		{"type":"resource","resource":{"uri":"file:///tmp/binary","blob":"aGk="}}
	]`)
	assertJSONKey(t, seen[schema.SessionListMethodName], "cursor", `"next"`)
	assertJSONKey(t, seen[schema.SessionCloseMethodName], "sessionId", `"new-session"`)
	assertJSONKey(t, seen[schema.SessionDeleteMethodName], "sessionId", `"old"`)
}

func TestCapabilityGates(t *testing.T) {
	c, _ := pipeClient(t, nil, nil)
	ctx := context.Background()
	session := Session{SessionID: "session"}
	tests := []struct {
		name string
		call func() error
	}{
		{"additional directories", func() error {
			_, err := c.NewSessionWithOptions(ctx, Initialization{}, "/tmp", NewSessionOptions{AdditionalDirectories: []string{"/extra"}})
			return err
		}},
		{"http mcp", func() error {
			_, err := c.NewSessionWithOptions(ctx, Initialization{}, "/tmp", NewSessionOptions{MCPServers: []schema.McpServer{NewHTTPMCPServer("remote", "https://example.test")}})
			return err
		}},
		{"sse mcp", func() error {
			_, err := c.NewSessionWithOptions(ctx, Initialization{}, "/tmp", NewSessionOptions{MCPServers: []schema.McpServer{NewSSEMCPServer("events", "https://example.test")}})
			return err
		}},
		{"load", func() error {
			_, err := c.LoadSession(ctx, Initialization{}, schema.LoadSessionRequest{SessionID: "s", Cwd: "/tmp"})
			return err
		}},
		{"resume", func() error {
			_, err := c.ResumeSession(ctx, Initialization{}, schema.ResumeSessionRequest{SessionID: "s", Cwd: "/tmp"})
			return err
		}},
		{"close", func() error { return c.CloseSession(ctx, Initialization{}, "s") }},
		{"list", func() error {
			_, err := c.ListSessions(ctx, Initialization{}, schema.ListSessionsRequest{})
			return err
		}},
		{"delete", func() error { return c.DeleteSession(ctx, Initialization{}, "s") }},
		{"logout", func() error { return c.Logout(ctx, Initialization{}) }},
		{"image prompt", func() error {
			_, err := c.PromptContent(ctx, Initialization{}, session, []Content{NewImageContent("image/png", "aGk=")})
			return err
		}},
		{"audio prompt", func() error {
			_, err := c.PromptContent(ctx, Initialization{}, session, []Content{NewAudioContent("audio/ogg", "aGk=")})
			return err
		}},
		{"embedded resource", func() error {
			_, err := c.PromptContent(ctx, Initialization{}, session, []Content{NewTextResourceContent("file:///tmp/x", "hi")})
			return err
		}},
	}
	for _, test := range tests {
		if err := test.call(); err == nil {
			t.Fatalf("%s: gate allowed an unadvertised capability", test.name)
		}
	}
}

func TestSessionUpdatesDecodeGeneratedUnion(t *testing.T) {
	updates := make(chan Update, 1)
	notifications := SessionUpdates(func(update Update) error {
		updates <- update
		return nil
	})
	if err := notifications(schema.SessionUpdateMethodName, json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":12,"size":64}}`)); err != nil {
		t.Fatal(err)
	}
	update := <-updates
	if update.SessionID != "s" || update.Update.UsageUpdate == nil || update.Update.UsageUpdate.Used != 12 {
		t.Fatalf("unexpected update: %+v", update)
	}
}

func TestUpdateConstructorsSetDiscriminatorAndSession(t *testing.T) {
	call := schema.ToolCall{ToolCallID: "build", Title: "build project"}
	encoded, err := json.Marshal(NewToolCallUpdate("session", call))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"sessionId":"session","update":{"sessionUpdate":"tool_call","toolCallId":"build","title":"build project"}}`
	assertJSONEqual(t, map[string]json.RawMessage{"value": encoded}, "value", want)
}

func TestTypedErrorHelpers(t *testing.T) {
	if !IsAuthRequired(&RPCError{Code: ErrorCodeAuthRequired}) || IsAuthRequired(&RPCError{Code: ErrorCodeResourceNotFound}) {
		t.Fatal("auth error classification failed")
	}
	if !IsResourceNotFound(&RPCError{Code: ErrorCodeResourceNotFound}) || IsResourceNotFound(fmt.Errorf("ordinary")) {
		t.Fatal("resource classification failed")
	}
}

func assertJSONKey(t *testing.T, params map[string]json.RawMessage, key, want string) {
	t.Helper()
	if string(params[key]) != want {
		t.Fatalf("%s = %s, want %s", key, params[key], want)
	}
}

func strPtr(value string) *string { return &value }

func assertJSONEqual(t *testing.T, params map[string]json.RawMessage, key, want string) {
	t.Helper()
	var got, expected any
	if err := json.Unmarshal(params[key], &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("%s = %s, want %s", key, params[key], want)
	}
}
