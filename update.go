package acp

import (
	"github.com/BrokkAi/acp-go/schema"
)

func sessionUpdate(sessionID SessionID, update schema.SessionUpdate) Update {
	return Update{SessionID: sessionID, Update: update}
}

func NewUserMessageChunkUpdate(sessionID SessionID, chunk schema.ContentChunk) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{UserMessageChunk: &chunk})
}

func NewAgentMessageChunkUpdate(sessionID SessionID, chunk schema.ContentChunk) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{AgentMessageChunk: &chunk})
}

func NewAgentThoughtChunkUpdate(sessionID SessionID, chunk schema.ContentChunk) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{AgentThoughtChunk: &chunk})
}

func NewToolCallUpdate(sessionID SessionID, call schema.ToolCall) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{ToolCall: &call})
}

func NewToolCallChangedUpdate(sessionID SessionID, update schema.ToolCallUpdate) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{ToolCallUpdate: &update})
}

func NewPlanUpdate(sessionID SessionID, plan schema.Plan) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{Plan: &plan})
}

func NewAvailableCommandsUpdate(sessionID SessionID, commands []schema.AvailableCommand) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{AvailableCommandsUpdate: &schema.AvailableCommandsUpdate{
		AvailableCommands: commands,
	}})
}

func NewCurrentModeUpdate(sessionID SessionID, modeID schema.SessionModeId) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{CurrentModeUpdate: &schema.CurrentModeUpdate{
		CurrentModeID: modeID,
	}})
}

func NewConfigOptionUpdate(sessionID SessionID, options []schema.SessionConfigOption) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{ConfigOptionUpdate: &schema.ConfigOptionUpdate{
		ConfigOptions: options,
	}})
}

// NewSessionInfoUpdate builds a session_info_update. Its title and updatedAt
// are patches: leave a field unset to keep the stored value, set it to null
// (schema.Nullable{Set: true, Null: true}) to clear it, or give a value
// (schema.Nullable{Set: true, Value: v}) to replace it. A Nullable without
// Set: true is omitted from the wire.
func NewSessionInfoUpdate(sessionID SessionID, info schema.SessionInfoUpdate) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{SessionInfoUpdate: &info})
}

func NewUsageUpdate(sessionID SessionID, used, size uint64) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{UsageUpdate: &schema.UsageUpdate{
		Used: used, Size: size,
	}})
}

// NewNoticeUpdate builds a live, user-facing notice. Agents send it only when
// the client advertised session.notices (see RequireSessionUpdateCapability).
func NewNoticeUpdate(sessionID SessionID, notice schema.Notice) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{Notice: &notice})
}

// NewCompactionUpdate builds a compaction_update upsert. Its summary, error,
// and _meta are patches: leave a field unset to keep the stored value, set
// it to null (schema.Nullable{Set: true, Null: true}) to clear it, or give a
// value (schema.Nullable{Set: true, Value: v}) to replace it. A Nullable
// without Set: true is omitted from the wire, so a value given without it is
// not sent. A summary of [] also clears; a nil slice is sent as null, so
// pass a non-nil empty slice to send []. Agents send it only when the client
// advertised session.compaction.
func NewCompactionUpdate(sessionID SessionID, update schema.CompactionUpdate) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{CompactionUpdate: &update})
}

// NewCompactionSummaryChunkUpdate builds a compaction_summary_chunk, which
// appends one content block to a compaction's summary. Agents send it only
// when the client advertised session.compaction.
func NewCompactionSummaryChunkUpdate(sessionID SessionID, chunk schema.CompactionSummaryChunk) Update {
	return sessionUpdate(sessionID, schema.SessionUpdate{CompactionSummaryChunk: &chunk})
}
