package schema

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"
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

// manyMembers returns a PromptRequest object with n junk members between a
// sessionId and its repeat.
func manyMembers(n int) []byte {
	b := []byte(`{"sessionId":"a"`)
	for i := range n {
		b = append(b, `,"k`...)
		b = strconv.AppendInt(b, int64(i), 10)
		b = append(b, `":0`...)
	}
	return append(b, `,"sessionId":7}`...)
}

// TestDecodeIsLinearInObjectSize guards against a peer stalling a decoder
// with a huge object: merging repeated keys and visiting members must stay
// linear, so 100k members decode in well under the bound (a quadratic visit
// takes tens of seconds), and the repeated key still reads with its last
// value, as in the reference SDK.
func TestDecodeIsLinearInObjectSize(t *testing.T) {
	data := manyMembers(100_000)
	start := time.Now()
	var request PromptRequest
	err := json.Unmarshal(data, &request)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("decoding 100k members took %v", elapsed)
	}
	if err == nil || err.Error() != "invalid type: integer `7`, expected a string" {
		t.Fatalf("error = %v", err)
	}
}

func BenchmarkDecodeManyMembers(b *testing.B) {
	data := manyMembers(100_000)
	b.ReportAllocs()
	for b.Loop() {
		var request PromptRequest
		_ = json.Unmarshal(data, &request)
	}
}
