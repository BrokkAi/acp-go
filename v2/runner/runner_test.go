package runner

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"

	schema "github.com/BrokkAi/acp-go/schema/v2"
	agentv2 "github.com/BrokkAi/acp-go/v2/agent"
)

const testAgentEnv = "ACP_GO_V2_RUNNER_TEST_AGENT"

func TestMain(m *testing.M) {
	if os.Getenv(testAgentEnv) == "1" {
		implementation := &echoV2Agent{}
		if err := agentv2.New(implementation).Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
			panic(err)
		}
		return
	}
	os.Exit(m.Run())
}

type echoV2Agent struct {
	closed atomic.Bool
}

func (a *echoV2Agent) Initialize(context.Context, agentv2.Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion: schema.ProtocolVersion(2),
		Info:            schema.Implementation{Name: "v2-runner-test-agent", Version: "test"},
		Capabilities:    &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}},
	}, nil
}

func (a *echoV2Agent) NewSession(ctx context.Context, client agentv2.Client, request schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	err := client.Notify(ctx, schema.SessionUpdateMethodName, schema.UpdateSessionNotification{
		SessionID: "v2-runner-session",
		Update: schema.SessionUpdate{StateUpdate: &schema.StateUpdate{
			Idle: &schema.IdleStateUpdate{},
		}},
	})
	if err != nil {
		return schema.NewSessionResponse{}, err
	}
	return schema.NewSessionResponse{SessionID: "v2-runner-session"}, nil
}

func (a *echoV2Agent) Prompt(_ context.Context, client agentv2.Client, request schema.PromptRequest, updates agentv2.SessionUpdater) (schema.PromptResponse, error) {
	prompt := ""
	if len(request.Prompt) == 1 && request.Prompt[0].Text != nil {
		prompt = request.Prompt[0].Text.Text
	}
	permission, err := requestPermission(request.SessionID, client)
	if err != nil {
		return schema.PromptResponse{}, err
	}
	if permission.Outcome.Cancelled == nil {
		return schema.PromptResponse{}, errors.New("default runner permission policy did not cancel")
	}
	updatesToSend := []schema.SessionUpdate{
		{AgentMessageChunk: &schema.ContentChunk{MessageID: "stale", Content: schema.ContentBlock{
			Text: &schema.TextContent{Text: prompt},
		}}},
		{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}},
		{AgentMessageChunk: &schema.ContentChunk{MessageID: "message", Content: schema.ContentBlock{
			Text: &schema.TextContent{Text: "v2 "},
		}}},
		{AgentMessage: &schema.AgentMessage{MessageID: "message"}},
		{AgentMessageChunk: &schema.ContentChunk{MessageID: "second", Content: schema.ContentBlock{
			Text: &schema.TextContent{Text: "runner"},
		}}},
	}
	for _, update := range updatesToSend {
		if err := updates.Update(update); err != nil {
			return schema.PromptResponse{}, err
		}
	}
	reason := schema.StopReasonEndTurn
	if err := updates.Update(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{
		Idle: &schema.IdleStateUpdate{StopReason: &reason},
	}}); err != nil {
		return schema.PromptResponse{}, err
	}
	return schema.PromptResponse{}, nil
}

func requestPermission(sessionID schema.SessionId, client agentv2.Client) (schema.RequestPermissionResponse, error) {
	return client.RequestPermission(context.Background(), schema.RequestPermissionRequest{
		SessionID: sessionID,
		Title:     "Run the echo test",
		Options: []schema.PermissionOption{{
			OptionID: "allow", Name: "Allow", Kind: schema.PermissionOptionKindAllowOnce,
		}},
	})
}

func (a *echoV2Agent) ListSessions(context.Context, agentv2.Client, schema.ListSessionsRequest) (schema.ListSessionsResponse, error) {
	return schema.ListSessionsResponse{}, nil
}

func (a *echoV2Agent) ResumeSession(context.Context, agentv2.Client, schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	return schema.ResumeSessionResponse{}, nil
}

func (a *echoV2Agent) CloseSession(context.Context, agentv2.Client, schema.CloseSessionRequest) (schema.CloseSessionResponse, error) {
	a.closed.Store(true)
	return schema.CloseSessionResponse{}, nil
}

func (a *echoV2Agent) CancelSession(schema.CancelSessionNotification) error { return nil }

func TestRunnerWaitsForRunningThenIdle(t *testing.T) {
	result, err := Runner{
		Config: Config{
			Directory: t.TempDir(),
			Agent: AgentConfig{
				Command:     []string{os.Args[0]},
				Environment: map[string]string{testAgentEnv: "1"},
			},
		},
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}.Execute(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "v2 runner" {
		t.Fatalf("text = %q", result.Text)
	}
	if result.StopReason == nil || *result.StopReason != schema.StopReasonEndTurn {
		t.Fatalf("stop reason = %+v", result.StopReason)
	}
}
