package acp

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go/schema"
)

// observeWire feeds session/update notifications for session "s" through the
// projection's notification adapter.
func observeWire(t *testing.T, projection *SessionProjection, updates ...string) {
	t.Helper()
	notifications := projection.Notifications(nil)
	for _, update := range updates {
		raw := json.RawMessage(`{"sessionId":"s","update":` + update + `}`)
		if err := notifications(schema.SessionUpdateMethodName, raw); err != nil {
			t.Fatalf("observe %s: %v", update, err)
		}
	}
}

func summaryText(blocks []schema.ContentBlock) string {
	var text strings.Builder
	for _, block := range blocks {
		if block.Text != nil {
			text.WriteString(block.Text.Text)
		}
	}
	return text.String()
}

func onlyCompaction(t *testing.T, projection *SessionProjection, id schema.CompactionId) CompactionState {
	t.Helper()
	compaction, ok := projection.Compaction("s", id)
	if !ok {
		t.Fatalf("compaction %q missing", id)
	}
	return compaction
}

func TestSessionProjectionCompactionPatchesAndChunks(t *testing.T) {
	projection := NewSessionProjection()
	text := func(s string) string { return `{"type":"text","text":"` + s + `"}` }

	// A first-seen chunk creates the compaction in progress.
	observeWire(t, projection, `{"sessionUpdate":"compaction_summary_chunk","compactionId":"c","content":`+text("A")+`}`)
	compaction := onlyCompaction(t, projection, "c")
	if compaction.Status != schema.CompactionStatusInProgress || summaryText(compaction.Summary) != "A" {
		t.Fatalf("first-seen chunk = %+v", compaction)
	}

	steps := []struct {
		update  string
		status  schema.CompactionStatus
		summary string
		error   string
		meta    schema.Meta
	}{
		// Omitted patch fields keep the stored value; chunks append.
		{`{"sessionUpdate":"compaction_update","compactionId":"c","status":"in_progress","error":"retrying","_meta":{"a":1}}`, "in_progress", "A", "retrying", schema.Meta{"a": float64(1)}},
		{`{"sessionUpdate":"compaction_summary_chunk","compactionId":"c","content":` + text("B") + `,"_meta":{"chunk":true}}`, "in_progress", "AB", "retrying", schema.Meta{"a": float64(1)}},
		// A concrete summary replaces everything streamed so far, and a
		// _meta object replaces the stored metadata whole.
		{`{"sessionUpdate":"compaction_update","compactionId":"c","status":"in_progress","summary":[` + text("S") + `],"_meta":{"b":2}}`, "in_progress", "S", "retrying", schema.Meta{"b": float64(2)}},
		{`{"sessionUpdate":"compaction_summary_chunk","compactionId":"c","content":` + text("T") + `}`, "in_progress", "ST", "retrying", schema.Meta{"b": float64(2)}},
		// null clears, including partial output before failure.
		{`{"sessionUpdate":"compaction_update","compactionId":"c","status":"failed","summary":null,"error":null,"_meta":null}`, "failed", "", "", nil},
		{`{"sessionUpdate":"compaction_summary_chunk","compactionId":"c","content":` + text("U") + `}`, "failed", "U", "", nil},
		// summary: [] also clears; unknown statuses are kept as received.
		{`{"sessionUpdate":"compaction_update","compactionId":"c","status":"_paused","summary":[]}`, "_paused", "", "", nil},
		{`{"sessionUpdate":"compaction_summary_chunk","compactionId":"c","content":` + text("V") + `}`, "_paused", "V", "", nil},
	}
	for _, step := range steps {
		observeWire(t, projection, step.update)
		compaction := onlyCompaction(t, projection, "c")
		errorText := ""
		if compaction.Error != nil {
			errorText = *compaction.Error
		}
		if compaction.Status != step.status || summaryText(compaction.Summary) != step.summary ||
			errorText != step.error || !reflect.DeepEqual(compaction.Meta, step.meta) {
			t.Fatalf("after %s: %+v (error %q)", step.update, compaction, errorText)
		}
	}
	observeWire(t, projection, `{"sessionUpdate":"compaction_update","compactionId":"c","status":"completed","summary":[]}`)
	if compaction := onlyCompaction(t, projection, "c"); compaction.Summary != nil {
		t.Fatalf("summary: [] left %+v", compaction.Summary)
	}
}

