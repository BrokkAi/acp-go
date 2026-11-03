package agent

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/BrokkAi/acp-go"
	unstableschema "github.com/BrokkAi/acp-go/schema/unstable"
)

// The capabilities are read as the unstable schema reads them: an omitted,
// null, or malformed value does not advertise support.
func TestUnstableClientCapabilitiesReadTheUnstableSchema(t *testing.T) {
	for params, want := range map[string]bool{
		`{"protocolVersion":1,"clientCapabilities":{"subagents":{}}}`:                true,
		`{"protocolVersion":1,"clientCapabilities":{"subagents":{"_meta":{"x":1}}}}`: true,
		`{"protocolVersion":1,"clientCapabilities":{"subagents":null}}`:              false,
		`{"protocolVersion":1,"clientCapabilities":{"subagents":true}}`:              false,
		`{"protocolVersion":1,"clientCapabilities":{}}`:                              false,
		`{"protocolVersion":1}`: false,
		`{"protocolVersion":1,"clientCapabilities":{"subagents":{},"subagents":"nope"}}`: false,
	} {
		if got := unstableClientCapabilities(json.RawMessage(params)).Subagents != nil; got != want {
			t.Errorf("%s: subagents advertised = %v, want %v", params, got, want)
		}
	}
	if unstableClientCapabilities(json.RawMessage(`{"protocolVersion":1,"clientCapabilities":{"plan":{}}}`)).Plan == nil {
		t.Error("plan was not read")
	}
}

// gatedUpdates are, per capability, an update needing it in typed, raw, and
// malformed (named and positional) form.
var gatedUpdates = []struct {
	capability string
	typed      unstableschema.SessionUpdate
	raw        string
	malformed  []string
}{
	{
		capability: `{"session":{"notices":{}}}`,
		typed:      unstableschema.SessionUpdate{Notice: &unstableschema.Notice{Severity: "info", Title: "t"}},
		raw:        `{"sessionId":"p","update":{"sessionUpdate":"notice","severity":"info","title":"t"}}`,
		malformed:  []string{`{"sessionId":"p","update":{"sessionUpdate":"notice"}}`, `["p",["notice"]]`},
	},
	{
		capability: `{"session":{"compaction":{}}}`,
		typed:      unstableschema.SessionUpdate{CompactionUpdate: &unstableschema.CompactionUpdate{CompactionID: "c", Status: unstableschema.CompactionStatusInProgress}},
		raw:        `{"sessionId":"p","update":{"sessionUpdate":"compaction_summary_chunk","compactionId":"c","content":{"type":"text","text":"x"}}}`,
		malformed:  []string{`{"sessionId":"p","update":{"sessionUpdate":"compaction_update"}}`, `["p",{"sessionUpdate":"compaction_summary_chunk"}]`},
	},
	{
		capability: `{"plan":{}}`,
		typed:      unstableschema.SessionUpdate{PlanRemoved: &unstableschema.PlanRemoved{}},
		raw:        `{"sessionId":"p","update":{"sessionUpdate":"plan_removed"}}`,
		malformed:  []string{`{"sessionId":"p","update":{"sessionUpdate":"plan_update"}}`, `["p",["plan_update"]]`},
	},
	{
		capability: `{"subagents":{}}`,
		typed:      unstableschema.SessionUpdate{SubagentUpdate: &unstableschema.SubagentUpdate{SessionID: "c"}},
		raw:        `{"sessionId":"p","update":{"sessionUpdate":"session_message","messageId":"m"}}`,
		malformed:  []string{`{"sessionId":"p","update":{"sessionUpdate":"session_message_chunk"}}`, `["parent",["subagent_update"]]`},
	},
	{
		capability: `{"session":{"configOptions":{"boolean":{}}}}`,
		raw:        `{"sessionId":"p","update":{"sessionUpdate":"config_option_update","configOptions":[{"type":"boolean","id":"o","name":"O","currentValue":true}]}}`,
	},
}

func capabilities(t *testing.T, document string) unstableschema.ClientCapabilities {
	t.Helper()
	var decoded unstableschema.ClientCapabilities
	if err := json.Unmarshal([]byte(document), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func requireRefused(t *testing.T, name string, err error) {
	t.Helper()
	var rpcError *acp.RPCError
	if !errors.As(err, &rpcError) || rpcError.Code != -32601 {
		t.Errorf("%s = %v, want -32601", name, err)
	}
}

func TestUnstableSessionUpdatesAreGatedInEveryEncoding(t *testing.T) {
	for _, test := range gatedUpdates {
		var params []any
		if test.typed.Notice != nil || test.typed.CompactionUpdate != nil || test.typed.PlanRemoved != nil || test.typed.SubagentUpdate != nil {
			notification := unstableschema.SessionNotification{SessionID: "p", Update: test.typed}
			params = append(params, notification, &notification)
		}
		params = append(params, json.RawMessage(test.raw))
		var decoded map[string]any
		if err := json.Unmarshal([]byte(test.raw), &decoded); err != nil {
			t.Fatal(err)
		}
		params = append(params, decoded)
		for _, malformed := range test.malformed {
			params = append(params, json.RawMessage(malformed))
		}
		granted := capabilities(t, test.capability)
		for i, param := range params {
			requireRefused(t, test.capability+" missing", requireUnstableSessionUpdate(unstableschema.ClientCapabilities{}, param))
			if err := requireUnstableSessionUpdate(granted, param); err != nil {
				t.Errorf("%s params %d with the capability: %v", test.capability, i, err)
			}
		}
	}
	for name, params := range map[string]any{
		"nil pointer":   (*unstableschema.SessionNotification)(nil),
		"agent chunk":   json.RawMessage(`{"sessionId":"p","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}`),
		"select config": json.RawMessage(`{"sessionId":"p","update":{"sessionUpdate":"config_option_update","configOptions":[]}}`),
		"not an object": json.RawMessage(`[]`),
		"unknown":       json.RawMessage(`{"sessionId":"p","update":{"sessionUpdate":"_vendor"}}`),
	} {
		if err := requireUnstableSessionUpdate(unstableschema.ClientCapabilities{}, params); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}
