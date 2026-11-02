package mcp_test

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/mcp"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

// paramsFixture is fixture recording each request's params.
func paramsFixture(t *testing.T) (*acp.Connection, chan json.RawMessage) {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	connection := acp.Connect(local, local, nil, nil)
	requests := make(chan json.RawMessage, 8)
	go func() {
		decoder := json.NewDecoder(peer)
		encoder := json.NewEncoder(peer)
		for {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Params json.RawMessage `json:"params"`
			}
			if err := decoder.Decode(&request); err != nil {
				return
			}
			requests <- request.Params
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"sessionId": "s"}})
		}
	}()
	t.Cleanup(func() {
		_ = peer.Close()
		_ = connection.Close()
	})
	return connection, requests
}

func systemPromptInitialization(capability *schema.SessionSystemPromptCapabilities) schema.InitializeResponse {
	return schema.InitializeResponse{AgentCapabilities: &schema.AgentCapabilities{
		MCPCapabilities: &schema.McpCapabilities{ACP: boolPtr(true)},
		SessionCapabilities: &schema.SessionCapabilities{
			Resume:       &schema.SessionResumeCapabilities{},
			Fork:         &schema.SessionForkCapabilities{},
			SystemPrompt: capability,
		},
	}}
}

func TestSessionHelpersCombineMCPServersWithASystemPrompt(t *testing.T) {
	ctx := context.Background()
	connection, requests := paramsFixture(t)
	initialization := systemPromptInitialization(&schema.SessionSystemPromptCapabilities{})
	prompt := &schema.SystemPrompt{Mode: schema.SystemPromptModeAppend, Content: []schema.ContentBlock{{Text: &schema.TextContent{Text: "Use the docs server."}}}}
	servers := []schema.McpServer{{ACP: &schema.McpServerAcp{Name: "docs", ServerID: "docs-1"}}}

	if _, err := mcp.NewSession(ctx, connection, initialization, "/repo", mcp.NewSessionOptions{Servers: servers, SystemPrompt: prompt}); err != nil {
		t.Fatal(err)
	}
	if _, err := mcp.ResumeSession(ctx, connection, initialization, schema.ResumeSessionRequest{SessionID: "s", Cwd: "/repo", MCPServers: servers, SystemPrompt: prompt}); err != nil {
		t.Fatal(err)
	}
	if _, err := mcp.ForkSession(ctx, connection, initialization, schema.ForkSessionRequest{SessionID: "s", Cwd: "/repo", MCPServers: servers, SystemPrompt: prompt}); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		params := string(<-requests)
		if !strings.Contains(params, `"mcpServers":[{"name":"docs","serverId":"docs-1","type":"acp"}]`) ||
			!strings.Contains(params, `"systemPrompt":{"content":[{"text":"Use the docs server.","type":"text"}],"mode":"append"}`) {
			t.Fatalf("params = %s", params)
		}
	}
}

func TestSessionHelpersRejectUnsupportedSystemPromptsBeforeWriting(t *testing.T) {
	ctx := context.Background()
	connection, requests := paramsFixture(t)
	text := []schema.ContentBlock{{Text: &schema.TextContent{Text: "x"}}}
	for name, test := range map[string]struct {
		capability *schema.SessionSystemPromptCapabilities
		prompt     schema.SystemPrompt
		want       string
	}{
		"capability absent":              {nil, schema.SystemPrompt{Mode: schema.SystemPromptModeAppend, Content: text}, "did not advertise systemPrompt support"},
		"override without mode.override": {&schema.SessionSystemPromptCapabilities{}, schema.SystemPrompt{Mode: schema.SystemPromptModeOverride, Content: text}, "override mode"},
		"unknown mode":                   {&schema.SessionSystemPromptCapabilities{}, schema.SystemPrompt{Mode: "replace", Content: text}, "unsupported systemPrompt mode"},
		"non-text content": {&schema.SessionSystemPromptCapabilities{}, schema.SystemPrompt{Mode: schema.SystemPromptModeAppend, Content: []schema.ContentBlock{
			{Image: &schema.ImageContent{Data: "AA==", MimeType: "image/png"}},
		}}, "not text (image)"},
	} {
		initialization := systemPromptInitialization(test.capability)
		prompt := test.prompt
		errors := map[string]error{}
		_, errors["session/new"] = mcp.NewSession(ctx, connection, initialization, "/repo", mcp.NewSessionOptions{SystemPrompt: &prompt})
		_, errors["session/resume"] = mcp.ResumeSession(ctx, connection, initialization, schema.ResumeSessionRequest{SessionID: "s", Cwd: "/repo", SystemPrompt: &prompt})
		_, errors["session/fork"] = mcp.ForkSession(ctx, connection, initialization, schema.ForkSessionRequest{SessionID: "s", Cwd: "/repo", SystemPrompt: &prompt})
		for method, err := range errors {
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("%s on %s: error = %v, want it to mention %q", name, method, err, test.want)
			}
		}
	}
	select {
	case params := <-requests:
		t.Fatalf("validation wrote %s to the wire", params)
	default:
	}
}
