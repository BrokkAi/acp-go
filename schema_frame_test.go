package acp

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go/schema"
)

// frameParityTypes are the v1 types whose captured Rust encodings
// TestTypedFramesMatchTheRustEncoding sends as params: those with f64
// fields, raw JSON fields, unions of primitives, string escapes, the
// session/update notification itself, and Meta: a _meta object as such, and
// every _meta of these types set to one with unsorted and repeated keys, big
// integers, and floats.
var frameParityTypes = map[string]reflect.Type{
	"Annotations":               reflect.TypeFor[schema.Annotations](),
	"CancelRequestNotification": reflect.TypeFor[schema.CancelRequestNotification](),
	"ContentBlock":              reflect.TypeFor[schema.ContentBlock](),
	"Cost":                      reflect.TypeFor[schema.Cost](),
	"ElicitationAcceptAction":   reflect.TypeFor[schema.ElicitationAcceptAction](),
	"Error":                     reflect.TypeFor[schema.Error](),
	"InitializeResponse":        reflect.TypeFor[schema.InitializeResponse](),
	"Meta":                      reflect.TypeFor[schema.Meta](),
	"NumberPropertySchema":      reflect.TypeFor[schema.NumberPropertySchema](),
	"SessionConfigSelect":       reflect.TypeFor[schema.SessionConfigSelect](),
	"SessionNotification":       reflect.TypeFor[schema.SessionNotification](),
	"SessionUpdate":             reflect.TypeFor[schema.SessionUpdate](),
	"TextContent":               reflect.TypeFor[schema.TextContent](),
	"ToolCall":                  reflect.TypeFor[schema.ToolCall](),
	"ToolCallUpdate":            reflect.TypeFor[schema.ToolCallUpdate](),
}

// TestTypedFramesMatchTheRustEncoding sends decoded schema values through
// acp.Connection, as notification params and as call params, and requires
// each frame to carry exactly the serde_json::to_string encoding of the Rust
// value captured in schema/testdata/hints_rust.json: the frame writer must
// not re-parse the numbers of a typed value, as the reference SDK, which
// writes a Rust value with to_value and to_string, does not.
func TestTypedFramesMatchTheRustEncoding(t *testing.T) {
	data, err := os.ReadFile("schema/testdata/hints_rust.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures struct {
		Cases map[string]struct {
			Input   string `json:"input"`
			Encoded string `json:"encoded"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixtures.Cases))
	for name, c := range fixtures.Cases {
		def, _, _ := strings.Cut(name, "/")
		def, _, _ = strings.Cut(def, ".")
		def, _, _ = strings.Cut(def, "[")
		if _, ok := frameParityTypes[def]; ok && c.Encoded != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) < 200 {
		t.Fatalf("only %d cases selected", len(names))
	}
	c, peer := pipeClient(t, nil, nil)
	lines := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(peer)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(lines)
				return
			}
			lines <- strings.TrimSuffix(line, "\n")
		}
	}()
	for _, name := range names {
		fixture := fixtures.Cases[name]
		def, _, _ := strings.Cut(name, "/")
		def, _, _ = strings.Cut(def, ".")
		def, _, _ = strings.Cut(def, "[")
		value := reflect.New(frameParityTypes[def])
		if err := json.Unmarshal([]byte(fixture.Input), value.Interface()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := c.Notify(t.Context(), "x/parity", value.Interface()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := `{"jsonrpc":"2.0","method":"x/parity","params":` + fixture.Encoded + `}`
		if got := <-lines; got != want {
			t.Errorf("%s:\n got %s\nwant %s", name, got, want)
		}
		go func() { _ = c.Call(t.Context(), "x/parity", value.Interface(), nil) }()
		got := <-lines
		var frame struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal([]byte(got), &frame)
		want = `{"jsonrpc":"2.0","id":` + string(frame.ID) + `,"method":"x/parity","params":` + fixture.Encoded + `}`
		if got != want {
			t.Errorf("%s call:\n got %s\nwant %s", name, got, want)
		}
	}
}
