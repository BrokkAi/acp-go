package schema

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
			// DefaultOnError<MaybeUndefined<VecSkipError<_>>>: bad items are
			// dropped; DefaultOnError reads a malformed patch as unset.
			name: "compaction drops bad summary items and malformed patches", value: new(CompactionUpdate),
			input: `{"compactionId":"c","status":"completed","summary":[{"type":"text","text":"kept"},{"type":"text"},7],"error":{"bad":true},"_meta":[]}`,
			want:  `{"compactionId":"c","status":"completed","summary":[{"type":"text","text":"kept"}]}`,
		},
		{
			name: "compaction keeps explicit null patches", value: new(CompactionUpdate),
			input: `{"compactionId":"c","status":"completed","summary":null,"error":null,"_meta":null}`,
			want:  `{"compactionId":"c","status":"completed","summary":null,"error":null,"_meta":null}`,
		},
		{
			// DefaultOnError<VecSkipError<_>>.
			name: "session info skips bad directories", value: new(SessionInfo),
			input: `{"sessionId":"s","cwd":"/w","additionalDirectories":["/a",5,null,"/b"],"title":7}`,
			want:  `{"sessionId":"s","cwd":"/w","additionalDirectories":["/a","/b"]}`,
		},
		{
			name: "session info reads a malformed list as not reported", value: new(SessionInfo),
			input: `{"sessionId":"s","cwd":"/w","additionalDirectories":"nope"}`,
			want:  `{"sessionId":"s","cwd":"/w"}`,
		},
		{
			name: "one bad command keeps the others", value: new(AvailableCommandsUpdate),
			input: `{"availableCommands":[{"name":"ok","description":"d"},{"name":"bad"}]}`,
			want:  `{"availableCommands":[{"name":"ok","description":"d"}]}`,
		},
		{
			name: "a required list must be present", value: new(AvailableCommandsUpdate),
			input: `{}`,
		},
		{
			// The tag-less stdio variant reads what the http variant does not.
			name: "an http server without a url falls back to stdio", value: new(McpServer),
			input: `{"type":"http","name":"n","command":"c","args":[],"env":[]}`,
			want:  `{"name":"n","command":"c","args":[],"env":[]}`,
		},
		{
			name: "a terminal without its id is rejected", value: new(ToolCallContent),
			input: `{"type":"terminal"}`,
		},
		{
			// Closed enums reject unknown values; default-on-error reads
			// the bad status as not reported.
			name: "an unknown status reads as not reported", value: new(ToolCallUpdate),
			input: `{"toolCallId":"t","status":"bogus"}`,
			want:  `{"toolCallId":"t"}`,
		},
		{
			name: "an unknown role is skipped", value: new(Annotations),
			input: `{"audience":["bogus","user"]}`,
			want:  `{"audience":["user"]}`,
		},
		{
			name: "a plan entry with an unknown priority is dropped", value: new(Plan),
			input: `{"entries":[{"content":"c","priority":"bogus","status":"pending"},{"content":"d","priority":"high","status":"pending"}]}`,
			want:  `{"entries":[{"content":"d","priority":"high","status":"pending"}]}`,
		},
		{
			name: "a closed stop reason rejects an unknown value", value: new(PromptResponse),
			input: `{"stopReason":"bogus"}`,
		},
		{
			// serde reads a unit variant from the map form {"variant": null}.
			name: "enums read from the map form", value: new(ToolCallUpdate),
			input: `{"toolCallId":"t","kind":{"read":null},"status":{"completed":null}}`,
			want:  `{"toolCallId":"t","kind":"read","status":"completed"}`,
		},
		{
			name: "an unknown tool kind in the map form reads as other", value: new(ToolCall),
			input: `{"toolCallId":"t","title":"x","kind":{"bogus":null}}`,
			want:  `{"toolCallId":"t","title":"x","kind":"other"}`,
		},
		{
			name: "a map-form role is kept in a list", value: new(Annotations),
			input: `{"audience":[{"user":null},"assistant"]}`,
			want:  `{"audience":["user","assistant"]}`,
		},
		{
			// A union of primitives accepts only values of its arms.
			name: "a config option with malformed options is dropped", value: new(ConfigOptionUpdate),
			input: `{"configOptions":[{"id":"a","name":"A","type":"select","currentValue":"x","options":"bad"},{"id":"b","name":"B","type":"select","currentValue":"x","options":[{"value":"x","name":"X"}]}]}`,
			want:  `{"configOptions":[{"id":"b","name":"B","type":"select","currentValue":"x","options":[{"value":"x","name":"X"}]}]}`,
		},
		{
			name: "an object is not an elicitation value", value: new(CreateElicitationResponse),
			input: `{"action":"accept","content":{"k":{"x":1}}}`,
		},
		{
			// serde reads a struct from an array, positionally.
			name: "a list request reads from an array", value: new(ListSessionsRequest),
			input: `["/w","c"]`,
			want:  `{"cwd":"/w","cursor":"c"}`,
		},
		{
			// Escaped keys match, braces inside strings are data, and a
			// duplicate key keeps its last value (the SDK parses into
			// serde_json::Value first).
			name: "keys are unescaped and the last duplicate wins", value: new(SessionInfo),
			input: ` { "sessionId" : "s\"}{" , "cwd":"/w", "sessionId":"t" } `,
			want:  `{"sessionId":"t","cwd":"/w"}`,
		},
		{
			name: "an array with spaces", value: new(ListSessionsRequest),
			input: ` [ "/w" , "c" ] `,
			want:  `{"cwd":"/w","cursor":"c"}`,
		},
		{
			name: "a malformed _meta reads as absent", value: new(ListSessionsRequest),
			input: `{"_meta":5}`,
			want:  `{}`,
		},
		{
			// From buffered content (a default-on-error list item) an
			// integer tag is a variant index; at the top level it is not.
			name: "a buffered integer tag is a variant index", value: new(ToolCallUpdate),
			input: `{"toolCallId":"t","content":[{"type":0,"content":{"type":"text","text":"x"}}]}`,
			want:  `{"toolCallId":"t","content":[{"type":"content","content":{"type":"text","text":"x"}}]}`,
		},
		{
			name: "an unbuffered integer tag is rejected", value: new(SessionNotification),
			input: `{"sessionId":"s","update":{"sessionUpdate":0,"content":{"type":"text","text":"x"}}}`,
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

// TestDecodeErrorsNameNoGoTypes keeps decode errors in serde's words, exactly
// as the Rust schema crate words them: they reach peers in -32602 answers,
// so they must not leak Go type names. The wants are serde_json::from_value
// errors of the 1.15.1 crate; every rejected hint fixture case is checked
// the same way by TestDeserializeHints.
func TestDecodeErrorsNameNoGoTypes(t *testing.T) {
	for _, tc := range []struct {
		value any
		input string
		want  string
	}{
		{new(ToolCallUpdate), `["t"]`, "invalid type: sequence, expected struct ToolCallUpdate"},
		{new(SessionInfo), `{"sessionId":5,"cwd":"/w"}`, "invalid type: integer `5`, expected a string"},
		{new(ReadTextFileRequest), `{"sessionId":"s","path":"/p","line":-1}`, "invalid value: integer `-1`, expected u32"},
		{new(Role), `null`, "invalid type: null, expected string or map"},
		{new(Role), `5`, "invalid type: integer `5`, expected string or map"},
		{new(Role), `"bogus"`, "unknown variant `bogus`, expected `assistant` or `user`"},
	} {
		err := json.Unmarshal([]byte(tc.input), tc.value)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: error = %v, want %q", tc.input, err, tc.want)
		}
	}
}
