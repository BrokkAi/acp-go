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
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/clienthost"
	"github.com/BrokkAi/acp-go/internal/osrun"
	"github.com/BrokkAi/acp-go/schema"
)

// AgentConfig configures an already-authenticated ACP executable.
type AgentConfig struct {
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment,omitempty"`
	AuthMethod  string            `json:"auth_method,omitempty"`
	Mode        string            `json:"mode,omitempty"`
	Model       string            `json:"model,omitempty"`
	Effort      string            `json:"effort,omitempty"`
}

// Config describes one process and its managed workspace. AutoApprove explicitly
// authorizes agent permission requests. Commands run with the caller's OS rights.
type Config struct {
	Directory      string
	StateDirectory string
	Agent          AgentConfig
	ClientInfo     acp.ClientInfo
	AutoApprove    bool
}

type Runner struct {
	Config Config
	Log    *slog.Logger
}

// Phase names a setup step reported in SetupError.Phase. Values match the
// phase recorded in the session transcript's session_end event.
type Phase string

const (
	// PhaseLaunch covers configuration checks, the state directory,
	// transcript, client host, and starting the agent process.
	PhaseLaunch       Phase = "launch"
	PhaseInitialize   Phase = "initialize"
	PhaseAuthenticate Phase = "authenticate"
	PhaseSessionNew   Phase = "session/new"
	PhaseSelectMode   Phase = "select mode"
	PhaseSelectModel  Phase = "select model"
	PhaseSelectEffort Phase = "select effort"
	// PhasePrompt covers recording the prompt in the transcript before it is
	// sent. Transcript write errors are sticky, so a failure here may come from
	// an earlier transcript write. Failures after the prompt is sent are not
	// setup errors.
	PhasePrompt Phase = "session/prompt"
)

// Setup failures happen before any release prompt reaches the agent. Retrying
// unchanged startup settings cannot repair them and must not spend release tries.
//
// Phase names the step that failed. Some selection failures can be classified
// further with errors.As: *acp.UnknownSelectionError means the value is not
// offered, *acp.UnsupportedSelectionError means the agent advertises no such
// selector, and *acp.RPCError means the agent returned a JSON-RPC error. Other
// failures stay untyped, including an agent that does not confirm the
// selection, malformed selector options, transport and context errors, and the
// legacy "unknown session mode" error, so a failure matching none of these is
// not necessarily an agent rejection.
type SetupError struct {
	Err   error
	Phase Phase
}

func (e *SetupError) Error() string { return "agent setup failed before prompt: " + e.Err.Error() }
func (e *SetupError) Unwrap() error { return e.Err }

