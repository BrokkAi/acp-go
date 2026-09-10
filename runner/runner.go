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

// Setup failures happen before any release prompt reaches the agent. Retrying
// unchanged startup settings cannot repair them and must not spend release tries.
type SetupError struct{ Err error }

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
		return "", &SetupError{errors.New("agent command is required")}
	}
	promptStarted := false
	defer func() {
		if runErr != nil && !promptStarted {
			runErr = &SetupError{runErr}
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
	cmd := osrun.StartCommand(context.Background(), a.Config.Directory, a.Config.Agent.Command, a.Config.Agent.Environment)
	diagnostics := &osrun.Tail{Capacity: 64 << 10}
	cmd.Stderr = io.MultiWriter(diagnostics, host.ProcessWriter("Agent stderr", ""))
	in, err := cmd.StdinPipe()
	if err != nil {
		return result, err
	}
	defer in.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return result, err
	}
	defer out.Close()
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("launch ACP agent: %w", err)
	}
	defer func() {
		_ = osrun.Kill(cmd)
		_ = cmd.Wait()
		text, _ := diagnostics.Text()
		_ = host.Record(map[string]string{"stderr": text})
		if runErr != nil && text != "" {
			runErr = fmt.Errorf("%w\nAgent diagnostics: %s", runErr, text)
		}
	}()
	connection := acp.Connect(out, in, host.Request, host.Notification)
	phase := "initialize"
	started := time.Now()
	defer func() {
		record := map[string]any{"event": "session_end", "phase": phase, "elapsed": time.Since(started).String()}
		if runErr != nil {
			record["error"] = runErr.Error()
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
		phase = "authenticate"
		if err := connection.Authenticate(ctx, init, a.Config.Agent.AuthMethod); err != nil {
			return result, err
		}
	}
	phase = "session/new"
	session, err := connection.NewSession(ctx, a.Config.Directory)
	if err != nil {
		return result, fmt.Errorf("create ACP session (check agent login): %w", err)
	}
	host.SetSession(session.SessionID)
	if a.Config.Agent.Mode != "" {
		phase = "select mode"
		if err := connection.SetMode(ctx, &session, a.Config.Agent.Mode); err != nil {
			return result, err
		}
	}
	if a.Config.Agent.Model != "" {
		phase = "select model"
		if err := connection.SetModel(ctx, &session, a.Config.Agent.Model); err != nil {
			return result, err
		}
		a.Log.Info("Using model", "model", a.Config.Agent.Model)
	}
	if a.Config.Agent.Effort != "" {
		phase = "select effort"
		if err := connection.SetEffort(ctx, &session, a.Config.Agent.Effort); err != nil {
			return result, err
		}
		a.Log.Info("Using reasoning effort", "effort", a.Config.Agent.Effort)
	}
	a.Log.Info("agent session", "id", session.SessionID, "transcript", transcript.Name())
	if err := host.Record(map[string]string{"prompt": prompt}); err != nil {
		return result, err
	}
	phase = "session/prompt"
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

func newHost(ctx context.Context, directory string, transcript io.Writer, logger *slog.Logger) (*clienthost.Host, error) {
	return clienthost.Open(ctx, clienthost.Config{
		Directory:  directory,
		Logger:     logger,
		Transcript: transcript,
	})
}
