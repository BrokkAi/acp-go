// Package clienthost provides the SDK's reference implementation of the host
// capabilities that an ACP agent may invoke in a client.
package clienthost

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

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/osrun"
	"github.com/BrokkAi/acp-go/schema"
)

const (
	defaultAnswerCapacity = 2 << 20
	defaultFileReadLimit  = 4 << 20
	defaultTerminalOutput = 1 << 20
	maxTerminals          = 32
)

// Config describes a workspace-confined client host.
type Config struct {
	// Directory is the immutable workspace root. Every filesystem and terminal
	// path is resolved beneath it.
	Directory string
	// AutoApprove selects allow_once/allow_always permission requests. It is
	// intentionally opt-in and defaults to disabled.
	AutoApprove bool
	Logger      *slog.Logger
	// Transcript receives raw notifications, permission decisions, process
	// output, and run metadata as newline-delimited JSON.
	Transcript io.Writer
	// AnswerCapacity bounds retained agent message text. The default is 2 MiB.
	AnswerCapacity int
}

type Host struct {
	autoApprove bool
	ctx         context.Context
	cancel      context.CancelFunc
	root        *os.Root
	directory   string

	mu         sync.Mutex
	session    schema.SessionId
	logger     *slog.Logger
	transcript *json.Encoder
	logError   error
	answer     osrun.Tail
	terminals  map[string]*commandTerminal
	next       uint64
	closing    bool
	toolOutput map[string]*toolTranscript
}

func Open(parent context.Context, config Config) (*Host, error) {
	if !filepath.IsAbs(config.Directory) {
		return nil, fmt.Errorf("client host directory must be absolute: %q", config.Directory)
	}
	root, err := os.OpenRoot(config.Directory)
	if err != nil {
		return nil, err
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Transcript == nil {
		config.Transcript = io.Discard
	}
	if config.AnswerCapacity <= 0 {
		config.AnswerCapacity = defaultAnswerCapacity
	}
	ctx, cancel := context.WithCancel(parent)
	return &Host{
		autoApprove: config.AutoApprove,
		ctx:         ctx,
		cancel:      cancel,
		root:        root,
		directory:   config.Directory,
		logger:      config.Logger,
		transcript:  json.NewEncoder(config.Transcript),
		answer:      osrun.Tail{Capacity: config.AnswerCapacity},
		terminals:   make(map[string]*commandTerminal),
		toolOutput:  make(map[string]*toolTranscript),
	}, nil
}

func (h *Host) SetSession(sessionID schema.SessionId) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.session = sessionID
}

func (h *Host) SetAutoApprove(enabled bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.autoApprove = enabled
}

func (h *Host) Record(value any) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.logError == nil {
		h.logError = h.transcript.Encode(value)
	}
	return h.logError
}

func (h *Host) LogErr() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logError
}

func (h *Host) Answer() (string, bool) {
	return h.answer.Text()
}

func (h *Host) Logger() *slog.Logger { return h.logger }

func (h *Host) Notification(method string, raw json.RawMessage) error {
	if method != schema.SessionUpdateMethodName {
		return nil
	}
	var update acp.Update
	if err := json.Unmarshal(raw, &update); err != nil {
		return err
	}
	h.mu.Lock()
	session := h.session
	h.mu.Unlock()
	if session != "" && update.SessionID != session {
		return nil
	}
	if err := h.Record(json.RawMessage(raw)); err != nil {
		return err
	}
	if chunk := update.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
		_, _ = h.answer.Write([]byte(chunk.Content.Text.Text))
	}
	return h.showUpdate(update)
}

func (h *Host) validSession(session schema.SessionId) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.session != "" && h.session == session
}

