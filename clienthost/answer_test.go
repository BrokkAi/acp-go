package clienthost

import (
	"context"
	"encoding/json"
	"testing"
)

// notify feeds one session/update notification to the host.
func notify(t *testing.T, h *Host, update string) {
	t.Helper()
	if err := h.Notification("session/update", json.RawMessage(`{"sessionId":"s","update":`+update+`}`)); err != nil {
		t.Fatal(err)
	}
}

func answerHost(t *testing.T) *Host {
	t.Helper()
	h, err := Open(context.Background(), Config{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	h.SetSession("s")
	return h
}

func TestAnswerSeparatesDistinctAgentMessages(t *testing.T) {
	h := answerHost(t)
	// An agent is free to end a message without a trailing newline. The next
	// message must not be welded onto it: a client that reads the final answer
	// off the last line would otherwise see both at once.
	notify(t, h, `{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"The work is complete."}}`)
	notify(t, h, `{"sessionUpdate":"agent_message_chunk","messageId":"m2","content":{"type":"text","text":"RESULT {\"status\":\"solved\"}"}}`)
	answer, truncated := h.Answer()
	if truncated {
		t.Fatal("a short answer was truncated")
	}
	if answer != "The work is complete.\nRESULT {\"status\":\"solved\"}" {
		t.Fatalf("answer = %q", answer)
	}
}

func TestAnswerJoinsChunksOfOneMessageUnchanged(t *testing.T) {
	h := answerHost(t)
	for _, text := range []string{"Checking ", "the ", "build."} {
		notify(t, h, `{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"`+text+`"}}`)
	}
	if answer, _ := h.Answer(); answer != "Checking the build." {
		t.Fatalf("chunks of one message were altered: %q", answer)
	}
}

func TestAnswerKeepsOneBufferWhenTheAgentOmitsMessageIds(t *testing.T) {
	h := answerHost(t)
	// messageId is optional. An agent that never sends one gets exactly the
	// behavior it had before message boundaries were tracked.
	notify(t, h, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"first "}}`)
	notify(t, h, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"second"}}`)
	if answer, _ := h.Answer(); answer != "first second" {
		t.Fatalf("answer = %q", answer)
	}
}

func TestAnswerDoesNotLeadWithASeparator(t *testing.T) {
	h := answerHost(t)
	notify(t, h, `{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"only"}}`)
	if answer, _ := h.Answer(); answer != "only" {
		t.Fatalf("the first message was prefixed: %q", answer)
	}
}

func TestAnswerIgnoresNonMessageUpdates(t *testing.T) {
	h := answerHost(t)
	notify(t, h, `{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"answer"}}`)
	notify(t, h, `{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"thinking"}}`)
	notify(t, h, `{"sessionUpdate":"tool_call","toolCallId":"t","title":"probe"}`)
	notify(t, h, `{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":" continues"}}`)
	if answer, _ := h.Answer(); answer != "answer continues" {
		t.Fatalf("an unrelated update broke the message: %q", answer)
	}
}
