package v2

import (
	"encoding/json"
	"reflect"
	"testing"
)

// upsert is a v2 update whose ApplyUpdate patches stored entity state.
type upsert[T any] interface {
	*T
	ApplyUpdate(T)
}

// upsertCase folds wire updates for one participating update type.
type upsertCase struct {
	name string
	// fold decodes each update over the entity's ID fields and applies them in
	// order to a new entity, returning the stored state's wire fields.
	fold func(t *testing.T, updates ...string) map[string]json.RawMessage
}

func upsertOf[T any, P upsert[T]](name, base string) upsertCase {
	return upsertCase{name: name, fold: func(t *testing.T, updates ...string) map[string]json.RawMessage {
		t.Helper()
		var state T
		if err := json.Unmarshal([]byte(base), &state); err != nil {
			t.Fatal(err)
		}
		for _, fragment := range updates {
			var update T
			if err := json.Unmarshal([]byte(merged(t, base, fragment)), &update); err != nil {
				t.Fatalf("decode %s: %v", fragment, err)
			}
			P(&state).ApplyUpdate(update)
		}
		return fieldsOf(t, state)
	}}
}

// The top-level _meta of each of these is a metadata patch.
var upserts = []upsertCase{
	upsertOf[ToolCallUpdate]("tool_call_update", `{"toolCallId":"call_001"}`),
	upsertOf[TerminalUpdate]("terminal_update", `{"terminalId":"term_001"}`),
	upsertOf[UserMessage]("user_message", `{"messageId":"msg_001"}`),
	upsertOf[AgentMessage]("agent_message", `{"messageId":"msg_001"}`),
	upsertOf[AgentThought]("agent_thought", `{"messageId":"msg_001"}`),
	upsertOf[SessionInfoUpdate]("session_info_update", `{}`),
	upsertOf[CompactionUpdate]("compaction_update", `{"compactionId":"cmp_001","status":"in_progress"}`),
}