func (h *Host) Request(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case schema.SessionRequestPermissionMethodName:
		var request schema.RequestPermissionRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
		}
		if !h.validSession(request.SessionID) {
			return nil, &acp.RPCError{Code: -32602, Message: "unknown sessionId"}
		}
		return h.permission(ctx, raw, request)
	case schema.FsReadTextFileMethodName:
		var request schema.ReadTextFileRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
		}
		if !h.validSession(request.SessionID) {
			return nil, &acp.RPCError{Code: -32602, Message: "unknown sessionId"}
		}
		if err := h.ready(); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return h.readFile(request)
	case schema.FsWriteTextFileMethodName:
		var request schema.WriteTextFileRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
		}
		if !h.validSession(request.SessionID) {
			return nil, &acp.RPCError{Code: -32602, Message: "unknown sessionId"}
		}
		if err := h.ready(); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return h.writeFile(request)
	case schema.TerminalCreateMethodName,
		schema.TerminalOutputMethodName,
		schema.TerminalWaitForExitMethodName,
		schema.TerminalKillMethodName,
		schema.TerminalReleaseMethodName:
		return h.terminal(ctx, method, raw)
	default:
		return nil, &acp.RPCError{Code: -32601, Message: "unsupported method: " + method}
	}
}

func (h *Host) ready() error {
	return h.ctx.Err()
}

func (h *Host) permission(ctx context.Context, raw json.RawMessage, request schema.RequestPermissionRequest) (any, error) {
	cancelled := schema.RequestPermissionResponse{Outcome: schema.RequestPermissionOutcome{
		Cancelled: &schema.RequestPermissionOutcomeCancelled{},
	}}
	h.mu.Lock()
	autoApprove := h.autoApprove
	h.mu.Unlock()
	if !autoApprove || ctx.Err() != nil || h.ctx.Err() != nil {
		return cancelled, nil
	}
	for _, kind := range []schema.PermissionOptionKind{
		schema.PermissionOptionKindAllowOnce,
		schema.PermissionOptionKindAllowAlways,
	} {
		for _, option := range request.Options {
			if option.Kind != kind {
				continue
			}
			if err := h.Record(map[string]any{"permission_request": raw, "selected": option.OptionID}); err != nil {
				return nil, err
			}
			return schema.RequestPermissionResponse{Outcome: schema.RequestPermissionOutcome{
				Selected: &schema.SelectedPermissionOutcome{OptionID: option.OptionID},
			}}, nil
		}
	}
	return cancelled, nil
}

func (h *Host) relative(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("ACP file and terminal paths must be absolute")
	}
	rel, err := filepath.Rel(h.directory, path)
	if err != nil {
		return "", err
	}
	if rel != "." && !filepath.IsLocal(rel) {
		return "", errors.New("path lies outside the workspace")
	}
	return rel, nil
}

func (h *Host) writeFile(request schema.WriteTextFileRequest) (any, error) {
	path, err := h.relative(request.Path)
	if err != nil {
		return nil, err
	}
	if err := h.root.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	if err := h.root.WriteFile(path, []byte(request.Content), 0644); err != nil {
		return nil, err
	}
	return schema.WriteTextFileResponse{}, nil
}

func (h *Host) readFile(request schema.ReadTextFileRequest) (any, error) {
	path, err := h.relative(request.Path)
	if err != nil {
		return nil, err
	}
	f, err := h.root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, defaultFileReadLimit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > defaultFileReadLimit {
		return nil, errors.New("file exceeds 4 MiB; inspect it through a terminal")
	}
	lines := strings.Split(string(b), "\n")
	first, last := 0, len(lines)
	if request.Line != nil {
		if *request.Line < 1 {
			return nil, errors.New("line must be at least 1")
		}
		first = min(last, int(*request.Line)-1)
	}
	if request.Limit != nil {
		if *request.Limit < 0 {
			return nil, errors.New("limit cannot be negative")
		}
		last = first + min(last-first, int(*request.Limit))
	}
	return schema.ReadTextFileResponse{Content: strings.Join(lines[first:last], "\n")}, nil
}

func (h *Host) Close() {
	h.cancel()
	h.mu.Lock()
	h.closing = true
	terminals := make([]*commandTerminal, 0, len(h.terminals))
	for _, terminal := range h.terminals {
		terminals = append(terminals, terminal)
	}
	h.mu.Unlock()
	for _, terminal := range terminals {
		terminal.kill()
		<-terminal.finished
	}
	_ = h.root.Close()
}
