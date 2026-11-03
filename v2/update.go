package v2

import (
	"encoding/json"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/castparams"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// SessionUpdates adapts v2 session/update notifications to the generated
// discriminated-union shape.
func SessionUpdates(handle func(Update) error) acp.Notifications {
	return func(method string, raw json.RawMessage) error {
		if method != schema.SessionUpdateMethodName {
			return nil
		}
		var update Update
		if err := castparams.Unmarshal(raw, &update); err != nil {
			return err
		}
		return handle(update)
	}
}

func sessionUpdate(sessionID SessionID, update schema.SessionUpdate) Update {
	return Update{SessionID: sessionID, Update: update}
}

// NewNoticeUpdate builds a live, user-facing notice. v2 requires no client
// capability; clients that do not implement notices may ignore them.
func NewNoticeUpdate(sessionID SessionID, notice schema.Notice) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{Notice: &notice})
}

// NewCompactionUpdate builds a compaction_update upsert. Its summary, error,
// and _meta are patches: leave a field unset to keep the stored value, set
// it to null to clear it, or give a value to replace it. A summary of []
// also clears, and a _meta object patches the stored metadata by top-level
// key. v2 requires no client capability.
func NewCompactionUpdate(sessionID SessionID, update schema.CompactionUpdate) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{CompactionUpdate: &update})
}

// NewCompactionSummaryChunkUpdate builds a compaction_summary_chunk, which
// appends one content block to a compaction's summary.
func NewCompactionSummaryChunkUpdate(sessionID SessionID, chunk schema.CompactionSummaryChunk) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{CompactionSummaryChunk: &chunk})
}
