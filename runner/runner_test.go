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
	"syscall"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/clienthost"
	"github.com/BrokkAi/acp-go/internal/osrun"
	"github.com/BrokkAi/acp-go/schema"
)

const (
	testAgentEnv     = "ACP_GO_RUNNER_TEST_AGENT"
	testSelectorsEnv = "ACP_GO_RUNNER_TEST_SELECTORS"
	testFailInitEnv  = "ACP_GO_RUNNER_TEST_FAIL_INITIALIZE"
	testShutdownEnv  = "ACP_GO_RUNNER_TEST_SHUTDOWN"
	testSleeperEnv   = "ACP_GO_RUNNER_TEST_SLEEPER"
)

func TestMain(m *testing.M) {
	if os.Getenv(testSleeperEnv) == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv(testAgentEnv) == "1" {
		beforeServe(os.Getenv(testShutdownEnv))
		implementation := &echoTestAgent{
			selectors: os.Getenv(testSelectorsEnv) == "1",
			failInit:  os.Getenv(testFailInitEnv) == "1",
		}
		if implementation.selectors {
			fmt.Fprintln(os.Stderr, "runner test diagnostics")
		}
		if err := agent.New(implementation).Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
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

// echoTestAgent advertises a model selector (large, small) and a reasoning
// effort selector (low, high) when selectors is set. It accepts model changes
// and rejects every effort change.
type echoTestAgent struct {
	sessions  int
	selectors bool
	failInit  bool
	model     string
}

func (a *echoTestAgent) Initialize(context.Context, agent.Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	if a.failInit {
		return schema.InitializeResponse{}, errors.New("initialize refused by test agent")
	}
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
	response := schema.NewSessionResponse{SessionID: "runner-session"}
	if a.selectors {
		a.model = "large"
		response.ConfigOptions = a.configOptions()
	}
	return response, nil
}

func (a *echoTestAgent) configOptions() []schema.SessionConfigOption {
	selector := func(id, name string, category schema.SessionConfigOptionCategory, current string, values ...string) schema.SessionConfigOption {
		options := make([]any, 0, len(values))
		for _, value := range values {
			options = append(options, map[string]any{"value": value, "name": value})
		}
		return schema.SessionConfigOption{ID: schema.SessionConfigId(id), Name: name, Category: &category,
			Select: &schema.SessionConfigSelect{CurrentValue: schema.SessionConfigValueId(current), Options: options}}
	}
	return []schema.SessionConfigOption{
		selector("model", "Model", schema.SessionConfigOptionCategoryModel, a.model, "large", "small"),
		selector("effort", "Effort", schema.SessionConfigOptionCategoryThoughtLevel, "low", "low", "high"),
	}
}

func (a *echoTestAgent) SetConfigOption(_ context.Context, _ agent.Client, request schema.SetSessionConfigOptionRequest) (schema.SetSessionConfigOptionResponse, error) {
	if request.ConfigID != "model" || request.ValueID == nil {
		return schema.SetSessionConfigOptionResponse{}, errors.New("effort rejected by test agent")
	}
	a.model = string(request.ValueID.Value)
	return schema.SetSessionConfigOptionResponse{ConfigOptions: a.configOptions()}, nil
}

func (a *echoTestAgent) Prompt(_ context.Context, _ agent.Client, request schema.PromptRequest, updates agent.SessionUpdater) (schema.PromptResponse, error) {
	crashIfAsked()
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

func runSelection(t *testing.T, selectors bool, model, effort string) error {
	t.Helper()
	environment := map[string]string{testAgentEnv: "1"}
	if selectors {
		environment[testSelectorsEnv] = "1"
	}
	result, err := Runner{
		Config: Config{
			Directory:      t.TempDir(),
			StateDirectory: t.TempDir(),
			Agent: AgentConfig{
				Command:     []string{os.Args[0]},
				Environment: environment,
				Model:       model,
				Effort:      effort,
			},
		},
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}.Execute(context.Background(), "runner hello")
	if err == nil {
		t.Fatalf("selection succeeded with result %q", result)
	}
	return err
}

func setupPhase(t *testing.T, err error, phase Phase) {
	t.Helper()
	var setup *SetupError
	if !errors.As(err, &setup) {
		t.Fatalf("error %v is not *SetupError", err)
	}
	if setup.Phase != phase {
		t.Fatalf("phase = %q, want %q (error %v)", setup.Phase, phase, err)
	}
	if !strings.HasPrefix(err.Error(), "agent setup failed before prompt: ") {
		t.Fatalf("setup error text changed: %v", err)
	}
}

func TestSetupErrorReportsUnknownModel(t *testing.T) {
	err := runSelection(t, true, "huge", "")
	setupPhase(t, err, PhaseSelectModel)
	var unknown *acp.UnknownSelectionError
	if !errors.As(err, &unknown) {
		t.Fatalf("error %v is not *acp.UnknownSelectionError", err)
	}
	if unknown.Category != schema.SessionConfigOptionCategoryModel || unknown.Value != "huge" ||
		strings.Join(unknown.Available, ",") != "large,small" {
		t.Fatalf("unknown selection = %+v", unknown)
	}
	// The diagnostics defer wraps the setup error with %w; the text keeps both.
	want := `agent setup failed before prompt: unknown Model "huge"; available values: large, small` +
		"\nAgent diagnostics: runner test diagnostics\n"
	if err.Error() != want {
		t.Fatalf("error text = %q, want %q", err.Error(), want)
	}
}

func TestSetupErrorReportsUnknownEffortAfterModel(t *testing.T) {
	err := runSelection(t, true, "small", "max")
	setupPhase(t, err, PhaseSelectEffort)
	var unknown *acp.UnknownSelectionError
	if !errors.As(err, &unknown) || unknown.Category != schema.SessionConfigOptionCategoryThoughtLevel || unknown.Value != "max" {
		t.Fatalf("error %v, unknown selection %+v", err, unknown)
	}
}

func TestSetupErrorReportsUnsupportedEffort(t *testing.T) {
	err := runSelection(t, false, "", "high")
	setupPhase(t, err, PhaseSelectEffort)
	var unsupported *acp.UnsupportedSelectionError
	if !errors.As(err, &unsupported) || unsupported.Category != schema.SessionConfigOptionCategoryThoughtLevel || unsupported.Value != "high" {
		t.Fatalf("error %v, unsupported selection %+v", err, unsupported)
	}
	var unknown *acp.UnknownSelectionError
	if errors.As(err, &unknown) {
		t.Fatalf("unsupported selector reported as unknown value: %v", err)
	}
}

func TestSetupErrorReportsAgentRejectedEffort(t *testing.T) {
	err := runSelection(t, true, "large", "high")
	setupPhase(t, err, PhaseSelectEffort)
	var rpc *acp.RPCError
	if !errors.As(err, &rpc) {
		t.Fatalf("error %v does not wrap *acp.RPCError", err)
	}
	var unknown *acp.UnknownSelectionError
	var unsupported *acp.UnsupportedSelectionError
	if errors.As(err, &unknown) || errors.As(err, &unsupported) {
		t.Fatalf("agent rejection classified as a local selection error: %v", err)
	}
}

func TestSetupErrorReportsLaunchPhase(t *testing.T) {
	_, err := Runner{Config: Config{StateDirectory: t.TempDir()}}.Execute(context.Background(), "hello")
	setupPhase(t, err, PhaseLaunch)
	if err.Error() != "agent setup failed before prompt: agent command is required" {
		t.Fatalf("error text = %q", err.Error())
	}
	_, err = Runner{Config: Config{
		Directory:      t.TempDir(),
		StateDirectory: t.TempDir(),
		Agent:          AgentConfig{Command: []string{filepath.Join(t.TempDir(), "missing-agent")}},
	}}.Execute(context.Background(), "hello")
	setupPhase(t, err, PhaseLaunch)
}

func testRunner(t *testing.T, environment map[string]string, authMethod string) Runner {
	t.Helper()
	environment[testAgentEnv] = "1"
	return Runner{
		Config: Config{
			Directory:      t.TempDir(),
			StateDirectory: t.TempDir(),
			Agent: AgentConfig{
				Command:     []string{os.Args[0]},
				Environment: environment,
				AuthMethod:  authMethod,
			},
		},
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
}

func TestSetupErrorReportsInitializeAndAuthenticatePhases(t *testing.T) {
	_, err := testRunner(t, map[string]string{testFailInitEnv: "1"}, "").Execute(context.Background(), "hello")
	setupPhase(t, err, PhaseInitialize)
	var rpc *acp.RPCError
	if !errors.As(err, &rpc) {
		t.Fatalf("initialize error %v does not wrap *acp.RPCError", err)
	}

	_, err = testRunner(t, map[string]string{}, "missing").Execute(context.Background(), "hello")
	setupPhase(t, err, PhaseAuthenticate)
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("transcript write failed") }

func TestSetupErrorReportsTranscriptWriteAsPromptPhase(t *testing.T) {
	original := newHost
	t.Cleanup(func() { newHost = original })
	newHost = func(ctx context.Context, directory string, _ io.Writer, logger *slog.Logger) (*clienthost.Host, error) {
		return original(ctx, directory, failingWriter{}, logger)
	}
	_, err := testRunner(t, map[string]string{}, "").Execute(context.Background(), "hello")
	setupPhase(t, err, PhasePrompt)
	if !strings.Contains(err.Error(), "transcript write failed") {
		t.Fatalf("error = %v", err)
	}
}

func runShutdown(t *testing.T, ctx context.Context, mode string, extra map[string]string) (string, error, string) {
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
	}.Execute(ctx, "runner hello")
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

func TestRunnerLetsAgentExitOnStdinEOF(t *testing.T) {
	result, err, transcript := runShutdown(t, context.Background(), "report", nil)
	if err != nil || result != "runner hello" {
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
	if err != nil || result != "runner hello" {
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
	if result != "runner hello" {
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
