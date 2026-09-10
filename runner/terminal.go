package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/osrun"
	"github.com/BrokkAi/acp-go/schema"
)

type commandTerminal struct {
	command  *exec.Cmd
	tail     osrun.Tail
	finished chan struct{}
	status   schema.TerminalExitStatus
	once     sync.Once
}

func (t *commandTerminal) kill() { t.once.Do(func() { _ = osrun.Kill(t.command) }) }

func (h *workspaceHost) terminal(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case schema.TerminalCreateMethodName:
		var request schema.CreateTerminalRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
		}
		if !h.validSession(request.SessionID) {
			return nil, &acp.RPCError{Code: -32602, Message: "unknown sessionId"}
		}
		if err := h.hostContext(); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return h.createTerminal(request)
	case schema.TerminalOutputMethodName:
		var request schema.TerminalOutputRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
		}
		if !h.validSession(request.SessionID) {
			return nil, &acp.RPCError{Code: -32602, Message: "unknown sessionId"}
		}
		return h.terminalOutput(request)
	case schema.TerminalWaitForExitMethodName:
		var request schema.WaitForTerminalExitRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
		}
		if !h.validSession(request.SessionID) {
			return nil, &acp.RPCError{Code: -32602, Message: "unknown sessionId"}
		}
		return h.waitForTerminal(ctx, request)
	case schema.TerminalKillMethodName:
		var request schema.KillTerminalRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
		}
		if !h.validSession(request.SessionID) {
			return nil, &acp.RPCError{Code: -32602, Message: "unknown sessionId"}
		}
		return h.killTerminal(request)
	case schema.TerminalReleaseMethodName:
		var request schema.ReleaseTerminalRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
		}
		if !h.validSession(request.SessionID) {
			return nil, &acp.RPCError{Code: -32602, Message: "unknown sessionId"}
		}
		return h.releaseTerminal(request)
	default:
		return nil, fmt.Errorf("unknown terminal operation")
	}
}

func (h *workspaceHost) createTerminal(request schema.CreateTerminalRequest) (any, error) {
	if request.Command == "" {
		return nil, fmt.Errorf("terminal command is required")
	}
	directory := h.directory
	if request.Cwd != nil {
		resolved, err := filepath.EvalSymlinks(*request.Cwd)
		if err != nil {
			return nil, err
		}
		if _, err := h.relative(resolved); err != nil {
			return nil, err
		}
		directory = resolved
	}
	limit := 1 << 20
	if request.OutputByteLimit != nil {
		outputLimit := *request.OutputByteLimit
		if outputLimit > uint64(limit) {
			outputLimit = uint64(limit)
		}
		limit = int(outputLimit)
	}
	env := make(map[string]string)
	for _, variable := range request.Env {
		env[variable.Name] = variable.Value
	}
	command := append([]string{request.Command}, request.Args...)
	t := &commandTerminal{
		command:  osrun.StartCommand(h.ctx, directory, command, env),
		tail:     osrun.Tail{Capacity: limit},
		finished: make(chan struct{}),
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing || h.ctx.Err() != nil {
		return nil, context.Canceled
	}
	if len(h.terminals) >= 32 {
		return nil, fmt.Errorf("release unused terminals before starting more")
	}
	h.next++
	id := fmt.Sprintf("t%d", h.next)
	t.command.Stdout = io.MultiWriter(&t.tail, &transcriptWriter{log: h.log, source: "Command output", id: id, record: h.record})
	t.command.Stderr = t.command.Stdout
	if err := t.command.Start(); err != nil {
		return nil, err
	}
	h.terminals[id] = t
	go func() {
		_ = t.command.Wait()
		// Reap any descendants holding pipes, then retire the process group
		// so a later release cannot signal a recycled PID.
		t.kill()
		code := int64(t.command.ProcessState.ExitCode())
		t.status.ExitCode = &code
		if status, ok := t.command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			t.status.ExitCode = nil
			signal := status.Signal().String()
			t.status.Signal = &signal
		}
		close(t.finished)
	}()
	return schema.CreateTerminalResponse{TerminalID: schema.TerminalId(id)}, nil
}

func (h *workspaceHost) terminalOutput(request schema.TerminalOutputRequest) (any, error) {
	h.mu.Lock()
	t := h.terminals[string(request.TerminalID)]
	h.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("unknown terminal %q", request.TerminalID)
	}
	output, truncated := t.tail.Text()
	result := schema.TerminalOutputResponse{Output: output, Truncated: truncated}
	select {
	case <-t.finished:
		status := t.status
		result.ExitStatus = &status
	default:
	}
	return result, nil
}

func (h *workspaceHost) waitForTerminal(ctx context.Context, request schema.WaitForTerminalExitRequest) (any, error) {
	h.mu.Lock()
	t := h.terminals[string(request.TerminalID)]
	h.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("unknown terminal %q", request.TerminalID)
	}
	select {
	case <-t.finished:
		return schema.WaitForTerminalExitResponse{ExitCode: t.status.ExitCode, Signal: t.status.Signal}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-h.ctx.Done():
		return nil, h.ctx.Err()
	}
}

func (h *workspaceHost) killTerminal(request schema.KillTerminalRequest) (any, error) {
	h.mu.Lock()
	t := h.terminals[string(request.TerminalID)]
	h.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("unknown terminal %q", request.TerminalID)
	}
	t.kill()
	return schema.KillTerminalResponse{}, nil
}

func (h *workspaceHost) releaseTerminal(request schema.ReleaseTerminalRequest) (any, error) {
	h.mu.Lock()
	t := h.terminals[string(request.TerminalID)]
	h.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("unknown terminal %q", request.TerminalID)
	}
	t.kill()
	<-t.finished
	h.mu.Lock()
	delete(h.terminals, string(request.TerminalID))
	h.mu.Unlock()
	return schema.ReleaseTerminalResponse{}, nil
}

func (h *workspaceHost) close() {
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
