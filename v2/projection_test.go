package v2

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

func textBlock(text string) schema.ContentBlock {
	return schema.ContentBlock{Text: &schema.TextContent{Text: text}}
}

func TestUpdateProjectionPatchSemantics(t *testing.T) {
	projection := NewUpdateProjection()
	projection.Apply(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
		MessageID: "message", Content: textBlock("hello"),
	}})
	projection.Apply(schema.SessionUpdate{AgentMessage: &schema.AgentMessage{MessageID: "message"}})
	if projection.Text() != "hello" {
		t.Fatalf("omitted snapshot changed text to %q", projection.Text())
	}
	projection.Apply(schema.SessionUpdate{AgentMessage: &schema.AgentMessage{
		MessageID: "message",
		Content: schema.Nullable[[]schema.ContentBlock]{
			Set:  true,
			Null: true,
		},
	}})
	if projection.Text() != "" {
		t.Fatalf("explicit null did not clear text: %q", projection.Text())
	}
	projection.Apply(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
		MessageID: "message", Content: textBlock("again"),
	}})
	if projection.Text() != "again" {
		t.Fatalf("text after clear = %q", projection.Text())
	}
}

func TestActiveWorkIgnoresPreRunningAndCompletesAtIdle(t *testing.T) {
	work := BeginActiveWork()
	work.Apply(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
		MessageID: "stale", Content: textBlock("before running"),
	}})
	work.Apply(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{Idle: &schema.IdleStateUpdate{}}})
	work.Apply(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}})
	work.Apply(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
		MessageID: "message", Content: textBlock("hello "),
	}})
	work.Apply(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
		MessageID: "second", Content: textBlock("world"),
	}})
	reason := schema.StopReasonEndTurn
	work.Apply(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{
		Idle: &schema.IdleStateUpdate{StopReason: &reason},
	}})
	result, err := work.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello world" || result.StopReason == nil || *result.StopReason != schema.StopReasonEndTurn {
		t.Fatalf("result = %+v", result)
	}
}

func TestSessionTrackerNotifications(t *testing.T) {
	tracker := NewSessionTracker()
	notifications := tracker.TrackerNotifications(nil)
	update := Update{
		SessionID: "session",
		Update: schema.SessionUpdate{StateUpdate: &schema.StateUpdate{
			Running: &schema.RunningStateUpdate{},
		}},
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	if err := notifications(schema.SessionUpdateMethodName, encoded); err != nil {
		t.Fatal(err)
	}
	work := tracker.Work("session")
	select {
	case <-work.Done():
		t.Fatal("work unexpectedly completed")
	default:
	}
}

func TestActiveWorkWaitHonorsContext(t *testing.T) {
	work := BeginActiveWork()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := work.Wait(ctx); err == nil {
		t.Fatal("expected timeout")
	}
}
