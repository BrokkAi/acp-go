// Package runner runs one draft ACP v2 agent process, submits a prompt, and
// waits for that session's foreground work to become idle.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/osrun"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// AgentConfig configures an ACP v2 executable.
type AgentConfig struct {
	Command     []string          `json:"command"`
	Environment map[string]string `json:"environment,omitempty"`
	AuthMethod  string            `json:"auth_method,omitempty"`
}

// Config describes one process and its workspace. Permissions defaults to the
// reference one-shot behavior: explicitly cancel every permission request.
type Config struct {
	Directory      string
	StateDirectory string
	Agent          AgentConfig
	ClientInfo     acpv2.ClientInfo
	Permissions    acpv2.Permissions
	Elicitation    acpv2.Elicitation
}

// Result contains the projected agent text and the stop reason from the idle
// state update. A nil StopReason means the agent reported idle without one, or
// with a malformed one; a custom or unknown reason is kept as received. Error
// is the failure the agent reported with the error stop reason, if any.
// UserMessageID is the ID the agent assigned to the inserted user message when
// it accepted the prompt.
type Result struct {
	Text          string
	StopReason    *schema.StopReason
	Error         *schema.Error
	UserMessageID schema.MessageId
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
	// transcript, and starting the agent process.
	PhaseLaunch       Phase = "launch"
	PhaseInitialize   Phase = "initialize"
	PhaseAuthenticate Phase = "authenticate"
	PhaseSessionNew   Phase = "session/new"
	// PhasePrompt covers session/prompt until the agent accepts the prompt.
	PhasePrompt Phase = "session/prompt"
)

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
	if isWorkError(runErr) {
		// The agent reported a failed turn over a working connection and the
		// session closed; the run itself completed. A WorkError joined with
		// a later failure, such as session/close, is a failed run.
		runErr = nil
	}
	if runErr == nil && crashed == nil {
		return process.Stop(ctx, shutdown)
	}
	process.Kill(shutdown)
	return crashed
}

// withAgentExit reports an agent's failed exit in place of the run error,
// keeping a setup failure's phase. A failure the agent reported for its turn
// is not replaced: the *acpv2.WorkError, and any error joined with it, is
// joined with the exit so errors.As finds each.
func withAgentExit(runErr error, exit *AgentExitError) error {
	if setup, ok := runErr.(*SetupError); ok {
		setup.Err = exit
		return setup
	}
	var workFailed *acpv2.WorkError
	if errors.As(runErr, &workFailed) {
		return errors.Join(runErr, exit)
	}
	return exit
}

// SetupError marks failures before the agent accepts a prompt. Phase names the
// step that failed.
type SetupError struct {
	Err   error
	Phase Phase
}

func (e *SetupError) Error() string { return "agent setup failed before prompt: " + e.Err.Error() }
func (e *SetupError) Unwrap() error { return e.Err }

