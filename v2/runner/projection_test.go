package runner

import (
	"testing"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

func textBlock(text string) schema.ContentBlock {
	return schema.ContentBlock{Text: &schema.TextContent{Text: text}}
}

func TestProjectionMirrorsReferencePatchSemantics(t *testing.T) {
	state := newSessionState()
	state.apply(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
		MessageID: "stale", Content: textBlock("ignore before running"),
	}})
	state.apply(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{Idle: &schema.IdleStateUpdate{}}})
	state.apply(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}})
	state.apply(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
		MessageID: "message", Content: textBlock("hello "),
	}})
	state.apply(schema.SessionUpdate{AgentMessage: &schema.AgentMessage{MessageID: "message"}})
	state.apply(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
		MessageID: "second", Content: textBlock("world"),
	}})

	if done := newRunState().wait("unused"); done == nil {
		t.Fatal("unexpected nil idle channel")
	}
	reason := schema.StopReasonEndTurn
	state.apply(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{
		Idle: &schema.IdleStateUpdate{StopReason: &reason},
	}})
	if got := state.projection.text(); got != "hello world" {
		t.Fatalf("text = %q", got)
	}
	if state.stopReason == nil || *state.stopReason != schema.StopReasonEndTurn {
		t.Fatalf("stop reason = %+v", state.stopReason)
	}
	select {
	case <-state.done:
	default:
		t.Fatal("idle did not complete the state")
	}
}

func TestProjectionExplicitNullClearsButOmissionPreserves(t *testing.T) {
	projection := newProjection()
	projection.apply(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
		MessageID: "message", Content: textBlock("hello"),
	}})
	projection.apply(schema.SessionUpdate{AgentMessage: &schema.AgentMessage{MessageID: "message"}})
	if projection.text() != "hello" {
		t.Fatalf("omitted snapshot changed text to %q", projection.text())
	}
	projection.apply(schema.SessionUpdate{AgentMessage: &schema.AgentMessage{
		MessageID: "message",
		Content: schema.Nullable[[]schema.ContentBlock]{
			Set:  true,
			Null: true,
		},
	}})
	if projection.text() != "" {
		t.Fatalf("explicit null did not clear text: %q", projection.text())
	}
	projection.apply(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
		MessageID: "message", Content: textBlock("again"),
	}})
	if projection.text() != "again" {
		t.Fatalf("text after clear = %q", projection.text())
	}
}
