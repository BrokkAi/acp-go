package acp

import (
	"encoding/json"
	"slices"
	"sync"

	"github.com/BrokkAi/acp-go/schema"
)

// SessionProjection folds the session notices and compactions a client
// receives into per-session state, in receive order.
//
// A compaction is one ID-addressed entity. Its first compaction_update or
// compaction_summary_chunk creates it with status in_progress and fixes its
// position, so Compactions lists compactions in the order they were first
// seen. Later updates patch it in place: status is replaced, and summary,
// error, and _meta are patches, where an unset field keeps the stored value,
// null clears it, and a value replaces it (a _meta object replaces the stored
// metadata as a whole). summary: [] also clears. A summary chunk appends to
// the current summary, so a concrete summary replaces everything streamed so
// far and later chunks append to it. Status is an open enum: unknown values
// are kept as received.
//
// Notices are live events, not session history: Notices returns those
// received since the last DrainNotices, which consumes them.
//
// Replay through session/load goes through the same Observe as live
// delivery and, as in the protocol, does not reset omitted patch fields.
// Other session updates are ignored.
//
// Accessors return snapshots that later updates never change. Snapshots share
// storage with the projection, so callers must not modify them.
type SessionProjection struct {
	mu       sync.Mutex
	sessions map[SessionID]*projectedSession
}

// CompactionState is the stored state of one compaction. Summary, Error, and
// Meta are nil when unset or cleared.
type CompactionState struct {
	ID      schema.CompactionId
	Status  schema.CompactionStatus
	Summary []schema.ContentBlock
	Error   *string
	Meta    schema.Meta
}

type projectedSession struct {
	order       []schema.CompactionId
	compactions map[schema.CompactionId]*CompactionState
	notices     []schema.Notice
}

// NewSessionProjection returns an empty projection.
func NewSessionProjection() *SessionProjection {
	return &SessionProjection{sessions: make(map[SessionID]*projectedSession)}
}

// Notifications returns a notification adapter that observes each
// session/update before invoking the optional next callback, preserving wire
// order. A session/update that does not decode is returned as an error
// without reaching next.
func (p *SessionProjection) Notifications(next Notifications) Notifications {
	return func(method string, raw json.RawMessage) error {
		if method == schema.SessionUpdateMethodName {
			var update Update
			if err := json.Unmarshal(raw, &update); err != nil {
				return err
			}
			p.Observe(update.SessionID, update.Update)
		}
		if next == nil {
			return nil
		}
		return next(method, raw)
	}
}

// Observe folds one session update for the session in receive order.
func (p *SessionProjection) Observe(sessionID SessionID, update schema.SessionUpdate) {
	if update.Notice == nil && update.CompactionUpdate == nil && update.CompactionSummaryChunk == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	session := p.sessions[sessionID]
	if session == nil {
		session = &projectedSession{compactions: make(map[schema.CompactionId]*CompactionState)}
		p.sessions[sessionID] = session
	}
	switch {
	case update.Notice != nil:
		session.notices = append(session.notices, *update.Notice)
	case update.CompactionUpdate != nil:
		patch := update.CompactionUpdate
		compaction := session.compaction(patch.CompactionID)
		compaction.Status = patch.Status
		if patch.Summary.Set {
			compaction.Summary = nil
			if !patch.Summary.Null && len(patch.Summary.Value) > 0 {
				// Clipped, so later chunks allocate instead of writing
				// into the update's backing array.
				compaction.Summary = slices.Clip(patch.Summary.Value)
			}
		}
		if patch.Error.Set {
			compaction.Error = nil
			if !patch.Error.Null {
				message := patch.Error.Value
				compaction.Error = &message
			}
		}
		if patch.Meta.Set {
			compaction.Meta = nil
			if !patch.Meta.Null {
				compaction.Meta = patch.Meta.Value
			}
		}
	case update.CompactionSummaryChunk != nil:
		chunk := update.CompactionSummaryChunk
		compaction := session.compaction(chunk.CompactionID)
		compaction.Summary = append(compaction.Summary, chunk.Content)
	}
}

// compaction returns the stored compaction, creating a first-seen ID in
// progress with unset optional fields.
func (s *projectedSession) compaction(id schema.CompactionId) *CompactionState {
	if compaction := s.compactions[id]; compaction != nil {
		return compaction
	}
	compaction := &CompactionState{ID: id, Status: schema.CompactionStatusInProgress}
	s.compactions[id] = compaction
	s.order = append(s.order, id)
	return compaction
}

// Compactions returns the session's compactions in the order they were first
// seen.
func (p *SessionProjection) Compactions(sessionID SessionID) []CompactionState {
	p.mu.Lock()
	defer p.mu.Unlock()
	session := p.sessions[sessionID]
	if session == nil {
		return nil
	}
	compactions := make([]CompactionState, 0, len(session.order))
	for _, id := range session.order {
		compactions = append(compactions, session.compactions[id].snapshot())
	}
	return compactions
}

// Compaction returns the stored compaction with the given ID.
func (p *SessionProjection) Compaction(sessionID SessionID, id schema.CompactionId) (CompactionState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	session := p.sessions[sessionID]
	if session == nil || session.compactions[id] == nil {
		return CompactionState{}, false
	}
	return session.compactions[id].snapshot(), true
}

func (c *CompactionState) snapshot() CompactionState {
	snapshot := *c
	snapshot.Summary = slices.Clip(snapshot.Summary)
	return snapshot
}

// Notices returns the live notices received for a session since the last
// drain, in receive order, without consuming them.
func (p *SessionProjection) Notices(sessionID SessionID) []schema.Notice {
	p.mu.Lock()
	defer p.mu.Unlock()
	session := p.sessions[sessionID]
	if session == nil {
		return nil
	}
	return slices.Clone(session.notices)
}

// DrainNotices returns and clears the session's live notices.
func (p *SessionProjection) DrainNotices(sessionID SessionID) []schema.Notice {
	p.mu.Lock()
	defer p.mu.Unlock()
	session := p.sessions[sessionID]
	if session == nil {
		return nil
	}
	notices := session.notices
	session.notices = nil
	return notices
}

// Forget discards the session's compactions and notices, for example after
// the session is closed.
func (p *SessionProjection) Forget(sessionID SessionID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, sessionID)
}