// Execute runs the agent, submits prompt, and waits for the session's
// foreground work to become idle. When the agent ends that work with the error
// stop reason, Execute closes the session as usual and returns the result,
// including any projected text, together with an *acpv2.WorkError carrying
// the reported JSON-RPC error. Every other stop reason, including cancelled,
// custom, and unknown ones, returns a nil error. As for a successful run, the
// agent's stderr is recorded but not appended to that error. If closing the
// session or the agent's exit then fails too, the *acpv2.WorkError is joined
// with that failure (an *AgentExitError for the exit), so errors.As finds
// each.
func (r Runner) Execute(ctx context.Context, prompt string) (result Result, runErr error) {
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	if r.Config.ClientInfo.Name == "" || r.Config.ClientInfo.Version == "" {
		r.Config.ClientInfo = acpv2.ClientInfo{Name: "acp-go-v2-runner", Version: "0.3.0"}
	}
	if len(r.Config.Agent.Command) == 0 || r.Config.Agent.Command[0] == "" {
		return result, &SetupError{Err: errors.New("agent command is required"), Phase: PhaseLaunch}
	}
	directory := r.Config.Directory
	if directory == "" {
		absolute, err := filepath.Abs(".")
		if err != nil {
			return result, &SetupError{Err: err, Phase: PhaseLaunch}
		}
		directory = absolute
	} else if !filepath.IsAbs(directory) {
		return result, &SetupError{Err: fmt.Errorf("workspace directory must be absolute: %q", directory), Phase: PhaseLaunch}
	}

	stateDirectory := r.Config.StateDirectory
	if stateDirectory == "" {
		created, err := os.MkdirTemp("", "acp-go-v2-state-")
		if err != nil {
			return result, &SetupError{Err: err, Phase: PhaseLaunch}
		}
		stateDirectory = created
		defer os.RemoveAll(stateDirectory)
	}
	if err := os.MkdirAll(filepath.Join(stateDirectory, "sessions"), 0o700); err != nil {
		return result, &SetupError{Err: err, Phase: PhaseLaunch}
	}
	transcript, err := os.CreateTemp(filepath.Join(stateDirectory, "sessions"), "session-*.jsonl")
	if err != nil {
		return result, &SetupError{Err: err, Phase: PhaseLaunch}
	}
	defer transcript.Close()

	tracker := acpv2.NewSessionTracker()
	host := newRunHost(r.Config.Permissions, r.Config.Elicitation, transcript)
	log.Info("Starting ACP v2 agent",
		"command", strings.Join(r.Config.Agent.Command, " "),
		"transcript", transcript.Name(),
	)
	diagnostics := &osrun.Tail{Capacity: 64 << 10}
	process, err := osrun.StartAgent(directory, r.Config.Agent.Command, r.Config.Agent.Environment, diagnostics)
	if err != nil {
		return result, &SetupError{Err: fmt.Errorf("launch ACP v2 agent: %w", err), Phase: PhaseLaunch}
	}
	// crashed is the exit error of an agent that failed on its own before the
	// run ended.
	var crashed error
	defer func() {
		exitErr := endAgent(ctx, process, runErr, crashed)
		text, _ := diagnostics.Text()
		_ = host.Record(map[string]string{"event": "agent_stderr", "text": text})
		switch {
		case exitErr != nil:
			runErr = withAgentExit(runErr, &AgentExitError{Err: exitErr, Stderr: text})
		case runErr != nil && text != "" && !isWorkError(runErr):
			// A bare *acpv2.WorkError means the run completed with the
			// error stop reason, so, as for a successful run, the agent's
			// stderr is recorded but not appended.
			runErr = fmt.Errorf("%w\nAgent diagnostics: %s", runErr, text)
		}
	}()

	connection := acpv2.Connect(process.Stdout, process.Stdin, host.Request, acpv2.SessionUpdates(func(update acpv2.Update) error {
		if err := host.Record(map[string]any{"event": "session_update", "update": update}); err != nil {
			return err
		}
		tracker.Observe(update)
		return nil
	}))
	release := process.Supervise(connection, shutdown)
	started := time.Now()
	phase := PhaseInitialize
	promptAccepted := false
	defer func() {
		crashed = release()
		if (runErr == nil || isWorkError(runErr)) && crashed == nil && !isClosed(connection.Done()) {
			// As the reference SDK does on a successful run, write what is
			// still queued for the agent before its stdin closes; a failed
			// drain fails the run, as it fails connect_with there. A
			// connection that already stopped is skipped: one the agent
			// ended with a clean EOF wrote its queued frames before
			// stopping, and one the supervisor closed after the agent
			// exited has no reader left.
			if err := connection.Shutdown(ctx); err != nil {
				err = fmt.Errorf("shut down ACP v2 connection: %w", err)
				if runErr != nil {
					err = errors.Join(runErr, err)
				}
				runErr = err
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
		if transport := connection.Err(); transport != nil {
			record["transport_error"] = transport.Error()
		}
		_ = host.Record(record)
		_ = connection.Close()
	}()
	defer func() {
		if runErr != nil && !promptAccepted {
			runErr = &SetupError{Err: runErr, Phase: phase}
		}
	}()

	capabilities := acpv2.Capabilities{}
	if r.Config.Elicitation != nil {
		capabilities.Elicitation = acpv2.ElicitationClientCapabilities(true, true)
	}
	initialization, err := connection.InitializeWithInfo(ctx, capabilities, r.Config.ClientInfo)
	if err != nil {
		return result, err
	}
	if r.Config.Agent.AuthMethod != "" {
		phase = PhaseAuthenticate
		if err := connection.AuthLogin(ctx, initialization, r.Config.Agent.AuthMethod); err != nil {
			return result, err
		}
	}
	phase = PhaseSessionNew
	session, err := connection.NewSessionWithOptions(ctx, initialization, directory, acpv2.NewSessionOptions{})
	if err != nil {
		return result, fmt.Errorf("create ACP v2 session (check agent login): %w", err)
	}
	work := tracker.BeginWork(session.SessionID)
	phase = PhasePrompt
	userMessageID, err := connection.Prompt(ctx, initialization, session, prompt)
	if err != nil {
		return result, err
	}
	promptAccepted = true
	_ = host.Record(map[string]any{"event": "prompt_accepted", "user_message_id": userMessageID})
	phase = "wait-for-idle"
	var workErr error
	select {
	case <-work.Done():
		workResult := work.Result()
		result = Result{
			Text:          workResult.Text,
			StopReason:    workResult.StopReason,
			Error:         workResult.Error,
			UserMessageID: userMessageID,
		}
		workErr = workResult.Err()
	case <-ctx.Done():
		return result, ctx.Err()
	case <-connection.Done():
		return result, errors.New("agent disconnected before foreground work became idle")
	}
	phase = "session/close"
	if err := connection.CloseSession(ctx, initialization, session.SessionID); err != nil {
		if workErr != nil {
			return result, errors.Join(workErr, err)
		}
		return result, err
	}
	return result, workErr
}

type runHost struct {
	permissions acpv2.Permissions
	elicitation acpv2.Elicitation
	request     acp.Handler

	mu         sync.Mutex
	transcript *json.Encoder
	logError   error
}

func newRunHost(permissions acpv2.Permissions, elicitation acpv2.Elicitation, transcript io.Writer) *runHost {
	if permissions == nil {
		permissions = cancelPermissions{}
	}
	return &runHost{
		permissions: permissions,
		elicitation: elicitation,
		request:     acpv2.HandleClientHost(permissions, elicitation),
		transcript:  json.NewEncoder(transcript),
	}
}

func (h *runHost) Request(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	return h.request(ctx, method, raw)
}

func (h *runHost) Record(value any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.logError == nil {
		h.logError = h.transcript.Encode(value)
	}
	return h.logError
}

type cancelPermissions struct{}

func (cancelPermissions) RequestPermission(context.Context, schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
	return acpv2.CancelPermission(), nil
}

// isWorkError reports whether err is a bare *acpv2.WorkError: the turn ended
// with the error stop reason and nothing else failed.
func isWorkError(err error) bool {
	_, ok := err.(*acpv2.WorkError)
	return ok
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
