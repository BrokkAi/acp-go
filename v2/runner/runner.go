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
// state update. A nil StopReason means the agent reported idle without one.
type Result struct {
	Text       string
	StopReason *schema.StopReason
}

type Runner struct {
	Config Config
	Log    *slog.Logger
}

// SetupError marks failures before the agent accepts a prompt.
type SetupError struct{ Err error }

func (e *SetupError) Error() string { return "agent setup failed before prompt: " + e.Err.Error() }
func (e *SetupError) Unwrap() error { return e.Err }

func (r Runner) Execute(ctx context.Context, prompt string) (result Result, runErr error) {
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	if r.Config.ClientInfo.Name == "" || r.Config.ClientInfo.Version == "" {
		r.Config.ClientInfo = acpv2.ClientInfo{Name: "acp-go-v2-runner", Version: "0.3.0"}
	}
	if len(r.Config.Agent.Command) == 0 || r.Config.Agent.Command[0] == "" {
		return result, &SetupError{errors.New("agent command is required")}
	}
	directory := r.Config.Directory
	if directory == "" {
		absolute, err := filepath.Abs(".")
		if err != nil {
			return result, &SetupError{err}
		}
		directory = absolute
	} else if !filepath.IsAbs(directory) {
		return result, &SetupError{fmt.Errorf("workspace directory must be absolute: %q", directory)}
	}

	stateDirectory := r.Config.StateDirectory
	if stateDirectory == "" {
		created, err := os.MkdirTemp("", "acp-go-v2-state-")
		if err != nil {
			return result, &SetupError{err}
		}
		stateDirectory = created
		defer os.RemoveAll(stateDirectory)
	}
	if err := os.MkdirAll(filepath.Join(stateDirectory, "sessions"), 0o700); err != nil {
		return result, &SetupError{err}
	}
	transcript, err := os.CreateTemp(filepath.Join(stateDirectory, "sessions"), "session-*.jsonl")
	if err != nil {
		return result, &SetupError{err}
	}
	defer transcript.Close()

	state := newRunState()
	host := newRunHost(r.Config.Permissions, r.Config.Elicitation, transcript)
	log.Info("Starting ACP v2 agent",
		"command", strings.Join(r.Config.Agent.Command, " "),
		"transcript", transcript.Name(),
	)
	command := osrun.StartCommand(context.Background(), directory, r.Config.Agent.Command, r.Config.Agent.Environment)
	diagnostics := &osrun.Tail{Capacity: 64 << 10}
	command.Stderr = diagnostics
	stdin, err := command.StdinPipe()
	if err != nil {
		return result, &SetupError{err}
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return result, &SetupError{err}
	}
	if err := command.Start(); err != nil {
		return result, &SetupError{fmt.Errorf("launch ACP v2 agent: %w", err)}
	}
	defer func() {
		_ = osrun.Kill(command)
		_ = command.Wait()
		text, _ := diagnostics.Text()
		_ = host.Record(map[string]string{"event": "agent_stderr", "text": text})
		if runErr != nil && text != "" {
			runErr = fmt.Errorf("%w\nAgent diagnostics: %s", runErr, text)
		}
	}()
	defer stdin.Close()
	defer stdout.Close()

	connection := acpv2.Connect(stdout, stdin, host.Request, acpv2.SessionUpdates(func(update acpv2.Update) error {
		if err := host.Record(map[string]any{"event": "session_update", "update": update}); err != nil {
			return err
		}
		state.notification(update)
		return nil
	}))
	started := time.Now()
	phase := "initialize"
	promptAccepted := false
	defer func() {
		record := map[string]any{"event": "session_end", "phase": phase, "elapsed": time.Since(started).String()}
		if runErr != nil {
			record["error"] = runErr.Error()
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
			runErr = &SetupError{runErr}
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
		phase = "authenticate"
		if err := connection.AuthLogin(ctx, initialization, r.Config.Agent.AuthMethod); err != nil {
			return result, err
		}
	}
	phase = "session/new"
	session, err := connection.NewSessionWithOptions(ctx, initialization, directory, acpv2.NewSessionOptions{})
	if err != nil {
		return result, fmt.Errorf("create ACP v2 session (check agent login): %w", err)
	}
	idle := state.wait(session.SessionID)
	phase = "session/prompt"
	if err := connection.Prompt(ctx, initialization, session, prompt); err != nil {
		return result, err
	}
	promptAccepted = true
	phase = "wait-for-idle"
	select {
	case <-idle:
		text, stopReason := state.result(session.SessionID)
		result = Result{Text: text, StopReason: stopReason}
	case <-ctx.Done():
		return result, ctx.Err()
	case <-connection.Done():
		return result, errors.New("agent disconnected before foreground work became idle")
	}
	phase = "session/close"
	if err := connection.CloseSession(ctx, initialization, session.SessionID); err != nil {
		return result, err
	}
	return result, nil
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