func (a Runner) Execute(ctx context.Context, prompt string) (result string, runErr error) {
	if a.Log == nil {
		a.Log = slog.Default()
	}
	if a.Config.ClientInfo.Name == "" || a.Config.ClientInfo.Version == "" {
		a.Config.ClientInfo = acp.ClientInfo{Name: "acp-go-runner", Version: "0.1.0"}
	}
	if len(a.Config.Agent.Command) == 0 || a.Config.Agent.Command[0] == "" {
		return "", &SetupError{Err: errors.New("agent command is required"), Phase: PhaseLaunch}
	}
	promptStarted := false
	phase := PhaseLaunch
	defer func() {
		if runErr != nil && !promptStarted {
			runErr = &SetupError{Err: runErr, Phase: phase}
		}
	}()
	dir := filepath.Join(a.Config.StateDirectory, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return result, err
	}
	transcript, err := os.CreateTemp(dir, "session-*.jsonl")
	if err != nil {
		return result, err
	}
	defer transcript.Close()
	host, err := newHost(ctx, a.Config.Directory, transcript, a.Log)
	if err != nil {
		return result, err
	}
	defer host.Close()
	host.SetAutoApprove(a.Config.AutoApprove)
	a.Log.Info("Starting agent", "command", strings.Join(a.Config.Agent.Command, " "), "transcript", transcript.Name())
	diagnostics := &osrun.Tail{Capacity: 64 << 10}
	process, err := osrun.StartAgent(a.Config.Directory, a.Config.Agent.Command, a.Config.Agent.Environment,
		io.MultiWriter(diagnostics, host.ProcessWriter("Agent stderr", "")))
	if err != nil {
		return result, fmt.Errorf("launch ACP agent: %w", err)
	}
	// crashed is the exit error of an agent that failed on its own before the
	// run ended.
	var crashed error
	defer func() {
		exitErr := endAgent(ctx, process, runErr, crashed)
		text, _ := diagnostics.Text()
		_ = host.Record(map[string]string{"stderr": text})
		switch {
		case exitErr != nil:
			runErr = withAgentExit(runErr, &AgentExitError{Err: exitErr, Stderr: text})
		case runErr != nil && text != "":
			runErr = fmt.Errorf("%w\nAgent diagnostics: %s", runErr, text)
		}
	}()
	connection := acp.Connect(process.Stdout, process.Stdin, host.Request, host.Notification)
	release := process.Supervise(connection, shutdown)
	phase = PhaseInitialize
	started := time.Now()
	defer func() {
		crashed = release()
		if runErr == nil && crashed == nil && !isClosed(connection.Done()) {
			// As the reference SDK does on a successful run, write what is
			// still queued for the agent before its stdin closes; a failed
			// drain fails the run, as it fails connect_with there. A
			// connection that already stopped is skipped: one the agent
			// ended with a clean EOF wrote its queued frames before
			// stopping, and one the supervisor closed after the agent
			// exited has no reader left.
			if err := connection.Shutdown(ctx); err != nil {
				runErr = fmt.Errorf("shut down ACP connection: %w", err)
			}
		}
		record := map[string]any{"event": "session_end", "phase": phase, "elapsed": time.Since(started).String()}
		if runErr != nil {
			record["error"] = runErr.Error()
		}
		if crashed != nil {
			record["agent_exit"] = crashed.Error()
		}
		if cause := context.Cause(ctx); cause != nil {
			record["context_cause"] = cause.Error()
		}
		if err := connection.Err(); err != nil {
			record["transport_error"] = err.Error()
		}
		_ = host.Record(record)
		_ = connection.Close()
	}()
	capabilities := acp.WorkspaceCapabilities(true, true, true)
	capabilities.Session = acp.ConfigOptionsClientCapabilities(true)
	init, err := connection.InitializeWithInfo(ctx, capabilities, a.Config.ClientInfo)
	if err != nil {
		return result, err
	}
	if a.Config.Agent.AuthMethod != "" {
		phase = PhaseAuthenticate
		if err := connection.Authenticate(ctx, init, a.Config.Agent.AuthMethod); err != nil {
			return result, err
		}
	}
	phase = PhaseSessionNew
	session, err := connection.NewSession(ctx, a.Config.Directory)
	if err != nil {
		return result, fmt.Errorf("create ACP session (check agent login): %w", err)
	}
	host.SetSession(session.SessionID)
	if a.Config.Agent.Mode != "" {
		phase = PhaseSelectMode
		if err := connection.SetMode(ctx, &session, a.Config.Agent.Mode); err != nil {
			return result, err
		}
	}
	if a.Config.Agent.Model != "" {
		phase = PhaseSelectModel
		if err := connection.SetModel(ctx, &session, a.Config.Agent.Model); err != nil {
			return result, err
		}
		a.Log.Info("Using model", "model", a.Config.Agent.Model)
	}
	if a.Config.Agent.Effort != "" {
		phase = PhaseSelectEffort
		if err := connection.SetEffort(ctx, &session, a.Config.Agent.Effort); err != nil {
			return result, err
		}
		a.Log.Info("Using reasoning effort", "effort", a.Config.Agent.Effort)
	}
	a.Log.Info("agent session", "id", session.SessionID, "transcript", transcript.Name())
	phase = PhasePrompt
	if err := host.Record(map[string]string{"prompt": prompt}); err != nil {
		return result, err
	}
	promptStarted = true
	reason, err := connection.Prompt(ctx, session, prompt)
	if err != nil {
		return result, err
	}
	if reason != schema.StopReasonEndTurn {
		return result, fmt.Errorf("agent stopped with %s", reason)
	}
	if logErr := host.LogErr(); logErr != nil {
		return result, logErr
	}
	text, truncated := host.Answer()
	if truncated {
		return "", errors.New("agent answer exceeded 2 MiB")
	}
	return text, nil
}

// shutdown bounds the graceful agent shutdown; tests shorten it.
var shutdown = osrun.DefaultShutdown

// AgentExitError reports an agent process that exited with a failure status on
// its own, either during the run or within the grace period after the runner
// closed its stdin. Like the Rust SDK's AcpAgent, the runner reports this exit
// in place of the protocol result. When the run itself succeeded, Execute
// returns its result together with this error. A setup failure keeps its
// *SetupError, wrapping this error.
type AgentExitError struct {
	// Err is the *exec.ExitError describing the exit status.
	Err error
	// Stderr is the bounded tail of the agent's stderr, possibly empty.
	Stderr string
}

func (e *AgentExitError) Error() string {
	if e.Stderr == "" {
		return "agent process exited with " + e.Err.Error()
	}
	return "agent process exited with " + e.Err.Error() + ": " + e.Stderr
}
func (e *AgentExitError) Unwrap() error { return e.Err }

// endAgent ends the agent process after the connection is closed, as the Rust
// SDK's AcpAgent does. Only a successful run shuts the agent down gracefully:
// stdin EOF, then SIGTERM, then SIGKILL, bounded by shutdown. A failed or
// cancelled run, including one whose agent failed on its own, kills the
// process group at once. endAgent returns the exit error of an agent that failed on its own,
// whether during the run (crashed) or after its stdin closed.
func endAgent(ctx context.Context, process *osrun.Agent, runErr, crashed error) error {
	if runErr == nil && crashed == nil {
		return process.Stop(ctx, shutdown)
	}
	process.Kill(shutdown)
	return crashed
}

// withAgentExit reports an agent's failed exit in place of the run error,
// keeping a setup failure's phase.
func withAgentExit(runErr error, exit *AgentExitError) error {
	if setup, ok := runErr.(*SetupError); ok {
		setup.Err = exit
		return setup
	}
	return exit
}

// newHost is a variable so tests can substitute the transcript writer.
var newHost = func(ctx context.Context, directory string, transcript io.Writer, logger *slog.Logger) (*clienthost.Host, error) {
	return clienthost.Open(ctx, clienthost.Config{
		Directory:  directory,
		Logger:     logger,
		Transcript: transcript,
	})
}

// isClosed reports whether done is closed.
func isClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