func TestSessionProjectionCompactionsKeepFirstSeenOrderAndReplay(t *testing.T) {
	updates := []string{
		`{"sessionUpdate":"compaction_summary_chunk","compactionId":"second","content":{"type":"text","text":"x"}}`,
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"ignored"}}`,
		`{"sessionUpdate":"compaction_update","compactionId":"first","status":"in_progress"}`,
		`{"sessionUpdate":"compaction_update","compactionId":"second","status":"completed","summary":[{"type":"text","text":"done"}]}`,
		`{"sessionUpdate":"compaction_update","compactionId":"first","status":"cancelled"}`,
	}
	live := NewSessionProjection()
	observeWire(t, live, updates...)
	compactions := live.Compactions("s")
	if len(compactions) != 2 || compactions[0].ID != "second" || compactions[1].ID != "first" {
		t.Fatalf("first-seen order = %+v", compactions)
	}
	if compactions[0].Status != schema.CompactionStatusCompleted || summaryText(compactions[0].Summary) != "done" ||
		compactions[1].Status != schema.CompactionStatusCancelled {
		t.Fatalf("compactions = %+v", compactions)
	}

	// session/load replays through the same path and reproduces the state.
	replay := NewSessionProjection()
	observeWire(t, replay, updates...)
	if !reflect.DeepEqual(replay.Compactions("s"), compactions) {
		t.Fatalf("replay %+v differs from live %+v", replay.Compactions("s"), compactions)
	}
	// Replay into existing state does not reset omitted fields; an agent
	// clears the summary before re-streaming it to avoid duplication.
	observeWire(t, replay,
		`{"sessionUpdate":"compaction_update","compactionId":"second","status":"completed","summary":null}`,
		`{"sessionUpdate":"compaction_summary_chunk","compactionId":"second","content":{"type":"text","text":"again"}}`,
		`{"sessionUpdate":"compaction_update","compactionId":"first","status":"cancelled"}`,
	)
	compactions = replay.Compactions("s")
	if len(compactions) != 2 || compactions[0].ID != "second" || summaryText(compactions[0].Summary) != "again" {
		t.Fatalf("replay into existing state = %+v", compactions)
	}

	replay.Forget("s")
	if compactions := replay.Compactions("s"); len(compactions) != 0 {
		t.Fatalf("Forget kept %+v", compactions)
	}
}

func TestSessionProjectionNoticesAreLiveEvents(t *testing.T) {
	projection := NewSessionProjection()
	observeWire(t, projection,
		`{"sessionUpdate":"notice","severity":"info","title":"first"}`,
		`{"sessionUpdate":"notice","severity":"_critical","title":"second","description":"custom severity","_meta":{"k":"v"}}`,
	)
	notices := projection.Notices("s")
	if len(notices) != 2 || notices[0].Title != "first" || notices[1].Severity != "_critical" ||
		notices[1].Description == nil || *notices[1].Description != "custom severity" {
		t.Fatalf("notices = %+v", notices)
	}
	if len(projection.Notices("other")) != 0 {
		t.Fatal("notices leaked across sessions")
	}
	// Notices peeks; DrainNotices consumes.
	if drained := projection.DrainNotices("s"); !reflect.DeepEqual(drained, notices) {
		t.Fatalf("drained = %+v", drained)
	}
	if notices := projection.Notices("s"); len(notices) != 0 {
		t.Fatalf("notices after drain = %+v", notices)
	}
	observeWire(t, projection, `{"sessionUpdate":"notice","severity":"error","title":"third"}`)
	if notices := projection.DrainNotices("s"); len(notices) != 1 || notices[0].Title != "third" {
		t.Fatalf("notices after drain = %+v", notices)
	}
	// Notices are not compactions.
	if len(projection.Compactions("s")) != 0 {
		t.Fatal("notice created a compaction")
	}
}

// The projection never writes into an update's backing array or into a
// value it has returned.
func TestSessionProjectionDoesNotAlias(t *testing.T) {
	projection := NewSessionProjection()
	summary := make([]schema.ContentBlock, 1, 4)
	summary[0] = NewTextContent("A")
	backing := summary[:4]
	projection.Observe("s", schema.SessionUpdate{CompactionUpdate: &schema.CompactionUpdate{
		CompactionID: "c", Status: schema.CompactionStatusInProgress,
		Summary: schema.Nullable[[]schema.ContentBlock]{Set: true, Value: summary},
	}})
	before := onlyCompaction(t, projection, "c")
	projection.Observe("s", schema.SessionUpdate{CompactionSummaryChunk: &schema.CompactionSummaryChunk{
		CompactionID: "c", Content: NewTextContent("B"),
	}})
	if backing[1].Text != nil {
		t.Fatal("chunk was written into the update's backing array")
	}
	if len(before.Summary) != 1 || cap(before.Summary) != 1 {
		t.Fatalf("returned summary changed: %+v", before.Summary)
	}
	if summary := onlyCompaction(t, projection, "c").Summary; summaryText(summary) != "AB" {
		t.Fatalf("summary = %q", summaryText(summary))
	}
}

func TestSessionProjectionNotificationsAdapter(t *testing.T) {
	projection := NewSessionProjection()
	var methods []string
	notifications := projection.Notifications(func(method string, _ json.RawMessage) error {
		// The projection has already observed the update.
		if method == schema.SessionUpdateMethodName && len(projection.Notices("s")) != 1 {
			t.Error("next ran before the projection observed the update")
		}
		methods = append(methods, method)
		return nil
	})
	if err := notifications(schema.SessionUpdateMethodName, json.RawMessage(`{"sessionId":"s","update":{"sessionUpdate":"notice","severity":"info","title":"hi"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := notifications("custom/notification", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := notifications(schema.SessionUpdateMethodName, json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed session/update was accepted")
	}
	if !reflect.DeepEqual(methods, []string{schema.SessionUpdateMethodName, "custom/notification"}) {
		t.Fatalf("next saw %v", methods)
	}
}

func TestSessionClientCapabilitiesAdvertiseNoticesAndCompaction(t *testing.T) {
	configOptions := ConfigOptionsClientCapabilities(true)
	configOptions.Meta = schema.Meta{"a": 1}
	capabilities := Capabilities{Session: SessionClientCapabilities(
		configOptions, nil, SessionNoticesClientCapabilities(), SessionCompactionClientCapabilities(),
		&schema.ClientSessionCapabilities{Meta: schema.Meta{"b": 2}},
	)}
	encoded, err := json.Marshal(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false,"session":{"compaction":{},"configOptions":{"boolean":{}},"notices":{},"_meta":{"a":1,"b":2}},"auth":{"terminal":false}}`
	if string(encoded) != want {
		t.Fatalf("capabilities = %s, want %s", encoded, want)
	}
	if len(configOptions.Meta) != 1 {
		t.Fatalf("a part was modified: %+v", configOptions.Meta)
	}
	if encoded, _ := json.Marshal(SessionNoticesClientCapabilities()); string(encoded) != `{"notices":{}}` {
		t.Fatalf("notices = %s", encoded)
	}
	if encoded, _ := json.Marshal(SessionCompactionClientCapabilities()); string(encoded) != `{"compaction":{}}` {
		t.Fatalf("compaction = %s", encoded)
	}
}

func TestRequireSessionUpdateCapability(t *testing.T) {
	notice := schema.SessionUpdate{Notice: &schema.Notice{Severity: schema.NoticeSeverityInfo, Title: "t"}}
	compaction := schema.SessionUpdate{CompactionUpdate: &schema.CompactionUpdate{CompactionID: "c", Status: schema.CompactionStatusCompleted}}
	chunk := schema.SessionUpdate{CompactionSummaryChunk: &schema.CompactionSummaryChunk{CompactionID: "c", Content: NewTextContent("x")}}
	message := schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{Content: NewTextContent("x")}}
	// Omitting session, or a null session or field, does not advertise.
	var decoded Capabilities
	if err := json.Unmarshal([]byte(`{"session":{"notices":null,"compaction":null}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	for name, capabilities := range map[string]Capabilities{
		"omitted": {},
		"null":    decoded,
		"other":   {Session: ConfigOptionsClientCapabilities(false)},
	} {
		for _, update := range []schema.SessionUpdate{notice, compaction, chunk} {
			if err := RequireSessionUpdateCapability(capabilities, update); err == nil {
				t.Errorf("%s: %+v was allowed", name, update)
			}
		}
		if err := RequireSessionUpdateCapability(capabilities, message); err != nil {
			t.Errorf("%s: agent message refused: %v", name, err)
		}
	}
	if err := RequireSessionUpdateCapability(Capabilities{Session: SessionNoticesClientCapabilities()}, notice); err != nil {
		t.Fatal(err)
	}
	err := RequireSessionUpdateCapability(Capabilities{Session: SessionNoticesClientCapabilities()}, chunk)
	// The refusal has the agent runtime's host-method gate shape.
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 || rpcErr.Message != "client did not advertise session.compaction support" {
		t.Fatalf("chunk without compaction = %#v", err)
	}
	for _, update := range []schema.SessionUpdate{compaction, chunk} {
		if err := RequireSessionUpdateCapability(Capabilities{Session: SessionCompactionClientCapabilities()}, update); err != nil {
			t.Fatal(err)
		}
	}
}

// A config_option_update with a boolean option needs
// session.configOptions.boolean, whether the option was built in Go or
// decoded from the wire; select-only updates need no capability.
func TestRequireSessionUpdateCapabilityGatesBooleanConfigOptions(t *testing.T) {
	selectOnly := schema.SessionUpdate{ConfigOptionUpdate: &schema.ConfigOptionUpdate{ConfigOptions: []schema.SessionConfigOption{{
		ID: "model", Name: "Model", Select: &schema.SessionConfigSelect{
			CurrentValue: "large", Options: []schema.SessionConfigSelectOption{{Value: "large", Name: "Large"}},
		},
	}}}}
	built := schema.SessionUpdate{ConfigOptionUpdate: &schema.ConfigOptionUpdate{ConfigOptions: []schema.SessionConfigOption{{
		ID: "fast", Name: "Fast", Boolean: &schema.SessionConfigBoolean{CurrentValue: true},
	}}}}
	var decoded schema.SessionUpdate
	if err := json.Unmarshal([]byte(`{"sessionUpdate":"config_option_update","configOptions":[{"id":"fast","name":"Fast","type":"boolean","currentValue":false}]}`), &decoded); err != nil {
		t.Fatal(err)
	}
	for name, capabilities := range map[string]Capabilities{
		"omitted":     {},
		"select only": {Session: ConfigOptionsClientCapabilities(false)},
		"notices":     {Session: SessionNoticesClientCapabilities()},
	} {
		if err := RequireSessionUpdateCapability(capabilities, selectOnly); err != nil {
			t.Errorf("%s: select-only update refused: %v", name, err)
		}
		for _, update := range []schema.SessionUpdate{built, decoded} {
			err := RequireSessionUpdateCapability(capabilities, update)
			var rpcErr *RPCError
			if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 || rpcErr.Message != "client did not advertise session.configOptions.boolean support" {
				t.Errorf("%s: boolean update = %#v", name, err)
			}
		}
	}
	advertised := Capabilities{Session: ConfigOptionsClientCapabilities(true)}
	for _, update := range []schema.SessionUpdate{selectOnly, built, decoded} {
		if err := RequireSessionUpdateCapability(advertised, update); err != nil {
			t.Errorf("advertised boolean: %v", err)
		}
	}
}

// SessionClientCapabilities copies Meta, Compaction, ConfigOptions, and
// Notices. A schema pin that adds a session capability fails here until the
// combiner copies it too.
func TestSessionClientCapabilitiesCopiesEveryField(t *testing.T) {
	var got []string
	typ := reflect.TypeFor[schema.ClientSessionCapabilities]()
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	want := []string{"Compaction", "ConfigOptions", "Notices", "Meta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schema.ClientSessionCapabilities fields = %v, SessionClientCapabilities copies %v", got, want)
	}
	part := schema.ClientSessionCapabilities{
		Meta:          schema.Meta{"k": "v"},
		Compaction:    &schema.CompactionCapabilities{},
		ConfigOptions: &schema.SessionConfigOptionsCapabilities{},
		Notices:       &schema.NoticeCapabilities{},
	}
	combined := reflect.ValueOf(*SessionClientCapabilities(&part))
	for i := range combined.NumField() {
		if combined.Field(i).IsZero() {
			t.Errorf("SessionClientCapabilities dropped %s", typ.Field(i).Name)
		}
	}
}

// session_info_update's title and updatedAt are patches, as in Rust's
// MaybeUndefined: omitted keeps the stored value, null clears it, and a
// value replaces it. _meta stays a plain optional.
func TestSessionInfoUpdateKeepsNullDistinctFromOmission(t *testing.T) {
	for _, test := range []struct {
		info schema.SessionInfoUpdate
		wire string
	}{
		{schema.SessionInfoUpdate{}, `{"sessionId":"s","update":{"sessionUpdate":"session_info_update"}}`},
		{schema.SessionInfoUpdate{Title: schema.Nullable[string]{Set: true, Null: true}}, `{"sessionId":"s","update":{"sessionUpdate":"session_info_update","title":null}}`},
		{schema.SessionInfoUpdate{
			Title:     schema.Nullable[string]{Set: true, Value: "Release"},
			UpdatedAt: schema.Nullable[string]{Set: true, Null: true},
		}, `{"sessionId":"s","update":{"sessionUpdate":"session_info_update","title":"Release","updatedAt":null}}`},
	} {
		encoded, err := json.Marshal(NewSessionInfoUpdate("s", test.info))
		if err != nil || string(encoded) != test.wire {
			t.Fatalf("encoded = %s, %v; want %s", encoded, err, test.wire)
		}
		var decoded Update
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		got := *decoded.Update.SessionInfoUpdate
		if got.Title != test.info.Title || got.UpdatedAt != test.info.UpdatedAt || got.Meta != nil {
			t.Fatalf("decoded = %+v, want %+v", got, test.info)
		}
	}
	var info schema.SessionInfoUpdate
	if err := json.Unmarshal([]byte(`{"_meta":null}`), &info); err != nil || info.Meta != nil {
		t.Fatalf("_meta null = %+v, %v", info, err)
	}
}
