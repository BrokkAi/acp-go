package runner

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/schema"
)

const testAgentEnv = "ACP_GO_RUNNER_TEST_AGENT"

func TestMain(m *testing.M) {
	if os.Getenv(testAgentEnv) == "1" {
		implementation := &echoTestAgent{}
		if err := agent.New(implementation).Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
			panic(err)
		}
		return
	}
	os.Exit(m.Run())
}

type echoTestAgent struct {
	sessions int
}

func (a *echoTestAgent) Initialize(context.Context, agent.Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion: 1,
		AgentInfo:       &schema.Implementation{Name: "runner-test-agent", Version: "test"},
	}, nil
}

func (a *echoTestAgent) NewSession(context.Context, agent.Client, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	a.sessions++
	if a.sessions != 1 {
		return schema.NewSessionResponse{}, errors.New("test agent serves one session")
	}
	return schema.NewSessionResponse{SessionID: "runner-session"}, nil
}

func (a *echoTestAgent) Prompt(_ context.Context, _ agent.Client, request schema.PromptRequest, updates agent.SessionUpdater) (schema.PromptResponse, error) {
	for _, block := range request.Prompt {
		if block.Text == nil {
			continue
		}
		if err := updates.Update(schema.SessionUpdate{
			AgentMessageChunk: &schema.ContentChunk{Content: schema.ContentBlock{Text: block.Text}},
		}); err != nil {
			return schema.PromptResponse{}, err
		}
	}
	return schema.PromptResponse{StopReason: schema.StopReasonEndTurn}, nil
}

func TestRunnerUsesReferenceClientHostToEndToEnd(t *testing.T) {
	workspace := t.TempDir()
	state := t.TempDir()
	result, err := Runner{
		Config: Config{
			Directory:      workspace,
			StateDirectory: state,
			Agent: AgentConfig{
				Command:     []string{os.Args[0]},
				Environment: map[string]string{testAgentEnv: "1"},
			},
		},
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}.Execute(context.Background(), "runner hello")
	if err != nil {
		t.Fatal(err)
	}
	if result != "runner hello" {
		t.Fatalf("result = %q", result)
	}
}
