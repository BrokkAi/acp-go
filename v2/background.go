package v2

import (
	"context"
	"fmt"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// NotCancellableError is returned, before anything is written, when a client
// would send tool_call/cancel or terminal/cancel for a tool call or terminal
// that does not currently advertise capabilities.cancel in the session's
// projection, which the v2 spec forbids. Neither method has an
// initialize-time capability: the entity's own capabilities gate it.
type NotCancellableError struct {
	SessionID SessionID
	// Entity is the tool call (EntityToolCall) or terminal (EntityTerminal).
	Entity EntityRef
	// Known is false when the session's projection has no such entity.
	Known bool
}

func (e *NotCancellableError) Error() string {
	label := "tool call"
	if e.Entity.Kind == EntityTerminal {
		label = "terminal"
	}
	if !e.Known {
		return fmt.Sprintf("%s %q is unknown in session %q", label, e.Entity.ID, e.SessionID)
	}
	return fmt.Sprintf("%s %q in session %q does not advertise capabilities.cancel", label, e.Entity.ID, e.SessionID)
}

// CancelToolCall sends tool_call/cancel for one tool call of the session, a
// background or foreground tool call that advertises capabilities.cancel.
// It fails with a *NotCancellableError before writing unless the tool call
// currently advertises it in the session's projection (see
// UpdateProjection.CanCancelToolCall); install TrackerNotifications and
// TrackerRequests so that the projection is current.
//
// The empty response only means the Agent accepted the cancellation; the
// work may still be running. The Agent confirms with a tool_call_update to a
// final status, normally cancelled, though completed or failed may win if the
// work finished first. Read it from the projection (ToolCall) once it
// arrives. Cancelling a tool call does not change the foreground state. The
// Agent answers -32602 when the tool call is unknown, already final, or not
// cancellable, which a race with its final update can still produce.
//
// A subagent's child session permits neither method: with the unstable
// subagent facade, check SubagentTree.CheckClientMethod first, which refuses
// them on child sessions.
func (t *SessionTracker) CancelToolCall(ctx context.Context, connection *Connection, sessionID SessionID, toolCallID schema.ToolCallId) error {
	if t == nil {
		return fmt.Errorf("session tracker is required")
	}
	if connection == nil {
		return fmt.Errorf("connection is required")
	}
	projection := t.Projection(sessionID)
	if !projection.CanCancelToolCall(toolCallID) {
		_, known := projection.ToolCall(toolCallID)
		return &NotCancellableError{SessionID: sessionID, Entity: EntityRef{Kind: EntityToolCall, ID: string(toolCallID)}, Known: known}
	}
	var result schema.CancelToolCallResponse
	return connection.Call(ctx, schema.ToolCallCancelMethodName, schema.CancelToolCallRequest{
		SessionID: sessionID, ToolCallID: toolCallID,
	}, &result)
}

// CancelTerminal sends terminal/cancel to stop the process of one terminal
// of the session. It fails with a *NotCancellableError before writing unless
// the terminal currently advertises capabilities.cancel in the session's
// projection (see UpdateProjection.CanCancelTerminal).
//
// The empty response only means the Agent accepted the cancellation; the
// process may still be running. The Agent confirms with a terminal_update
// that sets exitStatus. The terminal is not released: its output stays
// available, and neither the foreground state nor the status of a tool call
// that references the terminal changes. The Agent answers -32602 when the
// terminal is unknown, has already exited, or is not cancellable. Like
// CancelToolCall, it is refused on a subagent's child session by
// SubagentTree.CheckClientMethod.
func (t *SessionTracker) CancelTerminal(ctx context.Context, connection *Connection, sessionID SessionID, terminalID schema.TerminalId) error {
	if t == nil {
		return fmt.Errorf("session tracker is required")
	}
	if connection == nil {
		return fmt.Errorf("connection is required")
	}
	projection := t.Projection(sessionID)
	if !projection.CanCancelTerminal(terminalID) {
		_, known := projection.Terminal(terminalID)
		return &NotCancellableError{SessionID: sessionID, Entity: EntityRef{Kind: EntityTerminal, ID: string(terminalID)}, Known: known}
	}
	var result schema.CancelTerminalResponse
	return connection.Call(ctx, schema.TerminalCancelMethodName, schema.CancelTerminalRequest{
		SessionID: sessionID, TerminalID: terminalID,
	}, &result)
}
