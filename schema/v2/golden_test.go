package v2

import (
	"encoding/json"
	"testing"
)

var goldens = []struct {
	name string
	typ  any
	wire string
}{
	{
		"initialize request",
		InitializeRequest{},
		`{
			"protocolVersion": 2,
			"capabilities": {
				"auth": {"terminal": {}},
				"elicitation": {"form": {}, "url": {}}
			},
			"info": {"name": "acp-go", "version": "0.2.0"}
		}`,
	},
	{
		"initialize response",
		InitializeResponse{},
		`{
			"protocolVersion": 2,
			"info": {"name": "example-agent", "version": "1.0.0"},
			"capabilities": {"session": {}},
			"authMethods": [
				{"type": "agent", "methodId": "api-key", "name": "API Key"}
			]
		}`,
	},
	{
		"new session with stdio MCP",
		NewSessionRequest{},
		`{
			"cwd": "/repo",
			"mcpServers": [
				{"type": "stdio", "name": "tools", "command": "/opt/mcp-server", "args": ["--stdio"]}
			]
		}`,
	},
	{
		"accepted prompt",
		PromptRequest{},
		`{
			"sessionId": "sess_123",
			"prompt": [
				{"type": "text", "text": "inspect the repository"},
				{"type": "resource_link", "uri": "file:///repo/main.go", "name": "main.go"}
			]
		}`,
	},
	{
		"agent message update",
		UpdateSessionNotification{},
		`{
			"sessionId": "sess_123",
			"update": {
				"sessionUpdate": "agent_message",
				"messageId": "msg_1",
				"content": [{"type": "text", "text": "hello from v2"}]
			}
		}`,
	},
	{
		"idle state update",
		UpdateSessionNotification{},
		`{
			"sessionId": "sess_123",
			"update": {
				"sessionUpdate": "state_update",
				"state": "idle",
				"stopReason": "end_turn"
			}
		}`,
	},
	{
		"terminal auth method",
		AuthMethod{},
		`{
			"type": "terminal",
			"methodId": "console",
			"name": "Anthropic Console",
			"args": ["--console"]
		}`,
	},
	{
		"session list page",
		ListSessionsResponse{},
		`{
			"sessions": [{"sessionId": "sess_123", "cwd": "/repo"}],
			"nextCursor": "next"
		}`,
	},
}

func TestGoldenFlows(t *testing.T) {
	for _, golden := range goldens {
		t.Run(golden.name, func(t *testing.T) {
			want := canonical(t, golden.wire)
			value := golden.typ
			if err := json.Unmarshal([]byte(golden.wire), &value); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if canonical(t, string(got)) != want {
				t.Fatalf("round trip mismatch\n got: %s\nwant: %s", got, golden.wire)
			}
		})
	}
}

func TestUnionValidation(t *testing.T) {
	var both = SessionUpdate{
		AgentMessage: &AgentMessage{MessageID: "a"},
		UsageUpdate:  &UsageUpdate{},
	}
	if _, err := json.Marshal(both); err == nil {
		t.Fatal("two set variants must fail to marshal")
	}
	var none SessionUpdate
	if _, err := json.Marshal(none); err == nil {
		t.Fatal("zero set variants must fail to marshal")
	}
	var unknown UpdateSessionNotification
	wire := `{"sessionId":"s","update":{"sessionUpdate":"future_update","custom":true}}`
	if err := json.Unmarshal([]byte(wire), &unknown); err != nil {
		t.Fatalf("future session update must decode: %v", err)
	}
	if unknown.Update.Other == nil || unknown.Update.Kind != SessionUpdateKind("future_update") {
		t.Fatalf("future update was not preserved: %+v", unknown.Update)
	}
}

func TestPatchNullDiffersFromOmission(t *testing.T) {
	var omitted AgentMessage
	if err := json.Unmarshal([]byte(`{"messageId":"m"}`), &omitted); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(omitted)
	if err != nil {
		t.Fatal(err)
	}
	if canonical(t, string(encoded)) != canonical(t, `{"messageId":"m"}`) {
		t.Fatalf("omitted content re-encoded as %s", encoded)
	}

	var cleared AgentMessage
	if err := json.Unmarshal([]byte(`{"messageId":"m","content":null}`), &cleared); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(cleared)
	if err != nil {
		t.Fatal(err)
	}
	if canonical(t, string(encoded)) != canonical(t, `{"messageId":"m","content":null}`) {
		t.Fatalf("explicit null re-encoded as %s", encoded)
	}
}
