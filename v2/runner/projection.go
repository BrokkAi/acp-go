package runner

import (
	"sync"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// projection is the Go equivalent of the reference SDK's one-shot
// AgentTextProjection. It accumulates only agent messages; chunks append by
// messageId, while message snapshots honor Nullable patch semantics.
type projection struct {
	order    []schema.MessageId
	messages map[schema.MessageId][]schema.ContentBlock
}

func newProjection() *projection {
	return &projection{messages: make(map[schema.MessageId][]schema.ContentBlock)}
}

func (p *projection) apply(update schema.SessionUpdate) {
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

func (p *projection) message(id schema.MessageId) {
	if _, exists := p.messages[id]; !exists {
		p.order = append(p.order, id)
		p.messages[id] = nil
	}
}

func (p *projection) text() string {
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

// sessionState follows the reference SDK's one-shot completion rule: ignore
// queued updates until the session reports running, then treat the next idle
// state as completion of foreground work.
type sessionState struct {
	observedRunning bool
	projection      *projection
	stopReason      *schema.StopReason
	done            chan struct{}
	once            sync.Once
}

func newSessionState() *sessionState {
	return &sessionState{projection: newProjection(), done: make(chan struct{})}
}

func (s *sessionState) apply(update schema.SessionUpdate) {
	if !s.observedRunning {
		if update.StateUpdate != nil && update.StateUpdate.Running != nil {
			s.observedRunning = true
		}
		return
	}
	if idle := update.StateUpdate; idle != nil && idle.Idle != nil {
		s.stopReason = idle.Idle.StopReason
		s.once.Do(func() { close(s.done) })
		return
	}
	s.projection.apply(update)
}

type runState struct {
	mu       sync.Mutex
	sessions map[schema.SessionId]*sessionState
}

func newRunState() *runState {
	return &runState{sessions: make(map[schema.SessionId]*sessionState)}
}

func (r *runState) notification(update schema.UpdateSessionNotification) {
	r.mu.Lock()
	state, exists := r.sessions[update.SessionID]
	if !exists {
		state = newSessionState()
		r.sessions[update.SessionID] = state
	}
	r.mu.Unlock()
	state.apply(update.Update)
}

func (r *runState) wait(sessionID schema.SessionId) <-chan struct{} {
	r.mu.Lock()
	state, exists := r.sessions[sessionID]
	if !exists {
		state = newSessionState()
		r.sessions[sessionID] = state
	}
	r.mu.Unlock()
	return state.done
}

func (r *runState) result(sessionID schema.SessionId) (string, *schema.StopReason) {
	r.mu.Lock()
	state := r.sessions[sessionID]
	r.mu.Unlock()
	if state == nil {
		return "", nil
	}
	return state.projection.text(), state.stopReason
}
