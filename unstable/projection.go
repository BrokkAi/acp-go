package unstable

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/BrokkAi/acp-go"
	stableschema "github.com/BrokkAi/acp-go/schema"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

// Optional session features travel inside session/update rather than as their
// own methods, so the right shape for them is a projection helper, not a request
// facade: the client receives them and needs ordered derived state. Projection is
// that helper for plan operations and end-turn token usage, and it carries the
// stable notices and compactions through acp.SessionProjection.
//
// It folds updates in wire order, exactly as the client observes them, and keeps
// the reference rules: a full plan replaces the previous plan, plan_update
// replaces the plan payload, plan_removed clears it, and the latest usage update
// wins. Notices and compactions follow acp.SessionProjection.

// PlanState is the latest plan payload for one session: either the entries of a
// plan update or the content of a plan_update, whichever arrived last.
type PlanState struct {
	Entries []schema.PlanEntry
	Content *schema.PlanUpdateContent
}

// CompactionState is the derived state of one compaction. Compaction is a
// stable ACP feature, so its state is acp.CompactionState.
type CompactionState = acp.CompactionState

// UsageState is the latest end-turn token usage reported for one session.
type UsageState struct {
	Used uint64
	Size uint64
	Cost *schema.Cost
}

// Projection folds optional session updates into per-session derived state.
type Projection struct {
	mu     sync.Mutex
	plans  map[string]PlanState
	usage  map[string]UsageState
	stable *acp.SessionProjection
}

// NewProjection returns an empty projection.
func NewProjection() *Projection {
	return &Projection{
		plans:  make(map[string]PlanState),
		usage:  make(map[string]UsageState),
		stable: acp.NewSessionProjection(),
	}
}

// Notifications adapts session/update notifications into Observe calls. It
// decodes the unstable schema, so the optional update kinds stay visible; other
// notifications are ignored. It returns the error when a notice or
// compaction does not convert to its stable form instead of dropping it.
func (p *Projection) Notifications() acp.Notifications {
	return func(method string, raw json.RawMessage) error {
		if method != schema.SessionUpdateMethodName {
			return nil
		}
		var notification schema.SessionNotification
		if err := json.Unmarshal(raw, &notification); err != nil {
			return err
		}
		return p.observe(string(notification.SessionID), notification.Update)
	}
}

// Observe folds one session update. Notices and compactions are handed to
// the stable acp.SessionProjection in their stable v1 form. Both schemas are
// ACP v1, so every update that decodes from the wire converts; Observe skips
// one that cannot be encoded, such as a content block with no variant set,
// which no peer could have sent. Notifications reports that case as an error.
func (p *Projection) Observe(sessionID string, update schema.SessionUpdate) {
	_ = p.observe(sessionID, update)
}

func (p *Projection) observe(sessionID string, update schema.SessionUpdate) error {
	if update.Notice != nil || update.CompactionUpdate != nil || update.CompactionSummaryChunk != nil {
		stable, err := stableUpdate(update)
		if err != nil {
			return fmt.Errorf("convert %s update to stable ACP v1: %w", update.Kind, err)
		}
		p.stable.Observe(acp.SessionID(sessionID), stable)
		return nil
	}
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
	case update.UsageUpdate != nil:
		p.usage[sessionID] = UsageState{
			Used: update.UsageUpdate.Used,
			Size: update.UsageUpdate.Size,
			Cost: update.UsageUpdate.Cost,
		}
	}
	return nil
}

func stableUpdate(update schema.SessionUpdate) (stableschema.SessionUpdate, error) {
	var stable stableschema.SessionUpdate
	encoded, err := json.Marshal(update)
	if err != nil {
		return stable, err
	}
	return stable, json.Unmarshal(encoded, &stable)
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
	return p.stable.Compactions(acp.SessionID(sessionID))
}

// Notices returns the live notices received for a session since the last drain,
// without consuming them.
func (p *Projection) Notices(sessionID string) []stableschema.Notice {
	return p.stable.Notices(acp.SessionID(sessionID))
}

// DrainNotices returns and clears the session's live notices.
func (p *Projection) DrainNotices(sessionID string) []stableschema.Notice {
	return p.stable.DrainNotices(acp.SessionID(sessionID))
}

// Usage returns the latest end-turn token usage for a session.
func (p *Projection) Usage(sessionID string) (UsageState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	usage, ok := p.usage[sessionID]
	return usage, ok
}
