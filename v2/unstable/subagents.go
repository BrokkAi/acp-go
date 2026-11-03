package unstable

import (
	"context"
	"errors"

	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	"github.com/BrokkAi/acp-go/v2/agent"
)

// Subagent sessions are part of the draft-v2 baseline session model: there
// is no client capability, a client must understand subagent_update,
// session_message, and session_message_chunk, and an agent may send them on
// any connection.

// Value returns a patch field that sets value, for the nullable patch fields
// of schema.SubagentUpdate and schema.SessionMessage.
func Value[T any](value T) schema.Nullable[T] {
	return schema.Nullable[T]{Set: true, Value: value}
}

// Null returns a patch field that clears the stored value with an explicit
// null. The zero schema.Nullable omits the field, leaving the stored value
// unchanged.
func Null[T any]() schema.Nullable[T] {
	return schema.Nullable[T]{Set: true, Null: true}
}

// RunningState is the running work-state snapshot: foreground work is in
// progress.
func RunningState() schema.StateUpdate {
	return schema.StateUpdate{Running: &schema.RunningStateUpdate{}}
}

// RequiresActionState is the requires_action work-state snapshot: foreground
// work is blocked on user action.
func RequiresActionState() schema.StateUpdate {
	return schema.StateUpdate{RequiresAction: &schema.RequiresActionStateUpdate{}}
}

// UnknownState is the unknown work-state snapshot, which an agent must
// report when it loses observability of a child's foreground work. It is not
// a work outcome and does not close, cancel, or fail the child.
func UnknownState() schema.StateUpdate {
	return schema.StateUpdate{Unknown: &schema.UnknownStateUpdate{}}
}

// IdleState is the idle work-state snapshot with a stop reason. Idle does
// not end the child; the parent can assign it more work. Cancellation is
// idle with schema.StopReasonCancelled; use ErrorState for a failure.
func IdleState(stopReason schema.StopReason) schema.StateUpdate {
	return schema.StateUpdate{Idle: &schema.IdleStateUpdate{StopReason: &stopReason}}
}

// ErrorState is the idle snapshot with stop reason error and, when not nil,
// the JSON-RPC error that ended the foreground work. It does not close the
// child or fail its parent.
func ErrorState(failure *schema.Error) schema.StateUpdate {
	stopReason := schema.StopReasonError
	return schema.StateUpdate{Idle: &schema.IdleStateUpdate{StopReason: &stopReason, Error: failure}}
}

// SubagentUpdateNotification is the session/update notification that sends
// update on the parent session: the first one for a child announces the
// association, later ones patch it.
func SubagentUpdateNotification(parent schema.SessionId, update schema.SubagentUpdate) schema.UpdateSessionNotification {
	return schema.UpdateSessionNotification{SessionID: parent, Update: schema.SessionUpdate{SubagentUpdate: &update}}
}

// StateUpdateNotification is the session/update notification that reports
// state as the work state of the session sessionID.
func StateUpdateNotification(sessionID schema.SessionId, state schema.StateUpdate) schema.UpdateSessionNotification {
	return schema.UpdateSessionNotification{SessionID: sessionID, Update: schema.SessionUpdate{StateUpdate: &state}}
}

// SessionMessageNotification is the session/update notification that upserts
// message in the transcript of the session named by transcript.
func SessionMessageNotification(transcript schema.SessionId, message schema.SessionMessage) schema.UpdateSessionNotification {
	return schema.UpdateSessionNotification{SessionID: transcript, Update: schema.SessionUpdate{SessionMessage: &message}}
}

// SessionMessageChunkNotification is the session/update notification that
// appends chunk to a message in the transcript of the session named by
// transcript.
func SessionMessageChunkNotification(transcript schema.SessionId, chunk schema.SessionMessageChunk) schema.UpdateSessionNotification {
	return schema.UpdateSessionNotification{SessionID: transcript, Update: schema.SessionUpdate{SessionMessageChunk: &chunk}}
}

