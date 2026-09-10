package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/schema"
)

type recordedRequest struct {
	Method    string
	Params    map[string]json.RawMessage
	rawParams json.RawMessage
}

func singleRequestFixture(t *testing.T, result string) (*Connection, <-chan recordedRequest) {
	t.Helper()
	c, peer := pipeClient(t, nil, nil)
	requests := make(chan recordedRequest, 1)
	go func() {
		defer close(requests)
		var request packet
		if err := json.NewDecoder(peer).Decode(&request); err != nil {
			return
		}
		recorded := recordedRequest{Method: request.Method, rawParams: request.Params}
		if len(request.Params) > 0 {
			var params map[string]json.RawMessage
			if err := json.Unmarshal(request.Params, &params); err != nil {
				return
			}
			recorded.Params = params
		}
		if err := json.NewEncoder(peer).Encode(packet{
			Version: "2.0", ID: request.ID, Result: json.RawMessage(result),
		}); err != nil {
			return
		}
		requests <- recorded
	}()
	return c, requests
}

func receiveRequest(t *testing.T, requests <-chan recordedRequest) recordedRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for agent request")
		return recordedRequest{}
	}
}

func allCapabilities() Initialization {
	enabled := true
	return Initialization{AgentCapabilities: &schema.AgentCapabilities{
		LoadSession: &enabled,
		PromptCapabilities: &schema.PromptCapabilities{
			Image: &enabled, Audio: &enabled, EmbeddedContext: &enabled,
		},
		MCPCapabilities: &schema.McpCapabilities{HTTP: &enabled, SSE: &enabled},
		SessionCapabilities: &schema.SessionCapabilities{
			AdditionalDirectories: &schema.SessionAdditionalDirectoriesCapabilities{},
			Resume:                &schema.SessionResumeCapabilities{},
			Close:                 &schema.SessionCloseCapabilities{},
			List:                  &schema.SessionListCapabilities{},
			Delete:                &schema.SessionDeleteCapabilities{},
		},
		Auth: &schema.AgentAuthCapabilities{Logout: &schema.LogoutCapabilities{}},
	}}
}

