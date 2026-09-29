package cookbook_test

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	agentv2 "github.com/BrokkAi/acp-go/v2/agent"
)

// historyAgent is the deterministic in-memory draft-v2 agent behind the v2
// recipes. It echoes each prompt as an agent message, keeps every session's
// updates, and replays them when session/resume asks for replay. Its counters
// let a recipe show which lifecycle requests an application actually sent.
type historyAgent struct {
	mu       sync.Mutex
	sessions int
	messages int
	history  map[schema.SessionId][]schema.SessionUpdate
	resumes  map[schema.SessionId]int
	closes   map[schema.SessionId]int
}

func newHistoryAgent(history map[schema.SessionId][]schema.SessionUpdate) *historyAgent {
	if history == nil {
		history = make(map[schema.SessionId][]schema.SessionUpdate)
	}
	return &historyAgent{
		history: history,
		resumes: make(map[schema.SessionId]int),
		closes:  make(map[schema.SessionId]int),
	}
}

// agentText is one complete agent message, the shape a replayed history uses.
func agentText(messageID, text string) schema.SessionUpdate {
	return schema.SessionUpdate{AgentMessage: &schema.AgentMessage{
		MessageID: schema.MessageId(messageID),
		Content: schema.Nullable[[]schema.ContentBlock]{
			Set: true, Value: []schema.ContentBlock{{Text: &schema.TextContent{Text: text}}},
		},
	}}
}

func (a *historyAgent) Initialize(context.Context, agentv2.Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion: acpv2.Version,
		Info:            schema.Implementation{Name: "cookbook-agent", Version: "0.1.0"},
		Capabilities:    &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}},
	}, nil
}

func (a *historyAgent) NewSession(context.Context, agentv2.Client, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions++
	id := schema.SessionId(fmt.Sprintf("session-%d", a.sessions))
	a.history[id] = nil
	return schema.NewSessionResponse{SessionID: id}, nil
}

// Prompt inserts the user message, runs, answers, and goes idle. The prompt
// response only reports the inserted message ID, as draft v2 requires.
func (a *historyAgent) Prompt(_ context.Context, _ agentv2.Client, request schema.PromptRequest, updates agentv2.SessionUpdater) (schema.PromptResponse, error) {
	var text strings.Builder
	for _, block := range request.Prompt {
		if block.Text != nil {
			text.WriteString(block.Text.Text)
		}
	}
	a.mu.Lock()
	a.messages++
	userMessage := schema.MessageId(fmt.Sprintf("user-%d", a.messages))
	answer := fmt.Sprintf("agent-%d", a.messages)
	a.mu.Unlock()

	endTurn := schema.StopReasonEndTurn
	for _, update := range []schema.SessionUpdate{
		{UserMessage: &schema.UserMessage{
			MessageID: userMessage,
			Content:   schema.Nullable[[]schema.ContentBlock]{Set: true, Value: request.Prompt},
		}},
		{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}},
		agentText(answer, "Echo: "+text.String()),
		{StateUpdate: &schema.StateUpdate{Idle: &schema.IdleStateUpdate{StopReason: &endTurn}}},
	} {
		a.record(request.SessionID, update)
		if err := updates.Update(update); err != nil {
			return schema.PromptResponse{}, err
		}
	}
	return schema.PromptResponse{MessageID: userMessage}, nil
}

func (a *historyAgent) record(sessionID schema.SessionId, update schema.SessionUpdate) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.history[sessionID] = append(a.history[sessionID], update)
}

func (a *historyAgent) ListSessions(context.Context, agentv2.Client, schema.ListSessionsRequest) (schema.ListSessionsResponse, error) {
	return schema.ListSessionsResponse{}, nil
}

// ResumeSession replays the retained history before it responds, so replay
// precedes the resume response on the wire.
func (a *historyAgent) ResumeSession(ctx context.Context, client agentv2.Client, request schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	a.mu.Lock()
	history, known := a.history[request.SessionID]
	history = append([]schema.SessionUpdate(nil), history...)
	a.resumes[request.SessionID]++
	a.mu.Unlock()
	if !known {
		return schema.ResumeSessionResponse{}, &acp.RPCError{Code: -32002, Message: "unknown session"}
	}
	if request.ReplayFrom != nil && request.ReplayFrom.Start != nil {
		for _, update := range history {
			if err := client.Notify(ctx, schema.SessionUpdateMethodName, acpv2.Update{
				SessionID: request.SessionID,
				Update:    update,
			}); err != nil {
				return schema.ResumeSessionResponse{}, err
			}
		}
	}
	return schema.ResumeSessionResponse{}, nil
}

func (a *historyAgent) CloseSession(_ context.Context, _ agentv2.Client, request schema.CloseSessionRequest) (schema.CloseSessionResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closes[request.SessionID]++
	return schema.CloseSessionResponse{}, nil
}

func (a *historyAgent) CancelSession(schema.CancelSessionNotification) error { return nil }

// counts reports how many resume and close requests a session received.
func (a *historyAgent) counts(sessionID schema.SessionId) (resumes, closes int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.resumes[sessionID], a.closes[sessionID]
}
