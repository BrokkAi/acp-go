package unstable_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	stableschema "github.com/BrokkAi/acp-go/schema"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
	acpunstable "github.com/BrokkAi/acp-go/unstable"
)

func TestProjectionPlanLifecycle(t *testing.T) {
	projection := acpunstable.NewProjection()
	const session = "s"
	if _, ok := projection.Plan(session); ok {
		t.Fatal("empty projection reported a plan")
	}
	projection.Observe(session, schema.SessionUpdate{Plan: &schema.Plan{Entries: []schema.PlanEntry{
		{Content: "first", Priority: schema.PlanEntryPriorityHigh, Status: schema.PlanEntryStatusPending},
		{Content: "second", Priority: schema.PlanEntryPriorityLow, Status: schema.PlanEntryStatusCompleted},
	}}})
	plan, ok := projection.Plan(session)
	if !ok || len(plan.Entries) != 2 || plan.Entries[0].Content != "first" || plan.Content != nil {
		t.Fatalf("plan = %+v, %v", plan, ok)
	}

	projection.Observe(session, schema.SessionUpdate{PlanUpdate: &schema.PlanUpdate{Plan: schema.PlanUpdateContent{
		Markdown: &schema.PlanMarkdown{},
	}}})
	plan, ok = projection.Plan(session)
	if !ok || plan.Content == nil || plan.Content.Markdown == nil || plan.Entries != nil {
		t.Fatalf("plan update = %+v, %v", plan, ok)
	}

	projection.Observe(session, schema.SessionUpdate{PlanRemoved: &schema.PlanRemoved{PlanID: "plan-1"}})
	if _, ok := projection.Plan(session); ok {
		t.Fatal("plan_removed left a plan behind")
	}
}

func TestProjectionCompactionLifecycle(t *testing.T) {
	projection := acpunstable.NewProjection()
	const session = "s"
	projection.Observe(session, schema.SessionUpdate{CompactionUpdate: &schema.CompactionUpdate{
		CompactionID: "c1", Status: schema.CompactionStatusInProgress,
	}})
	projection.Observe(session, schema.SessionUpdate{CompactionSummaryChunk: &schema.CompactionSummaryChunk{
		CompactionID: "c1", Content: schema.ContentBlock{Text: &schema.TextContent{Text: "chunk"}},
	}})
	projection.Observe(session, schema.SessionUpdate{CompactionUpdate: &schema.CompactionUpdate{
		CompactionID: "c1", Status: schema.CompactionStatusCompleted,
		Summary: schema.Nullable[[]schema.ContentBlock]{Set: true, Value: []schema.ContentBlock{{Text: &schema.TextContent{Text: "final"}}}},
	}})
	projection.Observe(session, schema.SessionUpdate{CompactionUpdate: &schema.CompactionUpdate{
		CompactionID: "c2", Status: schema.CompactionStatusFailed,
	}})

	compactions := projection.Compactions(session)
	if len(compactions) != 2 {
		t.Fatalf("compactions = %+v", compactions)
	}
	if compactions[0].ID != "c1" || compactions[1].ID != "c2" {
		t.Fatalf("first-seen order = %q, %q", compactions[0].ID, compactions[1].ID)
	}
	if compactions[0].Status != stableschema.CompactionStatusCompleted {
		t.Fatalf("status = %q", compactions[0].Status)
	}
	// The summary field is a complete replacement, so the chunk is gone.
	if len(compactions[0].Summary) != 1 || compactions[0].Summary[0].Text.Text != "final" {
		t.Fatalf("summary = %+v", compactions[0].Summary)
	}
	if compactions[1].Status != stableschema.CompactionStatusFailed {
		t.Fatalf("second status = %q", compactions[1].Status)
	}
}

func TestProjectionNoticesAreLive(t *testing.T) {
	projection := acpunstable.NewProjection()
	const session = "s"
	projection.Observe(session, schema.SessionUpdate{Notice: &schema.Notice{
		Severity: schema.NoticeSeverityInfo, Title: "first",
	}})
	projection.Observe(session, schema.SessionUpdate{Notice: &schema.Notice{
		Severity: schema.NoticeSeverityError, Title: "second",
	}})
	if notices := projection.Notices(session); len(notices) != 2 || notices[0].Title != "first" || notices[1].Title != "second" {
		t.Fatalf("notices = %+v", notices)
	}
	drained := projection.DrainNotices(session)
	if len(drained) != 2 || drained[1].Title != "second" {
		t.Fatalf("drained = %+v", drained)
	}
	if notices := projection.Notices(session); len(notices) != 0 {
		t.Fatalf("notices after drain = %+v", notices)
	}
}

func TestProjectionUsageKeepsLatest(t *testing.T) {
	projection := acpunstable.NewProjection()
	const session = "s"
	if _, ok := projection.Usage(session); ok {
		t.Fatal("empty projection reported usage")
	}
	projection.Observe(session, schema.SessionUpdate{UsageUpdate: &schema.UsageUpdate{Used: 10, Size: 100}})
	projection.Observe(session, schema.SessionUpdate{UsageUpdate: &schema.UsageUpdate{Used: 42, Size: 200}})
	usage, ok := projection.Usage(session)
	if !ok || usage.Used != 42 || usage.Size != 200 {
		t.Fatalf("usage = %+v, %v", usage, ok)
	}
}

