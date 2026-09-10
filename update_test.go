package acp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go/schema"
)

func TestUpdateConstructorsProduceEveryWireShape(t *testing.T) {
	chunk := schema.ContentChunk{Content: NewTextContent("hello")}
	commands := []schema.AvailableCommand{{Name: "plan", Description: "Create a plan"}}
	options := []schema.SessionConfigOption{{
		ID: "model", Name: "Model", Select: &schema.SessionConfigSelect{
			CurrentValue: "large", Options: []schema.SessionConfigSelectOption{{Value: "large", Name: "Large"}},
		},
	}}
	title := "Release"
	tests := []struct {
		name string
		call func(SessionID) Update
		want string
	}{
		{"user message chunk", func(id SessionID) Update {
			return NewUserMessageChunkUpdate(id, chunk)
		}, `{"sessionId":"s","update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"hello"}}}`},
		{"agent message chunk", func(id SessionID) Update {
			return NewAgentMessageChunkUpdate(id, chunk)
		}, `{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"hello"}}}`},
		{"agent thought chunk", func(id SessionID) Update {
			return NewAgentThoughtChunkUpdate(id, chunk)
		}, `{"sessionId":"s","update":{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"hello"}}}`},
		{"tool call", func(id SessionID) Update {
			return NewToolCallUpdate(id, schema.ToolCall{ToolCallID: "build", Title: "Build"})
		}, `{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"build","title":"Build"}}`},
		{"tool call update", func(id SessionID) Update {
			return NewToolCallChangedUpdate(id, schema.ToolCallUpdate{ToolCallID: "build"})
		}, `{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"build"}}`},
		{"plan", func(id SessionID) Update {
			return NewPlanUpdate(id, schema.Plan{Entries: []schema.PlanEntry{{
				Content: "Ship", Priority: schema.PlanEntryPriorityHigh, Status: schema.PlanEntryStatusPending,
			}}})
		}, `{"sessionId":"s","update":{"sessionUpdate":"plan","entries":[{"content":"Ship","priority":"high","status":"pending"}]}}`},
		{"available commands", func(id SessionID) Update {
			return NewAvailableCommandsUpdate(id, commands)
		}, `{"sessionId":"s","update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"plan","description":"Create a plan"}]}}`},
		{"current mode", func(id SessionID) Update { return NewCurrentModeUpdate(id, "plan") }, `{"sessionId":"s","update":{"sessionUpdate":"current_mode_update","currentModeId":"plan"}}`},
		{"config options", func(id SessionID) Update { return NewConfigOptionUpdate(id, options) }, `{"sessionId":"s","update":{"sessionUpdate":"config_option_update","configOptions":[{"id":"model","name":"Model","type":"select","currentValue":"large","options":[{"value":"large","name":"Large"}]}]}}`},
		{"session info", func(id SessionID) Update {
			return NewSessionInfoUpdate(id, schema.SessionInfoUpdate{Title: &title})
		}, `{"sessionId":"s","update":{"sessionUpdate":"session_info_update","title":"Release"}}`},
		{"usage", func(id SessionID) Update { return NewUsageUpdate(id, 12, 64) }, `{"sessionId":"s","update":{"sessionUpdate":"usage_update","used":12,"size":64}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.call("s"))
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(test.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %s, want %s", encoded, test.want)
			}
		})
	}
}

func TestSessionUpdatesAdapterFiltersAndRejectsMalformedJSON(t *testing.T) {
	called := false
	updates := SessionUpdates(func(Update) error {
		called = true
		return nil
	})
	if err := updates("custom/notification", json.RawMessage(`{}`)); err != nil || called {
		t.Fatalf("non-update was not ignored: called=%v err=%v", called, err)
	}
	err := updates(schema.SessionUpdateMethodName, json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"future"`))
	if err == nil || !strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Fatalf("malformed update error = %v", err)
	}
	if called {
		t.Fatal("handler ran for malformed update")
	}
}
