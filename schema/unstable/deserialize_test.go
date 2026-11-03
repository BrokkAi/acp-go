package unstable

import (
	"encoding/json"
	"testing"
)

// TestDeserializeHintExamples spells out a few outcomes that
// TestDeserializeHints checks across the whole schema. The wants are the
// Rust schema crate's re-encodings of the same inputs.
func TestDeserializeHintExamples(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		input string
		want  string
	}{
		{name: "a malformed _meta reads as absent", value: new(ListProvidersRequest), input: `{"_meta":5}`, want: `{}`},
		{name: "a malformed positional _meta reads as absent", value: new(ListProvidersRequest), input: `[5]`, want: `{}`},
		{name: "a list request reads from an array", value: new(ListSessionsRequest), input: `["/w","c"]`, want: `{"cwd":"/w","cursor":"c"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tc.input), tc.value); err != nil {
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
	// Five elements for ListSessionsRequest's four fields.
	if err := json.Unmarshal([]byte(`[null,null,null,null,null]`), new(ListSessionsRequest)); err == nil {
		t.Fatal("an array longer than the struct was accepted")
	}
}