// NotifySubagentUpdate sends a subagent_update on the parent session. An
// agent must announce a child this way before any request or notification
// bearing the child's session ID, and must report a working directory that
// differs from the parent's before child traffic that depends on it.
//
// It fails before writing for a blank parent or child ID and a cwd or
// additional directory that is not absolute.
func NotifySubagentUpdate(ctx context.Context, client agent.Client, parent schema.SessionId, update schema.SubagentUpdate) error {
	if client == nil {
		return errors.New("subagent_update requires a client")
	}
	if err := validateSubagentUpdate(parent, update.SessionID, update.Cwd, update.AdditionalDirectories); err != nil {
		return err
	}
	return client.Notify(ctx, schema.SessionUpdateMethodName, SubagentUpdateNotification(parent, update))
}

// NotifySubagentState reports a child's work state on both paths the
// subagents RFD defines for v2: the child's own state_update first, then the
// same snapshot mirrored on its parent's association. Both describe one
// logical state, so a client applies the second idempotently.
func NotifySubagentState(ctx context.Context, client agent.Client, parent, child schema.SessionId, state schema.StateUpdate) error {
	if client == nil {
		return errors.New("state_update requires a client")
	}
	if err := validateSubagentUpdate(parent, child, schema.Nullable[schema.AbsolutePath]{}, schema.Nullable[[]schema.AbsolutePath]{}); err != nil {
		return err
	}
	if err := client.Notify(ctx, schema.SessionUpdateMethodName, StateUpdateNotification(child, state)); err != nil {
		return err
	}
	return client.Notify(ctx, schema.SessionUpdateMethodName, SubagentUpdateNotification(parent, schema.SubagentUpdate{SessionID: child, State: Value(state)}))
}

// NotifySessionMessage upserts a session-directed message in the transcript
// of the session named by transcript. It fails before writing for a blank
// transcript or message ID.
func NotifySessionMessage(ctx context.Context, client agent.Client, transcript schema.SessionId, message schema.SessionMessage) error {
	if client == nil {
		return errors.New("session_message requires a client")
	}
	if err := validateSessionMessage(transcript, message.MessageID); err != nil {
		return err
	}
	return client.Notify(ctx, schema.SessionUpdateMethodName, SessionMessageNotification(transcript, message))
}

// NotifySessionMessageChunk appends one content block to a session-directed
// message in the transcript of the session named by transcript. A chunk
// carries no participants; send them with NotifySessionMessage before the
// first chunk when they are known. It fails before writing for a blank
// transcript or message ID.
func NotifySessionMessageChunk(ctx context.Context, client agent.Client, transcript schema.SessionId, chunk schema.SessionMessageChunk) error {
	if client == nil {
		return errors.New("session_message_chunk requires a client")
	}
	if err := acpvalidate.Identifier("session ID", string(transcript)); err != nil {
		return err
	}
	if err := acpvalidate.Identifier("message ID", string(chunk.MessageID)); err != nil {
		return err
	}
	return client.Notify(ctx, schema.SessionUpdateMethodName, SessionMessageChunkNotification(transcript, chunk))
}

func validateSubagentUpdate(parent, child schema.SessionId, cwd schema.Nullable[schema.AbsolutePath], additional schema.Nullable[[]schema.AbsolutePath]) error {
	if err := acpvalidate.Identifier("parent session ID", string(parent)); err != nil {
		return err
	}
	if err := acpvalidate.Identifier("subagent session ID", string(child)); err != nil {
		return err
	}
	if cwd.Set && !cwd.Null {
		if err := acpvalidate.AbsolutePath("subagent working directory", string(cwd.Value)); err != nil {
			return err
		}
	}
	if additional.Set && !additional.Null {
		for _, directory := range additional.Value {
			if err := acpvalidate.AbsolutePath("subagent additional directory", string(directory)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSessionMessage(transcript schema.SessionId, messageID schema.MessageId) error {
	if err := acpvalidate.Identifier("session ID", string(transcript)); err != nil {
		return err
	}
	if err := acpvalidate.Identifier("message ID", string(messageID)); err != nil {
		return err
	}
	return nil
}
