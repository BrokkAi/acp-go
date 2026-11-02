package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/internal/osrun"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	agentv2 "github.com/BrokkAi/acp-go/v2/agent"
)

const (
	testAgentEnv       = "ACP_GO_V2_RUNNER_TEST_AGENT"
	testFailSessionEnv = "ACP_GO_V2_RUNNER_TEST_FAIL_SESSION"
	testFailInitEnv    = "ACP_GO_V2_RUNNER_TEST_FAIL_INITIALIZE"
	testFailPromptEnv  = "ACP_GO_V2_RUNNER_TEST_FAIL_PROMPT"
	testErrorStopEnv   = "ACP_GO_V2_RUNNER_TEST_ERROR_STOP"
	testShutdownEnv    = "ACP_GO_V2_RUNNER_TEST_SHUTDOWN"
	testSleeperEnv     = "ACP_GO_V2_RUNNER_TEST_SLEEPER"
)

func TestMain(m *testing.M) {
	if os.Getenv(testSleeperEnv) == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv(testAgentEnv) == "1" {
		beforeServe(os.Getenv(testShutdownEnv))
		implementation := &echoV2Agent{
			failSession: os.Getenv(testFailSessionEnv) == "1",
			failInit:    os.Getenv(testFailInitEnv) == "1",
			failPrompt:  os.Getenv(testFailPromptEnv) == "1",
			errorStop:   os.Getenv(testErrorStopEnv) == "1",
		}
		if implementation.failSession {
			fmt.Fprintln(os.Stderr, "v2 runner test diagnostics")
		}
		if err := agentv2.New(implementation).Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
			panic(err)
		}
		afterStdinEOF(os.Getenv(testShutdownEnv))
		return
	}
	// The race detector otherwise delays every clean agent exit by a second.
	os.Setenv("GORACE", "atexit_sleep_ms=0")
	os.Exit(m.Run())
}

// beforeServe prepares a shutdown mode before the agent serves: "hang" reports
// every SIGTERM on stderr and keeps running.
func beforeServe(mode string) {
	if mode != "hang" {
		return
	}
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	go func() {
		for range terms {
			fmt.Fprintln(os.Stderr, "agent ignoring SIGTERM")
		}
	}()
}

// afterStdinEOF runs once Serve has returned on stdin EOF. "report" leaves a
// note in the agent's stderr, which only reaches the transcript if the runner
// waited for the agent to exit; "fail" exits with status 3; "hang" ignores the
// shutdown request.
func afterStdinEOF(mode string) {
	switch mode {
	case "report":
		fmt.Fprintln(os.Stderr, "agent saw stdin EOF")
	case "fail":
		fmt.Fprintln(os.Stderr, "agent failing on stdin EOF")
		os.Exit(3)
	case "hang":
		fmt.Fprintln(os.Stderr, "agent ignoring stdin EOF")
		for {
			time.Sleep(time.Hour)
		}
	}
}

// crashIfAsked makes the "crash" agent fail mid-prompt, leaving behind a
// descendant that holds its stdout open.
func crashIfAsked() {
	if os.Getenv(testShutdownEnv) != "crash" {
		return
	}
	sleeper := exec.Command(os.Args[0])
	sleeper.Env = append(os.Environ(), testSleeperEnv+"=1")
	sleeper.Stdout = os.Stdout
	if err := sleeper.Start(); err != nil {
		panic(err)
	}
	fmt.Fprintln(os.Stderr, "agent crashing")
	os.Exit(5)
}

type echoV2Agent struct {
	closed      atomic.Bool
	failSession bool
	failInit    bool
	failPrompt  bool
	errorStop   bool
}

func (a *echoV2Agent) Initialize(context.Context, agentv2.Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	if a.failInit {
		return schema.InitializeResponse{}, errors.New("initialize refused by test agent")
	}
	return schema.InitializeResponse{
		ProtocolVersion: schema.ProtocolVersion(2),
		Info:            schema.Implementation{Name: "v2-runner-test-agent", Version: "test"},
		Capabilities:    &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}},
	}, nil
}

