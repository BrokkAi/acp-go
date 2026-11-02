package v2

import (
	"context"
	"encoding/json"
	"maps"
	"sync"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// WorkResult is the projected Agent text and the stop reason reported by the
// idle state update that ended one foreground-work turn.
type WorkResult struct {
	Text string
	// StopReason is why the work stopped, or nil when the idle update reported
	// no stop reason or a malformed one. The type is open: a custom or future
	// reason is kept as received, and its other fields are in
	// StopReasonFields. Without a recognized reason a client cannot infer how
	// the work ended.
	StopReason *schema.StopReason
	// Error is the failure the Agent reported beside the StopReasonError
	// stop reason, or nil when it reported none. It is always nil for other
	// stop reasons.
	Error *schema.Error
	// StopReasonFields keeps the other fields of a custom or future stop
	// reason, as received. It is nil for the known reasons.
	StopReasonFields map[string]json.RawMessage
}

// Err returns a *WorkError when the work ended with StopReasonError, so a
// caller can tell a failure from a completed turn without comparing stop
// reason strings. It returns nil for every other stop reason, including
// cancelled, custom, and unknown ones, and when none was reported.
func (r WorkResult) Err() error {
	if r.StopReason == nil || *r.StopReason != schema.StopReasonError {
		return nil
	}
	return &WorkError{Failure: r.Error}
}

// ActiveWork follows one foreground-work turn. Like the reference one-shot
// client, it ignores updates queued before the turn reports an active state
// (running, requires_action, or any state other than idle) and completes at
// the next idle update, whatever its stop reason: an omitted, null,
// malformed, custom, or unknown reason, an error, or a non-empty background
// list all end the foreground work. The result is fixed at that idle update;
// later updates, including a repeated idle snapshot, are not a new completion
// and do not change it.
//
// Tracking assumes each prompt starts new foreground work. The v2 prompt
// lifecycle lets a Client prompt in any state, and an Agent may instead insert
// a prompt sent during active work into the running turn, or hold it until
// that turn ends. A prompt that steers the current turn, or that the Agent
// never starts work for, may not be followed by a new active state, so the
// ActiveWork begun for it can stay incomplete. Bound every wait with ctx.
type ActiveWork struct {
	mu             sync.RWMutex
	observedActive bool
	completed      bool
	projection     *UpdateProjection
	result         WorkResult
	done           chan struct{}
}

func BeginActiveWork() *ActiveWork {
	return &ActiveWork{projection: newTextProjection(), done: make(chan struct{})}
}

func (w *ActiveWork) Apply(update schema.SessionUpdate) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.completed {
		return
	}
	state := update.StateUpdate
	if !w.observedActive {
		if state != nil && state.Idle == nil {
			w.observedActive = true
		}
		return
	}
	if state != nil && state.Idle != nil {
		idle := state.Idle
		w.result = WorkResult{
			Text:             w.projection.Text(),
			StopReason:       clonePtr(idle.StopReason),
			StopReasonFields: maps.Clone(idle.StopReasonFields),
		}
		if idle.StopReason != nil && *idle.StopReason == schema.StopReasonError {
			w.result.Error = clonePtr(idle.Error)
		}
		w.completed = true
		close(w.done)
		return
	}
	w.projection.Apply(update)
}

func (w *ActiveWork) Done() <-chan struct{} { return w.done }

// Result returns the work's result once Done is closed. Before then it
// returns the text projected so far and no stop reason.
func (w *ActiveWork) Result() WorkResult {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.completed {
		return w.result
	}
	return WorkResult{Text: w.projection.Text()}
}

// Wait blocks until the work completes or ctx ends. Completion with the error
// stop reason is not a Wait error; check WorkResult.Err.
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
// (and TrackerRequests through acp.WithRequestObserver, to apply permission
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

// TrackerRequests returns a request observer for acp.WithRequestObserver. It
// applies the toolCall of each session/request_permission tool_call subject to
// the session projection, like a tool_call_update, on the connection's read loop:
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
// prompt. The session's entity projection is kept. It assumes the prompt
// starts new foreground work; see ActiveWork for prompts sent during active
// work, and bound waits with ctx.
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

// clonePtr returns a shallow copy of *p, so a frozen WorkResult does not
// change when the caller later mutates the update it observed.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
