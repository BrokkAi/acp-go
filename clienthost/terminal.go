package clienthost

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

func (h *Host) terminal(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case schema.TerminalCreateMethodName:
		var request schema.CreateTerminalRequest
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

func (h *Host) createTerminal(request schema.CreateTerminalRequest) (any, error) {
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
	limit := defaultTerminalOutput
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
	if len(h.terminals) >= maxTerminals {
		return nil, fmt.Errorf("release unused terminals before starting more")
	}
	h.next++
	id := fmt.Sprintf("t%d", h.next)
	t.command.Stdout = io.MultiWriter(&t.tail, h.newProcessWriter("Command output", id))
	t.command.Stderr = t.command.Stdout
	if err := t.command.Start(); err != nil {
		return nil, err
	}
	h.terminals[id] = t
	go func() {
		_ = t.command.Wait()
		t.kill()
		code := uint32(t.command.ProcessState.ExitCode())
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

func (h *Host) terminalOutput(request schema.TerminalOutputRequest) (any, error) {
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

func (h *Host) waitForTerminal(ctx context.Context, request schema.WaitForTerminalExitRequest) (any, error) {
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

func (h *Host) killTerminal(request schema.KillTerminalRequest) (any, error) {
	h.mu.Lock()
	t := h.terminals[string(request.TerminalID)]
	h.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("unknown terminal %q", request.TerminalID)
	}
	t.kill()
	return schema.KillTerminalResponse{}, nil
}

func (h *Host) releaseTerminal(request schema.ReleaseTerminalRequest) (any, error) {
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