func TestClientLifecycleMethodsHaveIndependentWireFixtures(t *testing.T) {
	ctx := context.Background()
	init := allCapabilities()
	session := Session{SessionID: "session"}

	t.Run("new session", func(t *testing.T) {
		c, requests := singleRequestFixture(t, `{"sessionId":"new-session"}`)
		created, err := c.NewSessionWithOptions(ctx, init, "/tmp", NewSessionOptions{
			AdditionalDirectories: []string{"/tmp/extra"},
			MCPServers: []schema.McpServer{
				NewStdioMCPServer("local", "/usr/bin/mcp", []string{"--stdio"}, map[string]string{"MCP_ENV": "1"}),
				NewHTTPMCPServer("remote", "https://example.test/mcp"),
				NewSSEMCPServer("events", "https://example.test/sse"),
			},
		})
		if err != nil || created.SessionID != "new-session" {
			t.Fatalf("new session: %v, %+v", err, created)
		}
		request := receiveRequest(t, requests)
		if request.Method != schema.SessionNewMethodName {
			t.Fatalf("method = %s", request.Method)
		}
		assertJSONKey(t, request.Params, "cwd", `"/tmp"`)
		assertJSONKey(t, request.Params, "additionalDirectories", `["/tmp/extra"]`)
		assertJSONEqual(t, request.Params, "mcpServers", `[
			{"name":"local","command":"/usr/bin/mcp","args":["--stdio"],"env":[{"name":"MCP_ENV","value":"1"}]},
			{"type":"http","name":"remote","url":"https://example.test/mcp","headers":null},
			{"type":"sse","name":"events","url":"https://example.test/sse","headers":null}
		]`)
	})

	t.Run("prompt", func(t *testing.T) {
		c, requests := singleRequestFixture(t, `{"stopReason":"end_turn"}`)
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
		request := receiveRequest(t, requests)
		if request.Method != schema.SessionPromptMethodName {
			t.Fatalf("method = %s", request.Method)
		}
		assertJSONKey(t, request.Params, "sessionId", `"session"`)
		assertJSONEqual(t, request.Params, "prompt", `[
			{"type":"text","text":"inspect"},
			{"type":"resource_link","name":"spec","uri":"https://example.test/spec"},
			{"type":"image","mimeType":"image/png","data":"aGk="},
			{"type":"audio","mimeType":"audio/ogg","data":"aGk="},
			{"type":"resource","resource":{"uri":"file:///tmp/readme","text":"hello"}},
			{"type":"resource","resource":{"uri":"file:///tmp/binary","blob":"aGk="}}
		]`)
	})

	t.Run("load session", func(t *testing.T) {
		c, requests := singleRequestFixture(t, `{"configOptions":[]}`)
		loaded, err := c.LoadSession(ctx, init, schema.LoadSessionRequest{
			SessionID: "old", Cwd: "/tmp", AdditionalDirectories: []string{"/tmp/extra"},
		})
		if err != nil || loaded.ConfigOptions == nil {
			t.Fatalf("load session: %v, %+v", err, loaded)
		}
		request := receiveRequest(t, requests)
		if request.Method != schema.SessionLoadMethodName {
			t.Fatalf("method = %s", request.Method)
		}
		assertJSONKey(t, request.Params, "sessionId", `"old"`)
		assertJSONKey(t, request.Params, "cwd", `"/tmp"`)
		assertJSONKey(t, request.Params, "additionalDirectories", `["/tmp/extra"]`)
		assertJSONKey(t, request.Params, "mcpServers", `[]`)
	})

	t.Run("resume session", func(t *testing.T) {
		c, requests := singleRequestFixture(t, `{"configOptions":[]}`)
		resumed, err := c.ResumeSession(ctx, init, schema.ResumeSessionRequest{SessionID: "old", Cwd: "/tmp"})
		if err != nil || resumed.ConfigOptions == nil {
			t.Fatalf("resume session: %v, %+v", err, resumed)
		}
		request := receiveRequest(t, requests)
		if request.Method != schema.SessionResumeMethodName {
			t.Fatalf("method = %s", request.Method)
		}
		assertJSONKey(t, request.Params, "sessionId", `"old"`)
		assertJSONKey(t, request.Params, "cwd", `"/tmp"`)
	})

	t.Run("list sessions", func(t *testing.T) {
		c, requests := singleRequestFixture(t, `{"sessions":[{"sessionId":"listed","cwd":"/tmp"}],"nextCursor":"more"}`)
		cursor := "next"
		listed, err := c.ListSessions(ctx, init, schema.ListSessionsRequest{Cursor: &cursor, Cwd: strPtr("/tmp")})
		if err != nil || len(listed.Sessions) != 1 || listed.NextCursor == nil || *listed.NextCursor != "more" {
			t.Fatalf("list sessions: %v, %+v", err, listed)
		}
		request := receiveRequest(t, requests)
		if request.Method != schema.SessionListMethodName {
			t.Fatalf("method = %s", request.Method)
		}
		assertJSONKey(t, request.Params, "cursor", `"next"`)
		assertJSONKey(t, request.Params, "cwd", `"/tmp"`)
	})

	t.Run("close session", func(t *testing.T) {
		c, requests := singleRequestFixture(t, `{}`)
		if err := c.CloseSession(ctx, init, "session"); err != nil {
			t.Fatal(err)
		}
		request := receiveRequest(t, requests)
		if request.Method != schema.SessionCloseMethodName {
			t.Fatalf("method = %s", request.Method)
		}
		assertJSONKey(t, request.Params, "sessionId", `"session"`)
	})

	t.Run("delete session", func(t *testing.T) {
		c, requests := singleRequestFixture(t, `{}`)
		if err := c.DeleteSession(ctx, init, "old"); err != nil {
			t.Fatal(err)
		}
		request := receiveRequest(t, requests)
		if request.Method != schema.SessionDeleteMethodName {
			t.Fatalf("method = %s", request.Method)
		}
		assertJSONKey(t, request.Params, "sessionId", `"old"`)
	})

	t.Run("logout", func(t *testing.T) {
		c, requests := singleRequestFixture(t, `{}`)
		if err := c.Logout(ctx, init); err != nil {
			t.Fatal(err)
		}
		request := receiveRequest(t, requests)
		if request.Method != schema.LogoutMethodName {
			t.Fatalf("method = %s", request.Method)
		}
		if string(request.rawParams) != "{}" {
			t.Fatalf("logout params = %s", request.rawParams)
		}
	})
}

func unexpectedRequestRecorder(t *testing.T, peer net.Conn) <-chan packet {
	t.Helper()
	requests := make(chan packet, 16)
	go func() {
		decoder := json.NewDecoder(peer)
		encoder := json.NewEncoder(peer)
		for {
			var request packet
			if err := decoder.Decode(&request); err != nil {
				return
			}
			requests <- request
			_ = encoder.Encode(packet{Version: "2.0", ID: request.ID, Result: json.RawMessage(`{}`)})
		}
	}()
	return requests
}

