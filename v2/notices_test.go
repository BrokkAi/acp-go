package v2

import (
	"encoding/json"
	"reflect"
	"testing"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

func TestUpdateProjectionNoticesAreLiveEvents(t *testing.T) {
	projection := NewUpdateProjection()
	applyWire(t, projection,
		`{"sessionUpdate":"notice","severity":"warning","title":"first"}`,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m","content":{"type":"text","text":"hi"}}`,
		`{"sessionUpdate":"notice","severity":"_critical","title":"second","description":null,"_meta":{"k":"v"}}`,
	)
	notices := projection.Notices()
	if len(notices) != 2 || notices[0].Title != "first" || notices[1].Severity != "_critical" ||
		!notices[1].Description.Null {
		t.Fatalf("notices = %+v", notices)
	}
	// Notices are not entities.
	if timeline := projection.Timeline(); !reflect.DeepEqual(timeline, []EntityRef{{EntityAgentMessage, "m"}}) {
		t.Fatalf("timeline = %v", timeline)
	}
	if drained := projection.DrainNotices(); !reflect.DeepEqual(drained, notices) {
		t.Fatalf("drained = %+v", drained)
	}
	if notices := projection.Notices(); len(notices) != 0 {
		t.Fatalf("notices after drain = %+v", notices)
	}
	applyWire(t, projection, `{"sessionUpdate":"notice","severity":"error","title":"third"}`)
	if notices := projection.DrainNotices(); len(notices) != 1 || notices[0].Title != "third" {
		t.Fatalf("notices = %+v", notices)
	}

	// The text-only work projection ignores notices.
	text := newTextProjection()
	applyWire(t, text, `{"sessionUpdate":"notice","severity":"info","title":"ignored"}`)
	if len(text.Notices()) != 0 {
		t.Fatal("text projection stored a notice")
	}
}

func TestUpdateProjectionCompactionsInFirstSeenOrder(t *testing.T) {
	text := func(s string) string { return `{"type":"text","text":"` + s + `"}` }
	updates := []string{
		`{"sessionUpdate":"compaction_summary_chunk","compactionId":"second","content":` + text("A") + `}`,
		`{"sessionUpdate":"compaction_update","compactionId":"first","status":"in_progress","summary":[` + text("S") + `]}`,
		`{"sessionUpdate":"compaction_update","compactionId":"second","status":"failed","summary":null}`,
		`{"sessionUpdate":"compaction_summary_chunk","compactionId":"second","content":` + text("B") + `}`,
		`{"sessionUpdate":"compaction_update","compactionId":"first","status":"completed"}`,
	}
	live := NewUpdateProjection()
	applyWire(t, live, updates...)
	compactions := live.Compactions()
	if len(compactions) != 2 {
		t.Fatalf("compactions = %+v", compactions)
	}
	// null cleared the streamed summary; the later chunk appends to nothing.
	assertWire(t, compactions[0], `{"compactionId":"second","status":"failed","summary":[`+text("B")+`]}`)
	// An omitted summary keeps the stored one.
	assertWire(t, compactions[1], `{"compactionId":"first","status":"completed","summary":[`+text("S")+`]}`)

	replay := NewUpdateProjection()
	applyWire(t, replay, updates...)
	if !reflect.DeepEqual(replay.Compactions(), compactions) {
		t.Fatalf("replay %+v differs from live %+v", replay.Compactions(), compactions)
	}
}

// v2 has no notice or compaction client capability: a client that
// advertises nothing still receives and projects both.
func TestSessionTrackerProjectsNoticesAndCompactionsWithoutCapabilities(t *testing.T) {
	var advertised Capabilities
	if encoded, err := json.Marshal(advertised); err != nil || string(encoded) != `{}` {
		t.Fatalf("capabilities = %s, %v", encoded, err)
	}
	tracker := NewSessionTracker()
	notifications := tracker.TrackerNotifications(nil)
	for _, update := range []Update{
		NewNoticeUpdate("s", schema.Notice{Severity: schema.NoticeSeverityInfo, Title: "heads up"}),
		NewCompactionUpdate("s", schema.CompactionUpdate{CompactionID: "c", Status: schema.CompactionStatusCompleted}),
		NewCompactionSummaryChunkUpdate("s", schema.CompactionSummaryChunk{CompactionID: "c", Content: textBlock("kept")}),
	} {
		raw, err := json.Marshal(update)
		if err != nil {
			t.Fatal(err)
		}
		if err := notifications(schema.SessionUpdateMethodName, raw); err != nil {
			t.Fatal(err)
		}
	}
	projection := tracker.Projection("s")
	if notices := projection.Notices(); len(notices) != 1 || notices[0].Title != "heads up" {
		t.Fatalf("notices = %+v", notices)
	}
	compaction, ok := projection.Compaction("c")
	if !ok {
		t.Fatal("compaction missing")
	}
	assertWire(t, compaction, `{"compactionId":"c","status":"completed","summary":[{"type":"text","text":"kept"}]}`)
}

func TestNoticeAndCompactionConstructorsProduceWireShapes(t *testing.T) {
	for _, test := range []struct {
		update Update
		want   string
	}{
		{NewNoticeUpdate("s", schema.Notice{
			Severity: schema.NoticeSeverityWarning, Title: "Provider degraded",
			Description: schema.Nullable[string]{Set: true, Value: "Retry later"},
		}), `{"sessionId":"s","update":{"sessionUpdate":"notice","severity":"warning","title":"Provider degraded","description":"Retry later"}}`},
		{NewCompactionUpdate("s", schema.CompactionUpdate{
			CompactionID: "c", Status: schema.CompactionStatusCancelled,
			Summary: schema.Nullable[[]schema.ContentBlock]{Set: true, Value: []schema.ContentBlock{}},
			Error:   schema.Nullable[string]{Set: true, Null: true},
			Meta:    schema.Nullable[schema.Meta]{Set: true, Value: schema.Meta{{Key: "k", Value: nil}}},
		}), `{"sessionId":"s","update":{"sessionUpdate":"compaction_update","compactionId":"c","status":"cancelled","summary":[],"error":null,"_meta":{"k":null}}}`},
		{NewCompactionSummaryChunkUpdate("s", schema.CompactionSummaryChunk{CompactionID: "c", Content: textBlock("x")}),
			`{"sessionId":"s","update":{"sessionUpdate":"compaction_summary_chunk","compactionId":"c","content":{"type":"text","text":"x"}}}`},
	} {
		assertWire(t, test.update, test.want)
	}
}