// TestProjectionNotificationsAdapter proves the wire path works with an
// unstable-only update kind that the stable v1 facade would not surface.
func TestProjectionNotificationsAdapter(t *testing.T) {
	projection := acpunstable.NewProjection()
	notification := schema.SessionNotification{
		SessionID: "session-1",
		Update: schema.SessionUpdate{CompactionUpdate: &schema.CompactionUpdate{
			CompactionID: "c1",
			Status:       schema.CompactionStatusCompleted,
			Summary:      schema.Nullable[[]schema.ContentBlock]{Set: true, Value: []schema.ContentBlock{{Text: &schema.TextContent{Text: "done"}}}},
		}},
	}
	encoded, err := json.Marshal(notification)
	if err != nil {
		t.Fatal(err)
	}
	notifications := projection.Notifications()
	if err := notifications(schema.SessionUpdateMethodName, encoded); err != nil {
		t.Fatal(err)
	}
	compactions := projection.Compactions("session-1")
	if len(compactions) != 1 || compactions[0].Status != stableschema.CompactionStatusCompleted {
		t.Fatalf("compactions = %+v", compactions)
	}
	if err := notifications("session/cancel", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("unrelated notification = %v", err)
	}
	if err := notifications(schema.SessionUpdateMethodName, json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed session/update was accepted")
	}
}

// Notices and compactions go through the stable acp.SessionProjection, so
// the compaction patch rules hold here too.
func TestProjectionDelegatesStablePatchSemantics(t *testing.T) {
	projection := acpunstable.NewProjection()
	notifications := projection.Notifications()
	for _, update := range []string{
		`{"sessionUpdate":"compaction_summary_chunk","compactionId":"c","content":{"type":"text","text":"partial"}}`,
		`{"sessionUpdate":"compaction_update","compactionId":"c","status":"failed","summary":null,"error":"boom"}`,
		`{"sessionUpdate":"notice","severity":"_custom","title":"heads up"}`,
	} {
		raw := json.RawMessage(`{"sessionId":"s","update":` + update + `}`)
		if err := notifications(schema.SessionUpdateMethodName, raw); err != nil {
			t.Fatal(err)
		}
	}
	compactions := projection.Compactions("s")
	if len(compactions) != 1 || compactions[0].Summary != nil || compactions[0].Error == nil || *compactions[0].Error != "boom" {
		t.Fatalf("compactions = %+v", compactions)
	}
	if notices := projection.DrainNotices("s"); len(notices) != 1 || notices[0].Severity != "_custom" {
		t.Fatalf("notices = %+v", notices)
	}
}

// Every content shape the unstable schema decodes, with annotations and
// _meta, converts to the stable summary unchanged, whether it arrives as a
// compaction_summary_chunk or in a compaction_update summary. The adapter
// returns a conversion failure rather than dropping the update.
func TestProjectionConvertsEveryContentShapeToStable(t *testing.T) {
	blocks := []string{
		`{"type":"text","text":"t","annotations":{"audience":["user"],"priority":0.5},"_meta":{"k":"v"}}`,
		`{"type":"image","data":"aGk=","mimeType":"image/png","uri":"file:///i.png","_meta":{"k":"v"}}`,
		`{"type":"audio","data":"aGk=","mimeType":"audio/wav","annotations":{"lastModified":"2026-01-01T00:00:00Z"}}`,
		`{"type":"resource_link","uri":"file:///a","name":"a","title":"A","description":"d","mimeType":"text/plain","size":3}`,
		`{"type":"resource","resource":{"uri":"file:///b","text":"b","mimeType":"text/plain"},"_meta":{"k":"v"}}`,
		`{"type":"resource","resource":{"uri":"file:///c","blob":"aGk="}}`,
	}
	same := func(t *testing.T, got stableschema.ContentBlock, want string) {
		t.Helper()
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var gotValue, wantValue any
		if err := json.Unmarshal(encoded, &gotValue); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Fatalf("stable block = %s, want %s", encoded, want)
		}
	}
	projection := acpunstable.NewProjection()
	notifications := projection.Notifications()
	send := func(update string) {
		t.Helper()
		raw := json.RawMessage(`{"sessionId":"s","update":` + update + `}`)
		if err := notifications(schema.SessionUpdateMethodName, raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, block := range blocks {
		send(`{"sessionUpdate":"compaction_summary_chunk","compactionId":"chunks","content":` + block + `}`)
	}
	send(`{"sessionUpdate":"compaction_update","compactionId":"whole","status":"completed","summary":[` + strings.Join(blocks, ",") + `]}`)
	for _, id := range []stableschema.CompactionId{"chunks", "whole"} {
		var summary []stableschema.ContentBlock
		for _, compaction := range projection.Compactions("s") {
			if compaction.ID == id {
				summary = compaction.Summary
			}
		}
		if len(summary) != len(blocks) {
			t.Fatalf("%s summary = %+v", id, summary)
		}
		for i, block := range blocks {
			same(t, summary[i], block)
		}
	}
}
