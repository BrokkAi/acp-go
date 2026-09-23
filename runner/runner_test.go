package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/clienthost"
	"github.com/BrokkAi/acp-go/schema"
)

const (
	testAgentEnv     = "ACP_GO_RUNNER_TEST_AGENT"
	testSelectorsEnv = "ACP_GO_RUNNER_TEST_SELECTORS"
	testFailInitEnv  = "ACP_GO_RUNNER_TEST_FAIL_INITIALIZE"
)

func TestMain(m *testing.M) {
	if os.Getenv(testAgentEnv) == "1" {
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
		return
	}
	os.Exit(m.Run())
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
