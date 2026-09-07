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

	"github.com/BrokkAi/acp-go/internal/osrun"
)

type terminalStatus struct {
	Code   *int   `json:"exitCode,omitempty"`
	Signal string `json:"signal,omitempty"`
}
type commandTerminal struct {
	command  *exec.Cmd
	tail     osrun.Tail
	finished chan struct{}
	status   terminalStatus
	once     sync.Once
}

func (t *commandTerminal) kill() { t.once.Do(func() { _ = osrun.Kill(t.command) }) }

func (h *workspaceHost) terminal(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var p struct {
		ID      string                         `json:"terminalId"`
		Command string                         `json:"command"`
		Args    []string                       `json:"args"`
		Cwd     *string                        `json:"cwd"`
		Env     []struct{ Name, Value string } `json:"env"`
		Limit   *int                           `json:"outputByteLimit"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if method == "terminal/create" {
		if p.Command == "" {
			return nil, fmt.Errorf("terminal command is required")
		}
		directory := h.directory
		if p.Cwd != nil {
			resolved, err := filepath.EvalSymlinks(*p.Cwd)
			if err != nil {
				return nil, err
			}
			if _, err := h.relative(resolved); err != nil {
				return nil, err
			}
			directory = resolved
		}
		limit := 1 << 20
		if p.Limit != nil {
			if *p.Limit < 0 {
				return nil, fmt.Errorf("negative output limit")
			}
			limit = min(limit, *p.Limit)
		}
		env := make(map[string]string)
		for _, v := range p.Env {
			env[v.Name] = v.Value
		}
		t := &commandTerminal{command: osrun.StartCommand(h.ctx, directory, append([]string{p.Command}, p.Args...), env), tail: osrun.Tail{Capacity: limit}, finished: make(chan struct{})}
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
			code := t.command.ProcessState.ExitCode()
			t.status.Code = &code
			if status, ok := t.command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				t.status.Code = nil
				t.status.Signal = status.Signal().String()
			}
			close(t.finished)
		}()
		return map[string]string{"terminalId": id}, nil
	}
	h.mu.Lock()
	t := h.terminals[p.ID]
	h.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("unknown terminal %q", p.ID)
	}
	switch method {
	case "terminal/output":
		text, truncated := t.tail.Text()
		result := map[string]any{"output": text, "truncated": truncated}
		select {
		case <-t.finished:
			result["exitStatus"] = t.status
		default:
		}
		return result, nil
	case "terminal/wait_for_exit":
		select {
		case <-t.finished:
			return t.status, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-h.ctx.Done():
			return nil, h.ctx.Err()
		}
	case "terminal/kill":
		t.kill()
		return nil, nil
	case "terminal/release":
		t.kill()
		<-t.finished
		h.mu.Lock()
		delete(h.terminals, p.ID)
		h.mu.Unlock()
		return nil, nil
	}
	return nil, fmt.Errorf("unknown terminal operation")
}
func (h *workspaceHost) close() {
	h.cancel()
	h.mu.Lock()
	h.closing = true
	terminals := make([]*commandTerminal, 0, len(h.terminals))
	for _, t := range h.terminals {
		terminals = append(terminals, t)
	}
	h.mu.Unlock()
	for _, t := range terminals {
		t.kill()
		<-t.finished
	}
	_ = h.root.Close()
}
