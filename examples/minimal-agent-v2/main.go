// Command minimal-agent-v2 demonstrates the baseline draft ACP v2 agent
// surface over stdio.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"

	schema "github.com/BrokkAi/acp-go/schema/v2"
	agent "github.com/BrokkAi/acp-go/v2/agent"
)

type echoAgent struct {
	// mu serializes prompts, so a retry waits for the prompt it repeats.
	mu   sync.Mutex
	next int
	// inserted records, per session, the user message inserted for each
	// idempotency key. An agent that advertises capabilities.session must
	// honor the key (schema 1.19).
	inserted map[schema.SessionId]map[schema.IdempotencyKey]schema.MessageId
}

func (*echoAgent) Initialize(context.Context, agent.Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion: schema.ProtocolVersion(2),
		Info:            schema.Implementation{Name: "minimal-agent-v2", Version: "0.0"},
		Capabilities:    &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}},
	}, nil
}

func (*echoAgent) NewSession(context.Context, agent.Client, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	return schema.NewSessionResponse{SessionID: "minimal-v2-session"}, nil
}

// Prompt inserts the user message into the conversation, echoes it under the
// same ID, and returns that ID. ACP v2 requires the response to identify the
// inserted message. A retry with an idempotency key the session has already
// seen gets the message ID it was first given and starts no work.
func (a *echoAgent) Prompt(_ context.Context, _ agent.Client, request schema.PromptRequest, updates agent.SessionUpdater) (schema.PromptResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if key := request.IdempotencyKey; key != nil {
		if messageID, ok := a.inserted[request.SessionID][*key]; ok {
			return schema.PromptResponse{MessageID: messageID}, nil
		}
	}
	a.next++
	userMessageID := schema.MessageId(fmt.Sprintf("user-message-%d", a.next))
	text := ""
	if len(request.Prompt) == 1 && request.Prompt[0].Text != nil {
		text = request.Prompt[0].Text.Text
	}
	content := []schema.ContentBlock{{Text: &schema.TextContent{Text: text}}}
	for i, update := range []schema.SessionUpdate{
		{UserMessage: &schema.UserMessage{
			MessageID: userMessageID,
			Content:   schema.Nullable[[]schema.ContentBlock]{Set: true, Value: request.Prompt},
		}},
		{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}},
		{AgentMessage: &schema.AgentMessage{
			MessageID: schema.MessageId(fmt.Sprintf("agent-message-%d", a.next)),
			Content:   schema.Nullable[[]schema.ContentBlock]{Set: true, Value: content},
		}},
		{StateUpdate: &schema.StateUpdate{Idle: &schema.IdleStateUpdate{}}},
	} {
		if err := updates.Update(update); err != nil {
			return schema.PromptResponse{}, err
		}
		if i == 0 && request.IdempotencyKey != nil {
			// The user message is inserted: record its key in the same step.
			if a.inserted == nil {
				a.inserted = map[schema.SessionId]map[schema.IdempotencyKey]schema.MessageId{}
			}
			if a.inserted[request.SessionID] == nil {
				a.inserted[request.SessionID] = map[schema.IdempotencyKey]schema.MessageId{}
			}
			a.inserted[request.SessionID][*request.IdempotencyKey] = userMessageID
		}
	}
	return schema.PromptResponse{MessageID: userMessageID}, nil
}

func (*echoAgent) ListSessions(context.Context, agent.Client, schema.ListSessionsRequest) (schema.ListSessionsResponse, error) {
	return schema.ListSessionsResponse{}, nil
}

// ResumeSession keeps no history to replay, but a resume that asks for a
// replay still ends it with replay_complete, sent before the response (schema
// 1.18). A resume without replayFrom must not send one.
func (*echoAgent) ResumeSession(ctx context.Context, client agent.Client, request schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	if request.ReplayFrom != nil {
		if err := client.Notify(ctx, schema.SessionUpdateMethodName, schema.UpdateSessionNotification{
			SessionID: request.SessionID,
			Update:    schema.SessionUpdate{ReplayComplete: &schema.ReplayComplete{}},
		}); err != nil {
			return schema.ResumeSessionResponse{}, err
		}
	}
	return schema.ResumeSessionResponse{}, nil
}

func (*echoAgent) CloseSession(context.Context, agent.Client, schema.CloseSessionRequest) (schema.CloseSessionResponse, error) {
	return schema.CloseSessionResponse{}, nil
}

func (*echoAgent) CancelSession(schema.CancelSessionNotification) error { return nil }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := agent.New(&echoAgent{}).Serve(ctx, os.Stdin, os.Stdout); err != nil {
		panic(err)
	}
}
