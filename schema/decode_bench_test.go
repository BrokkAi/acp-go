package schema

import (
	"encoding/json"
	"testing"
)

// The session/update hot path: message chunks and tool call updates.
var benchUpdates = []string{
	`{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hello, world. This is a streamed chunk of text.","annotations":{"audience":["user"],"priority":0.5}},"messageId":"m"}}`,
	`{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"t","status":"in_progress","content":[{"type":"content","content":{"type":"text","text":"output line"}},{"type":"diff","path":"/w/a.go","oldText":"a","newText":"b"}],"locations":[{"path":"/w/a.go","line":3}],"_meta":{"k":"v"}}}`,
}

func BenchmarkDecodeSessionNotification(b *testing.B) {
	data := make([][]byte, len(benchUpdates))
	for i, u := range benchUpdates {
		data[i] = []byte(u)
	}
	b.ReportAllocs()
	for b.Loop() {
		for _, d := range data {
			var n SessionNotification
			if err := json.Unmarshal(d, &n); err != nil {
				b.Fatal(err)
			}
		}
	}
}
