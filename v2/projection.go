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

// SessionTracker owns, per session, an UpdateProjection of the session's
// entities and an ActiveWork projection of the current foreground work.
// Incoming traffic remains application-owned; install TrackerNotifications
// (and TrackerPermissions, to apply permission requests' tool calls) when the
// connection is created, before session setup or replay can begin.
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

// TrackerPermissions returns a permission host that applies the toolCall of
// each request's tool_call subject to the session projection, as a
// tool_call_update, before next handles the request. A nil next answers every
// request with the cancelled outcome.
func (t *SessionTracker) TrackerPermissions(next Permissions) Permissions {
	return trackerPermissions{tracker: t, next: next}
}

type trackerPermissions struct {
	tracker *SessionTracker
	next    Permissions
}

func (h trackerPermissions) RequestPermission(ctx context.Context, request schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
	h.tracker.Projection(request.SessionID).ApplyPermissionRequest(request)
	if h.next == nil {
		return CancelPermission(), nil
	}
	return h.next.RequestPermission(ctx, request)
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
