package v2

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

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
	return &ActiveWork{projection: newTextProjection(), done: make(chan struct{})}
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

// SessionTracker owns, per session, an UpdateProjection of the session's
// entities and an ActiveWork projection of the current foreground work.
// Incoming traffic remains application-owned; install TrackerNotifications
// (and TrackerRequests through ConnectObserving, to apply permission
// requests' tool calls in receive order) when the connection is created,
// before session setup or replay can begin.
type SessionTracker struct {
	mu          sync.Mutex
	sessions    map[SessionID]*ActiveWork
	projections map[SessionID]*UpdateProjection
}

func NewSessionTracker() *SessionTracker {
	return &SessionTracker{
		sessions:    make(map[SessionID]*ActiveWork),
		projections: make(map[SessionID]*UpdateProjection),
	}
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

// TrackerRequests returns a request observer for ConnectObserving. It applies
// the toolCall of each session/request_permission tool_call subject to the
// session projection, like a tool_call_update, on the connection's read loop:
// in receive order relative to session/update notifications and before the
// permission handler runs, as the spec requires. A request whose params do not
// decode is left to its handler. The optional next observer runs afterwards.
func (t *SessionTracker) TrackerRequests(next acp.RequestObserver) acp.RequestObserver {
	return func(method string, raw json.RawMessage) {
		if method == schema.SessionRequestPermissionMethodName {
			var request schema.RequestPermissionRequest
			if json.Unmarshal(raw, &request) == nil {
				t.Projection(request.SessionID).ApplyPermissionRequest(request)
			}
		}
		if next != nil {
			next(method, raw)
		}
	}
}

// Observe applies one session update to the session's projection and to its
// current work. Replayed and live updates take the same path.
func (t *SessionTracker) Observe(update Update) {
	t.mu.Lock()
	projection := t.projectionLocked(update.SessionID)
	work := t.workLocked(update.SessionID)
	t.mu.Unlock()
	projection.Apply(update.Update)
	work.Apply(update.Update)
}

// Projection returns the session's entity projection, creating one if
// needed. Unlike the work projection, it spans every update observed for the
// session, including replay, and BeginWork does not reset it.
func (t *SessionTracker) Projection(sessionID SessionID) *UpdateProjection {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.projectionLocked(sessionID)
}

// ResetProjection discards the session's entity projection, so the next
// update starts a fresh one. Call it before a full replay: replay rebuilds
// the session's state rather than merging into stale state. Holders of the
// previous projection keep it, but it no longer receives updates.
func (t *SessionTracker) ResetProjection(sessionID SessionID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.projections, sessionID)
}

// Forget discards the session's entity projection and work, for example after
// the session is closed. A later update for the session starts both afresh.
func (t *SessionTracker) Forget(sessionID SessionID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.projections, sessionID)
	delete(t.sessions, sessionID)
}

// ResumeSessionFromStart resets the session's projection and work and then
// resumes it with replay from the start, so the replay rebuilds the session's
// state. Install TrackerNotifications on the connection first.
func (t *SessionTracker) ResumeSessionFromStart(ctx context.Context, connection *Connection, initialization Initialization, sessionID SessionID, directory string, additionalDirectories []string) (schema.ResumeSessionResponse, error) {
	t.Forget(sessionID)
	return connection.ResumeSessionFromStart(ctx, initialization, sessionID, directory, additionalDirectories)
}

func (t *SessionTracker) projectionLocked(sessionID SessionID) *UpdateProjection {
	if projection := t.projections[sessionID]; projection != nil {
		return projection
	}
	projection := NewUpdateProjection()
	t.projections[sessionID] = projection
	return projection
}

// Work returns the current work projection, creating one if needed.
func (t *SessionTracker) Work(sessionID SessionID) *ActiveWork {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.workLocked(sessionID)
}

// BeginWork resets the session's work projection before submitting a new
// prompt. The session's entity projection is kept.
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