func (a *echoV2Agent) NewSession(ctx context.Context, client agentv2.Client, request schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	if a.failSession {
		return schema.NewSessionResponse{}, errors.New("session refused by test agent")
	}
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
	crashIfAsked()
	if a.failPrompt {
		return schema.PromptResponse{}, errors.New("prompt refused by test agent")
	}
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
	idle := acpv2.NewIdleStateUpdate(schema.StopReasonEndTurn)
	if a.errorStop {
		idle = acpv2.NewErrorIdleStateUpdate(&schema.Error{Code: -32000, Message: "Authentication required"})
	}
	if err := updates.Update(idle); err != nil {
		return schema.PromptResponse{}, err
	}
	return schema.PromptResponse{MessageID: "user-message"}, nil
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
	if result.UserMessageID != "user-message" {
		t.Fatalf("user message ID = %q", result.UserMessageID)
	}
}

func TestSetupErrorReportsPhase(t *testing.T) {
	_, err := Runner{Config: Config{Directory: "relative", Agent: AgentConfig{Command: []string{os.Args[0]}}}}.Execute(context.Background(), "hello")
	var setup *SetupError
	if !errors.As(err, &setup) || setup.Phase != PhaseLaunch {
		t.Fatalf("error %v, setup %+v", err, setup)
	}
	if err.Error() != `agent setup failed before prompt: workspace directory must be absolute: "relative"` {
		t.Fatalf("error text = %q", err.Error())
	}

	_, err = Runner{
		Config: Config{
			Directory: t.TempDir(),
			Agent: AgentConfig{
				Command:     []string{os.Args[0]},
				Environment: map[string]string{testAgentEnv: "1", testFailSessionEnv: "1"},
			},
		},
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}.Execute(context.Background(), "hello")
	setup = nil
	// The diagnostics defer wraps the setup error with %w.
	if !errors.As(err, &setup) || setup.Phase != PhaseSessionNew {
		t.Fatalf("error %v, setup %+v", err, setup)
	}
	if !strings.HasPrefix(err.Error(), "agent setup failed before prompt: create ACP v2 session (check agent login): ") ||
		!strings.HasSuffix(err.Error(), "\nAgent diagnostics: v2 runner test diagnostics\n") {
		t.Fatalf("error text = %q", err.Error())
	}
}

func TestSetupErrorReportsProtocolPhases(t *testing.T) {
	tests := []struct {
		environment map[string]string
		authMethod  string
		phase       Phase
	}{
		{map[string]string{testFailInitEnv: "1"}, "", PhaseInitialize},
		{map[string]string{}, "missing", PhaseAuthenticate},
		{map[string]string{testFailPromptEnv: "1"}, "", PhasePrompt},
	}
	for _, test := range tests {
		test.environment[testAgentEnv] = "1"
		_, err := Runner{
			Config: Config{
				Directory: t.TempDir(),
				Agent: AgentConfig{
					Command:     []string{os.Args[0]},
					Environment: test.environment,
					AuthMethod:  test.authMethod,
				},
			},
			Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		}.Execute(context.Background(), "hello")
		var setup *SetupError
		if !errors.As(err, &setup) || setup.Phase != test.phase {
			t.Fatalf("want phase %q: error %v, setup %+v", test.phase, err, setup)
		}
	}
}

