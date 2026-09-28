package unstable

import (
	"encoding/json"
	"sync"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

// Optional session features travel inside session/update rather than as their
// own methods, so the right shape for them is a projection helper, not a request
// facade: the client receives them and needs ordered derived state. Projection is
// that helper for plan operations, compaction, notices, and end-turn token
// usage.
//
// It folds updates in wire order, exactly as the client observes them, and keeps
// the reference rules: a full plan replaces the previous plan, plan_update
// replaces the plan payload, plan_removed clears it, compaction updates upsert by
// id, notices are live events rather than history, and the latest usage update
// wins.

// PlanState is the latest plan payload for one session: either the entries of a
// plan update or the content of a plan_update, whichever arrived last.
type PlanState struct {
	Entries []schema.PlanEntry
	Content *schema.PlanUpdateContent
}

// CompactionState is the derived state of one compaction.
type CompactionState struct {
	ID      schema.CompactionId
	Status  schema.CompactionStatus
	Summary []schema.ContentBlock
	Error   *string
}

// UsageState is the latest end-turn token usage reported for one session.
type UsageState struct {
	Used uint64
	Size uint64
	Cost *schema.Cost
}

// Projection folds optional session updates into per-session derived state.
type Projection struct {
	mu          sync.Mutex
	plans       map[string]PlanState
	compactions map[string][]schema.CompactionId
	states      map[string]map[schema.CompactionId]*CompactionState
	notices     map[string][]schema.Notice
	usage       map[string]UsageState
}

// NewProjection returns an empty projection.
func NewProjection() *Projection {
	return &Projection{
		plans:       make(map[string]PlanState),
		compactions: make(map[string][]schema.CompactionId),
		states:      make(map[string]map[schema.CompactionId]*CompactionState),
		notices:     make(map[string][]schema.Notice),
		usage:       make(map[string]UsageState),
	}
}

// Notifications adapts session/update notifications into Observe calls. It
// decodes the unstable schema, so the optional update kinds stay visible; other
// notifications are ignored.
func (p *Projection) Notifications() acp.Notifications {
	return func(method string, raw json.RawMessage) error {
		if method != schema.SessionUpdateMethodName {
			return nil
		}
		var notification schema.SessionNotification
		if err := json.Unmarshal(raw, &notification); err != nil {
			return err
		}
		p.Observe(string(notification.SessionID), notification.Update)
		return nil
	}
}

// Observe folds one session update.
func (p *Projection) Observe(sessionID string, update schema.SessionUpdate) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case update.Plan != nil:
		p.plans[sessionID] = PlanState{Entries: update.Plan.Entries}
	case update.PlanUpdate != nil:
		content := update.PlanUpdate.Plan
		p.plans[sessionID] = PlanState{Content: &content}
	case update.PlanRemoved != nil:
		delete(p.plans, sessionID)
	case update.CompactionUpdate != nil:
		p.upsertCompaction(sessionID, update.CompactionUpdate)
	case update.CompactionSummaryChunk != nil:
		p.appendCompactionSummary(sessionID, update.CompactionSummaryChunk)
	case update.Notice != nil:
		p.notices[sessionID] = append(p.notices[sessionID], *update.Notice)
	case update.UsageUpdate != nil:
		p.usage[sessionID] = UsageState{
			Used: update.UsageUpdate.Used,
			Size: update.UsageUpdate.Size,
			Cost: update.UsageUpdate.Cost,
		}
	}
}

func (p *Projection) upsertCompaction(sessionID string, update *schema.CompactionUpdate) {
	states := p.states[sessionID]
	if states == nil {
		states = make(map[schema.CompactionId]*CompactionState)
		p.states[sessionID] = states
	}
	state, known := states[update.CompactionID]
	if !known {
		state = &CompactionState{}
		states[update.CompactionID] = state
		p.compactions[sessionID] = append(p.compactions[sessionID], update.CompactionID)
	}
	state.ID = update.CompactionID
	state.Status = update.Status
	state.Error = update.Error
	// The summary field is a complete replacement, not a chunk.
	state.Summary = append([]schema.ContentBlock(nil), update.Summary...)
}

func (p *Projection) appendCompactionSummary(sessionID string, chunk *schema.CompactionSummaryChunk) {
	states := p.states[sessionID]
	if states == nil {
		states = make(map[schema.CompactionId]*CompactionState)
		p.states[sessionID] = states
	}
	state, known := states[chunk.CompactionID]
	if !known {
		state = &CompactionState{ID: chunk.CompactionID}
		states[chunk.CompactionID] = state
		p.compactions[sessionID] = append(p.compactions[sessionID], chunk.CompactionID)
	}
	state.Summary = append(state.Summary, chunk.Content)
}

// Plan returns the current plan payload for a session.
func (p *Projection) Plan(sessionID string) (PlanState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	plan, ok := p.plans[sessionID]
	return plan, ok
}

// Compactions returns the session's compactions in the order they were first
// seen.
func (p *Projection) Compactions(sessionID string) []CompactionState {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := p.compactions[sessionID]
	states := make([]CompactionState, 0, len(ids))
	for _, id := range ids {
		state := p.states[sessionID][id]
		states = append(states, *state)
	}
	return states
}

// Notices returns the live notices received for a session since the last drain,
// without consuming them.
func (p *Projection) Notices(sessionID string) []schema.Notice {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]schema.Notice(nil), p.notices[sessionID]...)
}

// DrainNotices returns and clears the session's live notices.
func (p *Projection) DrainNotices(sessionID string) []schema.Notice {
	p.mu.Lock()
	defer p.mu.Unlock()
	notices := p.notices[sessionID]
	delete(p.notices, sessionID)
	return notices
}

// Usage returns the latest end-turn token usage for a session.
func (p *Projection) Usage(sessionID string) (UsageState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	usage, ok := p.usage[sessionID]
	return usage, ok
}
