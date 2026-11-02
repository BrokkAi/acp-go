package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	"github.com/BrokkAi/acp-go/v2/mcp"
)

// rawPeer answers every request with a session ID and records its params.
func rawPeer(t *testing.T) (*acpv2.Connection, chan json.RawMessage) {
	t.Helper()
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	requests := make(chan json.RawMessage, 8)
	server := acp.Connect(pair.B, pair.B, agentSide(func(_ context.Context, _ string, raw json.RawMessage) (any, error) {
		requests <- raw
		return map[string]any{"sessionId": "s"}, nil
	}), nil)
	connection := acpv2.Connect(pair.A, pair.A, nil, nil)
	t.Cleanup(func() {
		_ = connection.Close()
		_ = server.Close()
		pair.Close()
	})
	initializeClient(t, connection)
	return connection, requests
}

func systemPromptInitialization(capability *schema.SessionSystemPromptCapabilities) schema.InitializeResponse {
	initialization := forkInitialization(allTransports())
	initialization.Capabilities.Session.SystemPrompt = capability
	return initialization
}

func TestSessionHelpersCombineMCPServersWithASystemPrompt(t *testing.T) {
	ctx := context.Background()
	connection, requests := rawPeer(t)
	initialization := systemPromptInitialization(&schema.SessionSystemPromptCapabilities{})
	prompt := &schema.SystemPrompt{Mode: schema.SystemPromptModeAppend, Content: []schema.ContentBlock{{Text: &schema.TextContent{Text: "Use the docs server."}}}}
	servers := []schema.McpServer{acpServer("docs", "docs-1")}

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
	connection, requests := rawPeer(t)
	text := []schema.ContentBlock{{Text: &schema.TextContent{Text: "x"}}}
	for name, test := range map[string]struct {
		capability *schema.SessionSystemPromptCapabilities
		prompt     schema.SystemPrompt
		want       string
	}{
		"capability absent":              {nil, schema.SystemPrompt{Mode: schema.SystemPromptModeAppend, Content: text}, "did not advertise systemPrompt support"},
		"override without mode.override": {&schema.SessionSystemPromptCapabilities{}, schema.SystemPrompt{Mode: schema.SystemPromptModeOverride, Content: text}, "override mode"},
		"unknown mode":                   {&schema.SessionSystemPromptCapabilities{}, schema.SystemPrompt{Mode: "replace", Content: text}, "unsupported systemPrompt mode"},
		"mode missing":                   {&schema.SessionSystemPromptCapabilities{}, schema.SystemPrompt{Content: text}, "mode is required"},
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
	// The session helpers read the unstable capabilities: the baseline
	// session surface, additional directories, and the native acp transport.
	if _, err := mcp.NewSession(ctx, connection, schema.InitializeResponse{}, "/repo", mcp.NewSessionOptions{}); err == nil || !strings.Contains(err.Error(), "session method surface") {
		t.Fatalf("session/new without session capabilities error = %v", err)
	}
	if _, err := mcp.NewSession(ctx, connection, forkInitialization(nil), "/repo", mcp.NewSessionOptions{Servers: []schema.McpServer{acpServer("docs", "docs-1")}}); err == nil || !strings.Contains(err.Error(), "MCP-over-ACP") {
		t.Fatalf("acp server without the capability error = %v", err)
	}
	if _, err := mcp.ResumeSession(ctx, connection, forkInitialization(nil), schema.ResumeSessionRequest{Cwd: "/repo"}); err == nil || !strings.Contains(err.Error(), "session ID is required") {
		t.Fatalf("blank resume session ID error = %v", err)
	}
	select {
	case params := <-requests:
		t.Fatalf("validation wrote %s to the wire", params)
	default:
	}
}

func TestAttachedSessionsCarryAndCheckTheSystemPrompt(t *testing.T) {
	ctx := context.Background()
	connection, requests := rawPeer(t)
	router := mcp.NewMessageRouter(connection)
	server := mcp.NewToolServer("project-tools", "1.0.0", mcp.NewToolRegistry())
	prompt := &schema.SystemPrompt{Mode: schema.SystemPromptModeOverride, Content: []schema.ContentBlock{{Text: &schema.TextContent{Text: "x"}}}}

	// Unsupported prompts fail before writing and release the registration.
	if _, _, err := mcp.NewAttachedSession(ctx, router, systemPromptInitialization(&schema.SessionSystemPromptCapabilities{}), schema.NewSessionRequest{Cwd: "/repo", SystemPrompt: prompt}, server); err == nil || !strings.Contains(err.Error(), "override mode") {
		t.Fatalf("attached session/new error = %v", err)
	}
	if _, _, err := mcp.ResumeAttachedSession(ctx, router, systemPromptInitialization(nil), schema.ResumeSessionRequest{SessionID: "s", Cwd: "/repo", SystemPrompt: prompt}, server); err == nil || !strings.Contains(err.Error(), "systemPrompt support") {
		t.Fatalf("attached session/resume error = %v", err)
	}
	select {
	case params := <-requests:
		t.Fatalf("validation wrote %s to the wire", params)
	default:
	}

	withOverride := systemPromptInitialization(&schema.SessionSystemPromptCapabilities{Mode: &schema.SessionSystemPromptModeCapabilities{
		Override: &schema.SessionSystemPromptOverrideCapabilities{},
	}})
	_, attachment, err := mcp.NewAttachedSession(ctx, router, withOverride, schema.NewSessionRequest{Cwd: "/repo", SystemPrompt: prompt}, server)
	if err != nil {
		t.Fatal(err)
	}
	defer attachment.Close(ctx)
	params := string(<-requests)
	if !strings.Contains(params, `"name":"project-tools"`) || !strings.Contains(params, `"systemPrompt":{"content":[{"text":"x","type":"text"}],"mode":"override"}`) {
		t.Fatalf("attached session/new params = %s", params)
	}
}
