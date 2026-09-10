package v2

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// UpdateProjection accumulates agent-message chunks and patch snapshots by
// messageId. It follows the reference SDK's one-shot text projection.
type UpdateProjection struct {
	mu       sync.RWMutex
	order    []schema.MessageId
	messages map[schema.MessageId][]schema.ContentBlock
}

func NewUpdateProjection() *UpdateProjection {
	return &UpdateProjection{messages: make(map[schema.MessageId][]schema.ContentBlock)}
}

func (p *UpdateProjection) Apply(update schema.SessionUpdate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case update.AgentMessageChunk != nil:
		chunk := update.AgentMessageChunk
		p.message(chunk.MessageID)
		p.messages[chunk.MessageID] = append(p.messages[chunk.MessageID], chunk.Content)
	case update.AgentMessage != nil:
		message := update.AgentMessage
		p.message(message.MessageID)
		switch {
		case !message.Content.Set:
			// Omitted content preserves the accumulated value.
		case message.Content.Null:
			p.messages[message.MessageID] = nil
		default:
			p.messages[message.MessageID] = append([]schema.ContentBlock(nil), message.Content.Value...)
		}
	}
}

func (p *UpdateProjection) Text() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]byte, 0, 128)
	for _, id := range p.order {
		for _, block := range p.messages[id] {
			if block.Text != nil {
				result = append(result, block.Text.Text...)
			}
		}
	}
	return string(result)
}

func (p *UpdateProjection) message(id schema.MessageId) {
	if _, exists := p.messages[id]; !exists {
		p.order = append(p.order, id)
		p.messages[id] = nil
	}
}

// WorkResult is the projected output and stop reason at an idle update.
type WorkResult struct {
	Text       string
	StopReason *schema.StopReason
}

// ActiveWork follows one foreground-work turn. Like the reference one-shot
// client, it ignores updates queued before Running and completes at the next
// Idle update.
type ActiveWork struct {
	mu              sync.RWMutex
	observedRunning bool
	projection      *UpdateProjection
	stopReason      *schema.StopReason
	done            chan struct{}
	once            sync.Once
}

func BeginActiveWork() *ActiveWork {
	return &ActiveWork{projection: NewUpdateProjection(), done: make(chan struct{})}
}

func (w *ActiveWork) Apply(update schema.SessionUpdate) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.observedRunning {
		if update.StateUpdate != nil && update.StateUpdate.Running != nil {
			w.observedRunning = true
		}
		return
	}
	if state := update.StateUpdate; state != nil && state.Idle != nil {
		if w.stopReason == nil {
			w.stopReason = state.Idle.StopReason
			w.once.Do(func() { close(w.done) })
		}
		return
	}
	w.projection.Apply(update)
}

func (w *ActiveWork) Done() <-chan struct{} { return w.done }

func (w *ActiveWork) Result() WorkResult {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return WorkResult{Text: w.projection.Text(), StopReason: w.stopReason}
}

func (w *ActiveWork) Wait(ctx context.Context) (WorkResult, error) {
	select {
	case <-w.done:
		return w.Result(), nil
	case <-ctx.Done():
		return WorkResult{}, ctx.Err()
	}
}

// SessionTracker owns one ActiveWork projection per session. Incoming traffic
// remains application-owned; install TrackerNotifications when the connection
// is created, before session setup or replay can begin.
type SessionTracker struct {
	mu       sync.Mutex
	sessions map[SessionID]*ActiveWork
}

func NewSessionTracker() *SessionTracker {
	return &SessionTracker{sessions: make(map[SessionID]*ActiveWork)}
}

// TrackerNotifications returns a notification adapter for this tracker. The
// optional next callback is invoked after the tracker has observed each
// session update, preserving connection wire order.
func (t *SessionTracker) TrackerNotifications(next acp.Notifications) acp.Notifications {
	return func(method string, raw json.RawMessage) error {
		if method == schema.SessionUpdateMethodName {
			var update Update
			if err := json.Unmarshal(raw, &update); err != nil {
				return err
			}
			t.Observe(update)
		}
		if next == nil {
			return nil
		}
		return next(method, raw)
	}
}

func (t *SessionTracker) Observe(update Update) {
	t.Work(update.SessionID).Apply(update.Update)
}

// Work returns the current work projection, creating one if needed.
func (t *SessionTracker) Work(sessionID SessionID) *ActiveWork {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.workLocked(sessionID)
}

// BeginWork resets the session's projection before submitting a new prompt.
func (t *SessionTracker) BeginWork(sessionID SessionID) *ActiveWork {
	t.mu.Lock()
	defer t.mu.Unlock()
	work := BeginActiveWork()
	t.sessions[sessionID] = work
	return work
}

func (t *SessionTracker) workLocked(sessionID SessionID) *ActiveWork {
	if work := t.sessions[sessionID]; work != nil {
		return work
	}
	work := BeginActiveWork()
	t.sessions[sessionID] = work
	return work
}
