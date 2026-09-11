package unstable

import (
	"encoding/json"
	"testing"
)

// goldens are hand-written wire documents for representative flows. Unlike
// the synthesized parity cases, they carry realistic field values and pin the
// exact shapes an implementer sends and receives.
var goldens = []struct {
	name string
	typ  any
	wire string
}{
	{
		"initialize request",
		InitializeRequest{},
		`{
			"protocolVersion": 1,
			"clientCapabilities": {
				"fs": {"readTextFile": true, "writeTextFile": true},
				"terminal": true,
				"elicitation": {"form": {}}
			},
			"clientInfo": {"name": "acp-go", "version": "0.1.0"}
		}`,
	},
	{
		"initialize response with auth",
		InitializeResponse{},
		`{
			"protocolVersion": 1,
			"agentCapabilities": {
				"loadSession": true,
				"promptCapabilities": {"image": true},
				"auth": {"logout": {}}
			},
			"agentInfo": {"name": "example-agent", "version": "1.2.3"},
			"authMethods": [
				{"id": "agent-login", "type": "agent"},
				{"id": "terminal-login", "type": "terminal"}
			]
		}`,
	},
	{
		"session/new with stdio MCP server",
		NewSessionRequest{},
		`{
			"cwd": "/repo",
			"mcpServers": [
				{"name": "docs", "command": "npx", "args": ["-y", "@docs/server"]}
			]
		}`,
	},
	{
		"session/new response with modes and options",
		NewSessionResponse{},
		`{
			"sessionId": "sess_123",
			"modes": {"currentModeId": "default", "availableModes": [
				{"id": "default", "name": "Default"},
				{"id": "plan", "name": "Plan"}
			]},
			"configOptions": [
				{
					"id": "model", "name": "Model", "type": "select",
					"category": "model", "currentValue": "opus",
					"options": [{"value": "opus", "name": "Opus"}]
				}
			]
		}`,
	},
	{
		"prompt with text and resource link",
		PromptRequest{},
		`{
			"sessionId": "sess_123",
			"prompt": [
				{"type": "text", "text": "inspect the logs"},
				{"type": "resource_link", "uri": "file:///repo/main.go", "name": "main.go"}
			]
		}`,
	},
	{
		"prompt response refusal",
		PromptResponse{},
		`{"stopReason": "refusal"}`,
	},
	{
		"session update: tool call with diff",
		SessionNotification{},
		`{
			"sessionId": "sess_123",
			"update": {
				"sessionUpdate": "tool_call",
				"toolCallId": "call_1",
				"title": "Edit main.go",
				"kind": "edit",
				"status": "in_progress",
				"content": [
					{"type": "diff", "path": "/repo/main.go", "oldText": "a", "newText": "b"}
				],
				"locations": [{"path": "/repo/main.go", "line": 4}]
			}
		}`,
	},
	{
		"session update: plan and usage",
		SessionNotification{},
		`{
			"sessionId": "sess_123",
			"update": {
				"sessionUpdate": "plan",
				"entries": [
					{"content": "find the bug", "priority": "high", "status": "completed"},
					{"content": "write the fix", "priority": "medium", "status": "in_progress"}
				]
			}
		}`,
	},
	{
		"session update: usage",
		SessionNotification{},
		`{
			"sessionId": "sess_123",
			"update": {
				"sessionUpdate": "usage_update",
				"contextWindow": {"used": 12000, "size": 200000},
				"cumulativeCost": {"usd": 0.42}
			}
		}`,
	},
	{
		"permission request and selection",
		RequestPermissionRequest{},
		`{
			"sessionId": "sess_123",
			"toolCall": {"toolCallId": "call_9", "title": "run tests", "kind": "execute"},
			"options": [
				{"optionId": "allow-once", "name": "Allow", "kind": "allow_once"},
				{"optionId": "reject-once", "name": "Deny", "kind": "reject_once"}
			]
		}`,
	},
	{
		"permission outcome selected",
		RequestPermissionResponse{},
		`{"outcome": {"outcome": "selected", "optionId": "allow-once"}}`,
	},
	{
		"terminal create and output",
		CreateTerminalRequest{},
		`{"command": "go", "args": ["test", "./..."], "cwd": "/repo", "outputByteLimit": 1048576}`,
	},
	{
		"terminal output with exit status",
		TerminalOutputResponse{},
		`{"output": "ok\n", "truncated": false, "exitStatus": {"exitCode": 0, "signal": null}}`,
	},
	{
		"fs read with line range",
		ReadTextFileRequest{},
		`{"sessionId": "sess_123", "path": "/repo/main.go", "line": 10, "limit": 5}`,
	},
	{
		"elicitation form request",
		CreateElicitationRequest{},
		`{
			"sessionId": "sess_123",
			"mode": "form",
			"requestedSchema": {
				"type": "object",
				"properties": {
					"ticket": {"type": "string", "format": "uri"}
				},
				"required": ["ticket"]
			}
		}`,
	},
	{
		"elicitation url request and accept",
		CreateElicitationRequest{},
		`{
			"sessionId": "sess_123",
			"mode": "url",
			"elicitationId": "el_1",
			"url": "https://example.test/login",
			"message": "Log in to continue"
		}`,
	},
	{
		"set config option boolean",
		SetSessionConfigOptionRequest{},
		`{"sessionId": "sess_123", "configId": "verbose", "type": "boolean", "value": true}`,
	},
	{
		"set config option select",
		SetSessionConfigOptionRequest{},
		`{"sessionId": "sess_123", "configId": "model", "value": "opus"}`,
	},
	{
		"cancel request",
		CancelRequestNotification{},
		`{"requestId": 42}`,
	},
}

func TestGoldenFlows(t *testing.T) {
	for _, g := range goldens {
		t.Run(g.name, func(t *testing.T) {
			want := canonical(t, g.wire)
			if err := json.Unmarshal([]byte(g.wire), &g.typ); err != nil {
				t.Fatalf("decode: %v\n%s", err, g.wire)
			}
			got, err := json.Marshal(g.typ)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if canonical(t, string(got)) != want {
				t.Errorf("round trip mismatch\n got: %s\nwant: %s", got, g.wire)
			}
		})
	}
}

// TestUnionValidation checks that unions reject ambiguous and empty payloads
// rather than silently encoding something surprising.
func TestUnionValidation(t *testing.T) {
	var both = SessionUpdate{
		ToolCall:    &ToolCall{ToolCallID: "a"},
		UsageUpdate: &UsageUpdate{},
	}
	if _, err := json.Marshal(both); err == nil {
		t.Error("two set variants must fail to marshal")
	}
	var none SessionUpdate
	if _, err := json.Marshal(none); err == nil {
		t.Error("zero set variants must fail to marshal")
	}
	var bad = []byte(`{"sessionId": "s", "update": {"sessionUpdate": "not_a_real_kind"}}`)
	var n SessionNotification
	if err := json.Unmarshal(bad, &n); err == nil {
		t.Error("unknown sessionUpdate tag must fail to decode")
	}
}
