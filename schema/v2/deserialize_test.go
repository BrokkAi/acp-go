package v2

import (
	"encoding/json"
	"testing"
)

// TestDeserializeHintExamples spells out, for a few messages, the outcomes
// that TestDeserializeHints checks across the whole schema. The wants are the
// Rust schema crate's re-encodings of the same inputs.
func TestDeserializeHintExamples(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		input string
		want  string // "" when decoding must fail
	}{
		{
			// DefaultOnError without VecSkipError: one bad item makes the
			// whole snapshot not reported, and a malformed _meta reads as
			// absent.
			name: "idle state reads a bad background list as not reported", value: new(IdleStateUpdate),
			input: `{"stopReason":"error","background":[{"type":"terminal","terminalId":"t"},{"type":"terminal"}],"_meta":5}`,
			want:  `{"stopReason":"error"}`,
		},
		{
			name: "a malformed patch reads as unset", value: new(CompactionUpdate),
			input: `{"compactionId":"c","status":"completed","summary":5}`,
			want:  `{"compactionId":"c","status":"completed"}`,
		},
		{
			// Rust decodes the line as a NonZeroU32 under DefaultOnError.
			name: "a zero line reads as no line", value: new(ToolCallLocation),
			input: `{"path":"/f","line":0}`,
			want:  `{"path":"/f"}`,
		},
		{
			name: "background work needs its type", value: new(BackgroundWork),
			input: `{"terminalId":"t"}`,
		},
		{
			// v2 ToolCallUpdate flattens nothing in Rust, so it reads from an
			// array, inside a union's array form too.
			name: "a tool call update reads from an array", value: new(UpdateSessionNotification),
			input: `{"sessionId":"s","update":["tool_call_update","t"]}`,
			want:  `{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"t"}}`,
		},
		{
			// serde reads a unit variant from the map form too.
			name: "an enum reads from the map form", value: new(ToolCallUpdate),
			input: `{"toolCallId":"t","kind":{"read":null}}`,
			want:  `{"toolCallId":"t","kind":"read"}`,
		},
		{
			name: "an open enum rejects null", value: new(NoticeSeverity),
			input: `null`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := json.Unmarshal([]byte(tc.input), tc.value)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("decoded %s", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if canonical(t, string(got)) != canonical(t, tc.want) {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}