func merged(t *testing.T, base, fragment string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(base), &fields); err != nil {
		t.Fatal(err)
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fragment), &extra); err != nil {
		t.Fatalf("fixture is not a JSON object: %v\n%s", err, fragment)
	}
	for key, value := range extra {
		fields[key] = value
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func fieldsOf(t *testing.T, value any) map[string]json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

// noMeta stands for no stored metadata, and for an omitted incoming _meta.
const noMeta = ""

// storedMeta returns the canonical stored metadata. Absent, null, and an
// object with no keys all mean no metadata.
func storedMeta(t *testing.T, fields map[string]json.RawMessage) string {
	t.Helper()
	raw, ok := fields["_meta"]
	if !ok {
		return noMeta
	}
	switch got := canonical(t, string(raw)); got {
	case "null", "{}":
		return noMeta
	default:
		return got
	}
}

func metaUpdate(meta string) string {
	if meta == noMeta {
		return `{}`
	}
	return `{"_meta":` + meta + `}`
}

func wantMeta(t *testing.T, want string) string {
	if want == noMeta {
		return noMeta
	}
	return canonical(t, want)
}

// TestMetaPatchConformance runs the conformance table of the v2 Metadata
// Patches RFD, plus unknown keys, against every participating update.
func TestMetaPatchConformance(t *testing.T) {
	cases := []struct {
		name, stored, incoming, want string
	}{
		{"omitted", `{"a":1,"b":2}`, noMeta, `{"a":1,"b":2}`},
		{"null clears", `{"a":1,"b":2}`, `null`, noMeta},
		{"empty object", `{"a":1,"b":2}`, `{}`, `{"a":1,"b":2}`},
		{"value replaces key", `{"a":1,"b":2}`, `{"a":3}`, `{"a":3,"b":2}`},
		{"null removes key", `{"a":1,"b":2}`, `{"a":null}`, `{"b":2}`},
		{"removing absent key", noMeta, `{"a":null}`, noMeta},
		{"nested objects not merged", `{"a":{"x":1,"y":2}}`, `{"a":{"x":3}}`, `{"a":{"x":3}}`},
		{"arrays replaced", `{"a":[1,2]}`, `{"a":[3]}`, `{"a":[3]}`},
		{"nested null is data", noMeta, `{"a":{"x":null}}`, `{"a":{"x":null}}`},
		{
			"unknown keys kept verbatim",
			`{"com.example/a":1,"traceparent":"00-80e1afed08e019fc1110464cfa66635c-7a085853722dc6d2-01"}`,
			`{"future.example/b":{"deep":[{"x":null}]},"_reserved":false}`,
			`{"com.example/a":1,"traceparent":"00-80e1afed08e019fc1110464cfa66635c-7a085853722dc6d2-01","future.example/b":{"deep":[{"x":null}]},"_reserved":false}`,
		},
		{"empty values are values", `{"a":1}`, `{"a":{},"b":[],"c":"","d":0,"e":false}`, `{"a":{},"b":[],"c":"","d":0,"e":false}`},
	}
	for _, entity := range upserts {
		for _, tc := range cases {
			t.Run(entity.name+"/"+tc.name, func(t *testing.T) {
				var updates []string
				if tc.stored != noMeta {
					updates = append(updates, metaUpdate(tc.stored))
				}
				updates = append(updates, metaUpdate(tc.incoming))
				if got, want := storedMeta(t, entity.fold(t, updates...)), wantMeta(t, tc.want); got != want {
					t.Fatalf("stored _meta = %q, want %q", got, want)
				}
			})
		}
	}
}

func TestMetaPatchSequences(t *testing.T) {
	cases := []struct {
		name    string
		updates []string
		want    string
	}{
		{"clear then object", []string{`{"a":1,"b":2}`, `null`, `{"c":3}`}, `{"c":3}`},
		{"object after clear starts empty", []string{`{"a":1}`, `null`, `{"b":2,"a":null}`}, `{"b":2}`},
		{"writers keep each other's keys", []string{`{"zed.dev/a":1,"acme.dev/b":{"x":1}}`, `{"proxy.dev/c":true}`, `{"zed.dev/a":null,"acme.dev/b":{"y":2}}`}, `{"acme.dev/b":{"y":2},"proxy.dev/c":true}`},
		{"removing every key", []string{`{"a":1}`, `{"a":null}`}, noMeta},
		{"rfd example", []string{`{"com.example/progress":0.5,"org.example/review":{"state":"pending"}}`, `{"com.example/progress":null,"org.example/review":{"reviewer":"sam"},"net.example/route":"edge"}`}, `{"org.example/review":{"reviewer":"sam"},"net.example/route":"edge"}`},
	}
	for _, entity := range upserts {
		for _, tc := range cases {
			t.Run(entity.name+"/"+tc.name, func(t *testing.T) {
				updates := make([]string, len(tc.updates))
				for i, meta := range tc.updates {
					updates[i] = metaUpdate(meta)
				}
				if got, want := storedMeta(t, entity.fold(t, updates...)), wantMeta(t, tc.want); got != want {
					t.Fatalf("stored _meta = %q, want %q", got, want)
				}
			})
		}
	}
}

// TestMetaPatchReplay covers replay: the original updates replayed in order
// reproduce the stored state, including removals, and replay never resets
// metadata implicitly, so stale keys survive unless the sender removes them.
func TestMetaPatchReplay(t *testing.T) {
	original := []string{
		`{"_meta":{"a":1,"b":2}}`,
		`{"_meta":{"a":null,"c":3}}`,
		`{"_meta":{"b":{"v":2}}}`,
	}
	const want = `{"b":{"v":2},"c":3}`
	stale := `{"_meta":{"a":"stale","stale":true}}`
	for _, entity := range upserts {
		t.Run(entity.name, func(t *testing.T) {
			live := entity.fold(t, original...)
			replayed := entity.fold(t, original...)
			if !reflect.DeepEqual(live, replayed) || storedMeta(t, replayed) != canonical(t, want) {
				t.Fatalf("replay = %s, live = %s", replayed["_meta"], live["_meta"])
			}

			// Replaying over stale metadata keeps keys nobody removes.
			over := entity.fold(t, append([]string{stale}, original...)...)
			if got := storedMeta(t, over); got != canonical(t, `{"b":{"v":2},"c":3,"stale":true}`) {
				t.Fatalf("replay over stale metadata = %s", got)
			}
			// The sender resynchronizes by removing stale keys with null, or
			// by clearing before it replays.
			removed := entity.fold(t, append(append([]string{stale}, original...), `{"_meta":{"stale":null}}`)...)
			if got := storedMeta(t, removed); got != canonical(t, want) {
				t.Fatalf("resync by removal = %s", got)
			}
			cleared := entity.fold(t, append([]string{stale, `{"_meta":null}`}, original...)...)
			if got := storedMeta(t, cleared); got != canonical(t, want) {
				t.Fatalf("resync by clear = %s", got)
			}
		})
	}
}

// Ported from Rust's apply_meta_update tests in schema v2 ext.rs.
func TestApplyMetaUpdate(t *testing.T) {
	meta := func(doc string) Nullable[Meta] {
		var value Nullable[Meta]
		if err := json.Unmarshal([]byte(doc), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	encode := func(value Nullable[Meta]) string {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return canonical(t, string(encoded))
	}

	stored := meta(`{"zed.dev/kept":1,"zed.dev/removed":true,"acme.dev/nested":{"a":1,"b":2},"acme.dev/list":[1,2]}`)
	ApplyMetaUpdate(&stored, meta(`{"zed.dev/removed":null,"zed.dev/absent":null,"acme.dev/nested":{"b":3,"c":null},"acme.dev/list":[3],"acme.dev/added":[]}`))
	if got, want := encode(stored), canonical(t, `{"zed.dev/kept":1,"acme.dev/nested":{"b":3,"c":null},"acme.dev/list":[3],"acme.dev/added":[]}`); got != want {
		t.Fatalf("patched = %s, want %s", got, want)
	}

	original := meta(`{"zed.dev/kept":1}`)
	stored = meta(`{"zed.dev/kept":1}`)
	ApplyMetaUpdate(&stored, Nullable[Meta]{})
	if !reflect.DeepEqual(stored, original) {
		t.Fatalf("omitted patch changed metadata: %+v", stored)
	}
	ApplyMetaUpdate(&stored, meta(`{}`))
	if !reflect.DeepEqual(stored, original) {
		t.Fatalf("empty patch changed metadata: %+v", stored)
	}
	ApplyMetaUpdate(&stored, meta(`null`))
	if !stored.Set || !stored.Null {
		t.Fatalf("null patch did not clear metadata: %+v", stored)
	}

	for _, initial := range []Nullable[Meta]{{}, {Set: true, Null: true}} {
		stored := initial
		ApplyMetaUpdate(&stored, meta(`{"zed.dev/added":1,"zed.dev/removed":null}`))
		if got := encode(stored); got != canonical(t, `{"zed.dev/added":1}`) {
			t.Fatalf("patch from %+v = %s", initial, got)
		}
	}
}

// A patch built in Go behaves like the same patch decoded from the wire: any
// value that encodes as JSON null removes its key. Neither map is modified.
func TestApplyMetaUpdateGoValues(t *testing.T) {
	storedMap := Meta{"nil": 1, "slice": 2, "map": 3, "pointer": 4, "raw": 5, "kept": 6}
	stored := Nullable[Meta]{Set: true, Value: storedMap}
	patch := Meta{
		"nil":     nil,
		"slice":   []string(nil),
		"map":     map[string]int(nil),
		"pointer": (*int)(nil),
		"raw":     json.RawMessage(" null "),
		"nested":  map[string]any{"x": nil},
		"empty":   []string{},
	}
	ApplyMetaUpdate(&stored, Nullable[Meta]{Set: true, Value: patch})

	if len(storedMap) != 6 || len(patch) != 7 {
		t.Fatalf("inputs modified: stored %v, patch %v", storedMap, patch)
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := canonical(t, string(encoded)), canonical(t, `{"kept":6,"nested":{"x":null},"empty":[]}`); got != want {
		t.Fatalf("patched = %s, want %s", got, want)
	}
}

// fieldPatch is one patch field of an upsert: its value in a populated
// entity and other values that must each replace it whole.
type fieldPatch struct {
	field  string
	others []string
}

// TestUpsertFieldsDistinguishOmittedNullAndValue checks every patch field,
// including the alpha.10 to alpha.13 additions (capabilities, size): omission
// leaves the stored value unchanged, null is stored as null, and a value,
// even an empty one or a custom enum value, replaces the stored value whole.
// Field patches never touch the stored metadata.
func TestUpsertFieldsDistinguishOmittedNullAndValue(t *testing.T) {
	cases := []struct {
		entity    upsertCase
		populated string
		fields    []fieldPatch
	}{
		{
			upserts[0],
			`{"toolCallId":"call_001","name":"read_file","title":"Reading configuration","kind":"read","status":"in_progress",
			  "content":[{"type":"content","content":{"type":"text","text":"old","_meta":{"source":"tool"}}}],
			  "locations":[{"path":"/workspace/config.json","line":3}],"rawInput":{"path":"/workspace/config.json"},
			  "rawOutput":{"text":"old"},"capabilities":{"cancel":{}}}`,
			[]fieldPatch{
				{"name", []string{`"write_file"`}},
				{"title", []string{`"Writing"`}},
				{"kind", []string{`"edit"`, `"_custom_kind"`}},
				{"status", []string{`"completed"`, `"deferred"`, `"_awaiting_review"`}},
				{"content", []string{`[]`}},
				{"locations", []string{`[]`}},
				{"rawInput", []string{`{}`, `[1]`, `"text"`}},
				{"rawOutput", []string{`{}`, `false`}},
				{"capabilities", []string{`{}`}},
			},
		},
		{
			upserts[1],
			`{"terminalId":"term_001","command":"cargo test","cwd":"/workspace","output":{"data":"b2xk"},
			  "exitStatus":{"exitCode":0},"size":{"cols":120,"rows":40},"capabilities":{"cancel":{}}}`,
			[]fieldPatch{
				{"command", []string{`"ls"`}},
				{"cwd", []string{`"/tmp"`}},
				{"output", []string{`{"data":""}`}},
				{"exitStatus", []string{`{}`, `{"signal":"SIGTERM"}`}},
				{"size", []string{`{"cols":80,"rows":24}`}},
				{"capabilities", []string{`{}`}},
			},
		},
		{upserts[2], `{"messageId":"msg_001","content":[{"type":"text","text":"hi"}]}`, []fieldPatch{{"content", []string{`[]`}}}},
		{upserts[3], `{"messageId":"msg_001","content":[{"type":"text","text":"hi"}]}`, []fieldPatch{{"content", []string{`[]`}}}},
		{upserts[4], `{"messageId":"msg_001","content":[{"type":"text","text":"hi"}]}`, []fieldPatch{{"content", []string{`[]`}}}},
		{
			upserts[5],
			`{"title":"Session","updatedAt":"2026-01-01T00:00:00Z"}`,
			[]fieldPatch{{"title", []string{`"Renamed"`}}, {"updatedAt", []string{`"2026-02-01T00:00:00Z"`}}},
		},
		{
			upserts[6],
			`{"compactionId":"cmp_001","status":"in_progress","summary":[{"type":"text","text":"sum"}],"error":"boom"}`,
			[]fieldPatch{{"summary", []string{`[]`}}, {"error", []string{`"other"`}}},
		},
	}
	const meta = `{"_meta":{"keep":true}}`
	for _, tc := range cases {
		t.Run(tc.entity.name, func(t *testing.T) {
			updates := []string{meta, tc.populated}
			expected := tc.entity.fold(t, updates...)
			for key, value := range fieldsOfDoc(t, tc.populated) {
				if canonical(t, string(expected[key])) != canonical(t, string(value)) {
					t.Fatalf("populating %s stored %s, want %s", key, expected[key], value)
				}
			}
			for _, patch := range tc.fields {
				original := string(expected[patch.field])
				for _, replacement := range append(append([]string{}, patch.others...), `null`, original) {
					updates = append(updates, `{"`+patch.field+`":`+replacement+`}`)
					expected[patch.field] = json.RawMessage(replacement)
					assertFields(t, tc.entity.fold(t, updates...), expected, "patching "+patch.field+" to "+replacement)

					updates = append(updates, `{}`)
					assertFields(t, tc.entity.fold(t, updates...), expected, "omitting "+patch.field)
				}
			}
		})
	}
}

func fieldsOfDoc(t *testing.T, doc string) map[string]json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func assertFields(t *testing.T, got, want map[string]json.RawMessage, step string) {
	t.Helper()
	encode := func(fields map[string]json.RawMessage) string {
		encoded, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return canonical(t, string(encoded))
	}
	if encode(got) != encode(want) {
		t.Fatalf("%s: stored %s, want %s", step, encode(got), encode(want))
	}
}

// A first update holding only metadata must not invent other fields.
func TestUpsertMetadataOnlyFirstUpdate(t *testing.T) {
	for _, entity := range upserts {
		got := entity.fold(t, `{"_meta":{"source":"replay"}}`)
		for key := range got {
			switch key {
			case "_meta", "toolCallId", "terminalId", "messageId", "compactionId", "status":
			default:
				t.Fatalf("%s: metadata-only update stored %s", entity.name, key)
			}
		}
	}
}

// Patch fields written as anyOf [$ref, null] in the schema keep null distinct
// from omission on the wire, including UsageUpdate.cost.
func TestPatchFieldsRoundTripNull(t *testing.T) {
	for _, tc := range []struct {
		value any
		docs  []string
	}{
		{&ToolCallUpdate{}, []string{
			`{"toolCallId":"c"}`,
			`{"toolCallId":"c","kind":null,"status":null,"capabilities":null,"rawInput":null,"rawOutput":null}`,
		}},
		{&TerminalUpdate{}, []string{
			`{"terminalId":"t"}`,
			`{"terminalId":"t","cwd":null,"output":null,"exitStatus":null,"size":null,"capabilities":null}`,
		}},
		{&UsageUpdate{}, []string{
			`{"used":1,"size":2}`,
			`{"used":1,"size":2,"cost":null}`,
			`{"used":1,"size":2,"cost":{"amount":0.5,"currency":"USD"}}`,
		}},
	} {
		for _, doc := range tc.docs {
			value := reflect.New(reflect.TypeOf(tc.value).Elem()).Interface()
			if err := json.Unmarshal([]byte(doc), value); err != nil {
				t.Fatalf("decode %s: %v", doc, err)
			}
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if canonical(t, string(encoded)) != canonical(t, doc) {
				t.Fatalf("%s re-encoded as %s", doc, encoded)
			}
		}
	}
}
