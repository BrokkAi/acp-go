// Command minimal-agent-v2 demonstrates the baseline draft ACP v2 agent
// surface over stdio.
package main

import (
	"context"
	"os"
	"os/signal"

	schema "github.com/BrokkAi/acp-go/schema/v2"
	agent "github.com/BrokkAi/acp-go/v2/agent"
)

type echoAgent struct{}

func (echoAgent) Initialize(context.Context, agent.Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion: schema.ProtocolVersion(2),
		Info:            schema.Implementation{Name: "minimal-agent-v2", Version: "0.0"},
		Capabilities:    &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}},
	}, nil
}

func (echoAgent) NewSession(context.Context, agent.Client, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	return schema.NewSessionResponse{SessionID: "minimal-v2-session"}, nil
}

// Prompt inserts the user message into the conversation, echoes it under the
// same ID, and returns that ID. ACP v2 requires the response to identify the
// inserted message.
func (echoAgent) Prompt(_ context.Context, _ agent.Client, request schema.PromptRequest, updates agent.SessionUpdater) (schema.PromptResponse, error) {
	text := ""
	if len(request.Prompt) == 1 && request.Prompt[0].Text != nil {
		text = request.Prompt[0].Text.Text
	}
	userMessageID := schema.MessageId("user-message")
	content := []schema.ContentBlock{{Text: &schema.TextContent{Text: text}}}
	for _, update := range []schema.SessionUpdate{
		{UserMessage: &schema.UserMessage{
			MessageID: userMessageID,
			Content:   schema.Nullable[[]schema.ContentBlock]{Set: true, Value: request.Prompt},
		}},
		{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}},
		{AgentMessage: &schema.AgentMessage{
			MessageID: "agent-message",
			Content:   schema.Nullable[[]schema.ContentBlock]{Set: true, Value: content},
		}},
		{StateUpdate: &schema.StateUpdate{Idle: &schema.IdleStateUpdate{}}},
	} {
		if err := updates.Update(update); err != nil {
			return schema.PromptResponse{}, err
		}
	}
	return schema.PromptResponse{MessageID: userMessageID}, nil
}

func (echoAgent) ListSessions(context.Context, agent.Client, schema.ListSessionsRequest) (schema.ListSessionsResponse, error) {
	return schema.ListSessionsResponse{}, nil
}

func (echoAgent) ResumeSession(context.Context, agent.Client, schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	return schema.ResumeSessionResponse{}, nil
}

func (echoAgent) CloseSession(context.Context, agent.Client, schema.CloseSessionRequest) (schema.CloseSessionResponse, error) {
	return schema.CloseSessionResponse{}, nil
}

func (echoAgent) CancelSession(schema.CancelSessionNotification) error { return nil }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := agent.New(echoAgent{}).Serve(ctx, os.Stdin, os.Stdout); err != nil {
		panic(err)
	}
}
