package unstable_test

import (
	"encoding/json"
	"testing"

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
		Summary: []schema.ContentBlock{{Text: &schema.TextContent{Text: "final"}}},
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
	if compactions[0].Status != schema.CompactionStatusCompleted {
		t.Fatalf("status = %q", compactions[0].Status)
	}
	// The summary field is a complete replacement, so the chunk is gone.
	if len(compactions[0].Summary) != 1 || compactions[0].Summary[0].Text.Text != "final" {
		t.Fatalf("summary = %+v", compactions[0].Summary)
	}
	if compactions[1].Status != schema.CompactionStatusFailed {
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
			Summary:      []schema.ContentBlock{{Text: &schema.TextContent{Text: "done"}}},
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
	if len(compactions) != 1 || compactions[0].Status != schema.CompactionStatusCompleted {
		t.Fatalf("compactions = %+v", compactions)
	}
	if err := notifications("session/cancel", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("unrelated notification = %v", err)
	}
	if err := notifications(schema.SessionUpdateMethodName, json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed session/update was accepted")
	}
}
