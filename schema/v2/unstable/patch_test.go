package unstable

import (
	"encoding/json"
	"testing"
)

// The draft subagent_update and session_message upserts keep null distinct
// from omission for every patch field, including cwd, additionalDirectories,
// senderMessageId, and recipientMessageId, and patch their top-level _meta.
func TestDraftUpsertsPatchFields(t *testing.T) {
	var subagent SubagentUpdate
	for _, doc := range []string{
		`{"sessionId":"sub","cwd":"/work","additionalDirectories":["/lib"],"_meta":{"a":1,"b":2}}`,
		`{"sessionId":"sub","title":"Child"}`,
		`{"sessionId":"sub","cwd":null,"_meta":{"a":null}}`,
	} {
		var update SubagentUpdate
		if err := json.Unmarshal([]byte(doc), &update); err != nil {
			t.Fatal(err)
		}
		subagent.ApplyUpdate(update)
	}
	assertJSON(t, subagent, `{"sessionId":"sub","title":"Child","cwd":null,"additionalDirectories":["/lib"],"_meta":{"b":2}}`)

	var message SessionMessage
	for _, doc := range []string{
		`{"messageId":"m","senderMessageId":"s1","recipientMessageId":"r1"}`,
		`{"messageId":"m","senderMessageId":null}`,
		`{"messageId":"m","recipientMessageId":"r2","_meta":{"x":{"y":null}}}`,
	} {
		var update SessionMessage
		if err := json.Unmarshal([]byte(doc), &update); err != nil {
			t.Fatal(err)
		}
		message.ApplyUpdate(update)
	}
	assertJSON(t, message, `{"messageId":"m","senderMessageId":null,"recipientMessageId":"r2","_meta":{"x":{"y":null}}}`)
}

func assertJSON(t *testing.T, value any, want string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if got := canonical(t, string(encoded)); got != canonical(t, want) {
		t.Fatalf("stored %s, want %s", got, want)
	}
}