func TestSessionValidationRejectsBeforeWire(t *testing.T) {
	ctx := context.Background()
	c, peer := pipeClient(t, nil, nil)
	wireRequests := unexpectedRequestRecorder(t, peer)
	disabled := false
	full := allCapabilities()
	noCapabilities := Initialization{}
	falseLoad := Initialization{AgentCapabilities: &schema.AgentCapabilities{LoadSession: &disabled}}
	falseMCP := Initialization{AgentCapabilities: &schema.AgentCapabilities{
		MCPCapabilities: &schema.McpCapabilities{HTTP: &disabled, SSE: &disabled},
	}}
	falsePrompt := Initialization{AgentCapabilities: &schema.AgentCapabilities{
		PromptCapabilities: &schema.PromptCapabilities{
			Image: &disabled, Audio: &disabled, EmbeddedContext: &disabled,
		},
	}}
	session := Session{SessionID: "session"}

	tests := []struct {
		name string
		want string
		call func() error
	}{
		{"new relative cwd", "ACP path must be absolute", func() error {
			_, err := c.NewSessionWithOptions(ctx, full, "relative", NewSessionOptions{})
			return err
		}},
		{"additional directories unsupported", "agent did not advertise additionalDirectories support", func() error {
			_, err := c.NewSessionWithOptions(ctx, noCapabilities, "/tmp", NewSessionOptions{AdditionalDirectories: []string{"/extra"}})
			return err
		}},
		{"additional directory relative", `ACP path must be absolute: "extra"`, func() error {
			_, err := c.NewSessionWithOptions(ctx, full, "/tmp", NewSessionOptions{AdditionalDirectories: []string{"extra"}})
			return err
		}},
		{"mcp empty variant", "MCP server 0 has no transport variant", func() error {
			_, err := c.NewSessionWithOptions(ctx, full, "/tmp", NewSessionOptions{MCPServers: []schema.McpServer{{}}})
			return err
		}},
		{"stdio missing name", "MCP server 0 requires a name and command", func() error {
			_, err := c.NewSessionWithOptions(ctx, full, "/tmp", NewSessionOptions{MCPServers: []schema.McpServer{NewStdioMCPServer("", "/bin/mcp", nil, nil)}})
			return err
		}},
		{"stdio missing command", "MCP server 0 requires a name and command", func() error {
			_, err := c.NewSessionWithOptions(ctx, full, "/tmp", NewSessionOptions{MCPServers: []schema.McpServer{NewStdioMCPServer("local", "", nil, nil)}})
			return err
		}},
		{"http unsupported", "agent did not advertise HTTP MCP server support", func() error {
			_, err := c.NewSessionWithOptions(ctx, noCapabilities, "/tmp", NewSessionOptions{MCPServers: []schema.McpServer{NewHTTPMCPServer("remote", "https://example.test")}})
			return err
		}},
		{"http false", "agent did not advertise HTTP MCP server support", func() error {
			_, err := c.NewSessionWithOptions(ctx, falseMCP, "/tmp", NewSessionOptions{MCPServers: []schema.McpServer{NewHTTPMCPServer("remote", "https://example.test")}})
			return err
		}},
		{"http missing name", "HTTP MCP server 0 requires a name and URL", func() error {
			_, err := c.NewSessionWithOptions(ctx, full, "/tmp", NewSessionOptions{MCPServers: []schema.McpServer{NewHTTPMCPServer("", "https://example.test")}})
			return err
		}},
		{"http missing url", "HTTP MCP server 0 requires a name and URL", func() error {
			_, err := c.NewSessionWithOptions(ctx, full, "/tmp", NewSessionOptions{MCPServers: []schema.McpServer{NewHTTPMCPServer("remote", "")}})
			return err
		}},
		{"sse unsupported", "agent did not advertise SSE MCP server support", func() error {
			_, err := c.NewSessionWithOptions(ctx, noCapabilities, "/tmp", NewSessionOptions{MCPServers: []schema.McpServer{NewSSEMCPServer("events", "https://example.test")}})
			return err
		}},
		{"sse false", "agent did not advertise SSE MCP server support", func() error {
			_, err := c.NewSessionWithOptions(ctx, falseMCP, "/tmp", NewSessionOptions{MCPServers: []schema.McpServer{NewSSEMCPServer("events", "https://example.test")}})
			return err
		}},
		{"sse missing name", "SSE MCP server 0 requires a name and URL", func() error {
			_, err := c.NewSessionWithOptions(ctx, full, "/tmp", NewSessionOptions{MCPServers: []schema.McpServer{NewSSEMCPServer("", "https://example.test")}})
			return err
		}},
		{"sse missing url", "SSE MCP server 0 requires a name and URL", func() error {
			_, err := c.NewSessionWithOptions(ctx, full, "/tmp", NewSessionOptions{MCPServers: []schema.McpServer{NewSSEMCPServer("events", "")}})
			return err
		}},
		{"load unsupported", "agent did not advertise session/load support", func() error {
			_, err := c.LoadSession(ctx, noCapabilities, schema.LoadSessionRequest{SessionID: "old", Cwd: "/tmp"})
			return err
		}},
		{"load false", "agent did not advertise session/load support", func() error {
			_, err := c.LoadSession(ctx, falseLoad, schema.LoadSessionRequest{SessionID: "old", Cwd: "/tmp"})
			return err
		}},
		{"load relative cwd", `ACP path must be absolute: "relative"`, func() error {
			_, err := c.LoadSession(ctx, full, schema.LoadSessionRequest{SessionID: "old", Cwd: "relative"})
			return err
		}},
		{"load empty session", "session ID is required", func() error {
			_, err := c.LoadSession(ctx, full, schema.LoadSessionRequest{Cwd: "/tmp"})
			return err
		}},
		{"resume unsupported", "agent did not advertise session/resume support", func() error {
			_, err := c.ResumeSession(ctx, noCapabilities, schema.ResumeSessionRequest{SessionID: "old", Cwd: "/tmp"})
			return err
		}},
		{"resume relative cwd", `ACP path must be absolute: "relative"`, func() error {
			_, err := c.ResumeSession(ctx, full, schema.ResumeSessionRequest{SessionID: "old", Cwd: "relative"})
			return err
		}},
		{"resume empty session", "session ID is required", func() error {
			_, err := c.ResumeSession(ctx, full, schema.ResumeSessionRequest{Cwd: "/tmp"})
			return err
		}},
		{"close unsupported", "agent did not advertise session/close support", func() error {
			return c.CloseSession(ctx, noCapabilities, "session")
		}},
		{"close empty session", "session ID is required", func() error { return c.CloseSession(ctx, full, "") }},
		{"list unsupported", "agent did not advertise session/list support", func() error {
			_, err := c.ListSessions(ctx, noCapabilities, schema.ListSessionsRequest{})
			return err
		}},
		{"list relative cwd", `ACP path must be absolute: "relative"`, func() error {
			_, err := c.ListSessions(ctx, full, schema.ListSessionsRequest{Cwd: strPtr("relative")})
			return err
		}},
		{"delete unsupported", "agent did not advertise session/delete support", func() error {
			return c.DeleteSession(ctx, noCapabilities, "old")
		}},
		{"delete empty session", "session ID is required", func() error { return c.DeleteSession(ctx, full, "") }},
		{"logout unsupported", "agent did not advertise logout support", func() error { return c.Logout(ctx, noCapabilities) }},
		{"prompt empty session", "session ID is required", func() error {
			_, err := c.PromptContent(ctx, full, Session{}, []Content{NewTextContent("hi")})
			return err
		}},
		{"prompt empty variant", "prompt block 0 has 0 content variants, exactly one is required", func() error {
			_, err := c.PromptContent(ctx, full, session, []Content{{}})
			return err
		}},
		{"prompt multiple variants", "prompt block 0 has 2 content variants, exactly one is required", func() error {
			_, err := c.PromptContent(ctx, full, session, []Content{{Text: &schema.TextContent{Text: "hi"}, Image: &schema.ImageContent{MimeType: "image/png", Data: "aGk="}}})
			return err
		}},
		{"prompt image unsupported", "agent did not advertise image prompt support", func() error {
			_, err := c.PromptContent(ctx, noCapabilities, session, []Content{NewImageContent("image/png", "aGk=")})
			return err
		}},
		{"prompt image false", "agent did not advertise image prompt support", func() error {
			_, err := c.PromptContent(ctx, falsePrompt, session, []Content{NewImageContent("image/png", "aGk=")})
			return err
		}},
		{"prompt audio unsupported", "agent did not advertise audio prompt support", func() error {
			_, err := c.PromptContent(ctx, noCapabilities, session, []Content{NewAudioContent("audio/ogg", "aGk=")})
			return err
		}},
		{"prompt audio false", "agent did not advertise audio prompt support", func() error {
			_, err := c.PromptContent(ctx, falsePrompt, session, []Content{NewAudioContent("audio/ogg", "aGk=")})
			return err
		}},
		{"prompt embedded resource unsupported", "agent did not advertise embedded resource prompt support", func() error {
			_, err := c.PromptContent(ctx, noCapabilities, session, []Content{NewTextResourceContent("file:///tmp/x", "hi")})
			return err
		}},
		{"prompt embedded resource false", "agent did not advertise embedded resource prompt support", func() error {
			_, err := c.PromptContent(ctx, falsePrompt, session, []Content{NewTextResourceContent("file:///tmp/x", "hi")})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
	select {
	case request := <-wireRequests:
		t.Fatalf("validation wrote %s request to wire: %s", request.Method, request.Params)
	default:
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

func strPtr(value string) *string { return &value }
