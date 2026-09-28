package acptest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/BrokkAi/acp-go/agent"
	schema "github.com/BrokkAi/acp-go/schema"
)

// Command is one deterministic prompt instruction for TestAgent, mirroring the
// reference Rust harness's typed prompt commands. Prompt text is the command's
// JSON encoding, so a test pins exactly what the agent emits.
//
// A prompt that is not a JSON command is treated as a plain message, which
// keeps simple prompts readable.
type Command struct {
	// Message is the agent message chunk text. Empty emits no message.
	Message string `json:"message,omitempty"`
	// Thought is an agent thought chunk text. Empty emits no thought.
	Thought string `json:"thought,omitempty"`
	// StopReason overrides the default end_turn stop reason.
	StopReason schema.StopReason `json:"stopReason,omitempty"`
	// Fail makes the agent return an error for this prompt.
	Fail string `json:"fail,omitempty"`
}

// Prompt returns the prompt text that drives TestAgent with this command.
func (c Command) Prompt() string {
	encoded, err := json.Marshal(c)
	if err != nil {
		// Command only holds JSON-friendly fields.
		panic(fmt.Sprintf("acptest: encode command: %v", err))
	}
	return string(encoded)
}

// ParseCommand decodes prompt text into a Command. Plain text becomes a command
// with that text as the message.
func ParseCommand(prompt string) Command {
	var command Command
	if err := json.Unmarshal([]byte(prompt), &command); err != nil {
		return Command{Message: prompt}
	}
	return command
}

// TestAgent is a deterministic v1 agent driven by Command prompts. Its hooks
// let a test pin the advertised capabilities and the session identifier.
type TestAgent struct {
	// Capabilities is returned by Initialize. Nil advertises no optional
	// capabilities.
	Capabilities *schema.AgentCapabilities
	// SessionID is returned by NewSession. Empty uses "test-session".
	SessionID string
	// Prompts receives every prompt request, when non-nil and buffered.
	Prompts chan schema.PromptRequest
}

func (a *TestAgent) Initialize(context.Context, agent.Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion:   1,
		AgentInfo:         &schema.Implementation{Name: "acptest-agent", Version: "test"},
		AgentCapabilities: a.Capabilities,
	}, nil
}

func (a *TestAgent) NewSession(context.Context, agent.Client, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	sessionID := a.SessionID
	if sessionID == "" {
		sessionID = "test-session"
	}
	return schema.NewSessionResponse{SessionID: schema.SessionId(sessionID)}, nil
}

func (a *TestAgent) Prompt(_ context.Context, _ agent.Client, request schema.PromptRequest, updates agent.SessionUpdater) (schema.PromptResponse, error) {
	if a.Prompts != nil {
		a.Prompts <- request
	}
	prompt := ""
	if len(request.Prompt) == 1 && request.Prompt[0].Text != nil {
		prompt = request.Prompt[0].Text.Text
	}
	command := ParseCommand(prompt)
	if command.Fail != "" {
		return schema.PromptResponse{}, errors.New(command.Fail)
	}
	if command.Thought != "" {
		if err := updates.Update(schema.SessionUpdate{AgentThoughtChunk: &schema.ContentChunk{
			Content: schema.ContentBlock{Text: &schema.TextContent{Text: command.Thought}},
		}}); err != nil {
			return schema.PromptResponse{}, err
		}
	}
	if command.Message != "" {
		if err := updates.Update(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
			Content: schema.ContentBlock{Text: &schema.TextContent{Text: command.Message}},
		}}); err != nil {
			return schema.PromptResponse{}, err
		}
	}
	stopReason := command.StopReason
	if stopReason == "" {
		stopReason = schema.StopReasonEndTurn
	}
	return schema.PromptResponse{StopReason: stopReason}, nil
}