func runShutdown(t *testing.T, ctx context.Context, mode string, extra map[string]string) (Result, error, string) {
	t.Helper()
	state := t.TempDir()
	environment := map[string]string{testAgentEnv: "1", testShutdownEnv: mode}
	for key, value := range extra {
		environment[key] = value
	}
	result, err := Runner{
		Config: Config{
			Directory:      t.TempDir(),
			StateDirectory: state,
			Agent: AgentConfig{
				Command:     []string{os.Args[0]},
				Environment: environment,
			},
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}.Execute(ctx, "hello")
	transcripts, globErr := filepath.Glob(filepath.Join(state, "sessions", "*.jsonl"))
	if globErr != nil || len(transcripts) != 1 {
		t.Fatalf("transcripts = %v, %v", transcripts, globErr)
	}
	transcript, readErr := os.ReadFile(transcripts[0])
	if readErr != nil {
		t.Fatal(readErr)
	}
	return result, err, string(transcript)
}

func shortenShutdown(t *testing.T, s osrun.Shutdown) {
	previous := shutdown
	shutdown = s
	t.Cleanup(func() { shutdown = previous })
}

// An error stop reason is a typed failure that keeps the projected text and
// the reported JSON-RPC error. The run itself completed: the session is
// closed and the agent is shut down gracefully.
func TestRunnerReportsErrorStopReason(t *testing.T) {
	result, err, transcript := runShutdown(t, context.Background(), "report", map[string]string{testErrorStopEnv: "1"})
	var failed *acpv2.WorkError
	if !errors.As(err, &failed) {
		t.Fatalf("Execute error = %v, want *acpv2.WorkError", err)
	}
	var setup *SetupError
	if errors.As(err, &setup) {
		t.Fatalf("work failure reported as a setup failure: %v", err)
	}
	if failed.Failure == nil || failed.Failure.Code != -32000 || failed.Failure.Message != "Authentication required" {
		t.Fatalf("failure = %+v", failed.Failure)
	}
	if result.Text != "v2 runner" || result.UserMessageID != "user-message" || result.Error != failed.Failure ||
		result.StopReason == nil || *result.StopReason != schema.StopReasonError {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(transcript, `"phase":"session/close"`) {
		t.Fatalf("session was not closed:\n%s", transcript)
	}
	if !strings.Contains(transcript, "agent saw stdin EOF") {
		t.Fatalf("agent was not shut down gracefully:\n%s", transcript)
	}
}

func TestRunnerLetsAgentExitOnStdinEOF(t *testing.T) {
	result, err, transcript := runShutdown(t, context.Background(), "report", nil)
	if err != nil || result.Text != "v2 runner" {
		t.Fatalf("Execute = %+v, %v", result, err)
	}
	if !strings.Contains(transcript, "agent saw stdin EOF") {
		t.Fatalf("agent was stopped before it handled stdin EOF:\n%s", transcript)
	}
}

func TestRunnerTerminatesAgentIgnoringStdinEOF(t *testing.T) {
	// The default shutdown would take three seconds for this agent.
	shortenShutdown(t, osrun.Shutdown{Exit: 50 * time.Millisecond, Terminate: 50 * time.Millisecond})
	start := time.Now()
	result, err, transcript := runShutdown(t, context.Background(), "hang", nil)
	if err != nil || result.Text != "v2 runner" {
		t.Fatalf("Execute = %+v, %v", result, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Execute took %v; the shortened shutdown was not used", elapsed)
	}
	// The agent ignores stdin EOF and SIGTERM, so Execute returning means
	// it was killed.
	if !strings.Contains(transcript, "agent ignoring stdin EOF") {
		t.Fatalf("agent stderr missing from transcript:\n%s", transcript)
	}
	if runtime.GOOS != "windows" && !strings.Contains(transcript, "agent ignoring SIGTERM") {
		t.Fatalf("agent was killed without SIGTERM first:\n%s", transcript)
	}
}

func TestRunnerReportsAgentFailingAfterStdinEOF(t *testing.T) {
	result, err, _ := runShutdown(t, context.Background(), "fail", nil)
	if result.Text != "v2 runner" {
		t.Fatalf("result = %+v, want the successful run's result", result)
	}
	var exit *AgentExitError
	if !errors.As(err, &exit) || exitCode(exit) != 3 || !strings.Contains(exit.Stderr, "agent failing on stdin EOF") {
		t.Fatalf("Execute error = %v, want AgentExitError with status 3 and stderr", err)
	}
}

func TestRunnerKillsAgentAfterFailedRun(t *testing.T) {
	// Graceful shutdown would take three seconds for this agent; a failed
	// run must kill it at once.
	start := time.Now()
	_, err, _ := runShutdown(t, context.Background(), "hang", map[string]string{testFailInitEnv: "1"})
	var setup *SetupError
	if !errors.As(err, &setup) || setup.Phase != PhaseInitialize {
		t.Fatalf("Execute error = %v, want an initialize SetupError", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Execute took %v; a failed run must not wait for the agent", elapsed)
	}
}

func TestRunnerReportsAgentCrashMidPrompt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	_, err, _ := runShutdown(t, ctx, "crash", nil)
	var exit *AgentExitError
	if !errors.As(err, &exit) || exitCode(exit) != 5 || !strings.Contains(exit.Stderr, "agent crashing") {
		t.Fatalf("Execute error = %v, want AgentExitError with status 5 and stderr", err)
	}
	// The descendant holding stdout must not keep the run alive.
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Execute took %v after the agent crashed", elapsed)
	}
}

func exitCode(err *AgentExitError) int {
	var exit *exec.ExitError
	if !errors.As(err.Err, &exit) {
		return -1
	}
	return exit.ExitCode()
}
