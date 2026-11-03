package unstable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	"github.com/BrokkAi/acp-go/internal/rpcctl"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

// SubagentsClientCapabilities returns the v1 subagents client capability,
// {}, for schema.ClientCapabilities.Subagents. It tells the agent that the
// client understands subagent_update associations, session_message and
// session_message_chunk, child work-state snapshots, and the restricted
// child-session rules. A v1 agent must not send those updates without it;
// send it with Initialize, since the stable acp.Capabilities cannot carry it:
//
//	capabilities := schema.ClientCapabilities{Subagents: unstable.SubagentsClientCapabilities()}
//	initialization, err := unstable.Initialize(ctx, connection, capabilities, info)
func SubagentsClientCapabilities() *schema.SubagentCapabilities {
	return &schema.SubagentCapabilities{}
}

// Initialize negotiates ACP v1 exactly as acp.Connection.InitializeWithInfo
// does, with the unstable client capabilities, which the stable
// acp.Capabilities drops, and returns the agent's response decoded as the
// unstable schema, which the facades in this package gate on. The client name
// and version are required, and the response's protocolVersion is checked as
// InitializeWithInfo checks it.
func Initialize(ctx context.Context, connection *acp.Connection, capabilities schema.ClientCapabilities, info schema.Implementation) (schema.InitializeResponse, error) {
	var result schema.InitializeResponse
	if info.Name == "" || info.Version == "" {
		return result, fmt.Errorf("client name and version are required")
	}
	request := schema.InitializeRequest{
		ClientCapabilities: &capabilities,
		ClientInfo:         &info,
		ProtocolVersion:    schema.ProtocolVersion(acp.Version),
	}
	var raw json.RawMessage
	if err := connection.Call(ctx, schema.InitializeMethodName, request, &raw); err != nil {
		return result, err
	}
	if err := rpcctl.V1InitializeResult(raw); err != nil {
		return result, err
	}
	return result, json.Unmarshal(raw, &result)
}

// AdvertisesSubagents reports whether client capabilities advertise
// subagents. An agent reads it from the initialize request decoded as the
// unstable schema; the v1 agent runtime checks it itself before writing a
// subagent update (see NotifySubagentUpdate).
func AdvertisesSubagents(capabilities *schema.ClientCapabilities) bool {
	return capabilities != nil && capabilities.Subagents != nil
}

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
// idle with schema.IdleStopReasonCancelled; use ErrorState for a failure.
func IdleState(stopReason schema.IdleStopReason) schema.StateUpdate {
	return schema.StateUpdate{Idle: &schema.IdleStateUpdate{StopReason: &stopReason}}
}

// ErrorState is the idle snapshot with stop reason error and, when not nil,
// the JSON-RPC error that ended the child's foreground work. In v1 it is the
// only place a client learns that a child's work failed; it does not close
// the child or fail its parent.
func ErrorState(failure *schema.Error) schema.StateUpdate {
	stopReason := schema.IdleStopReasonError
	return schema.StateUpdate{Idle: &schema.IdleStateUpdate{StopReason: &stopReason, Error: failure}}
}

// SubagentUpdateNotification is the session/update notification that sends
// update on the parent session: the first one for a child announces the
// association, later ones patch it.
func SubagentUpdateNotification(parent schema.SessionId, update schema.SubagentUpdate) schema.SessionNotification {
	return schema.SessionNotification{SessionID: parent, Update: schema.SessionUpdate{SubagentUpdate: &update}}
}

// SessionMessageNotification is the session/update notification that upserts
// message in the transcript of the session named by transcript.
func SessionMessageNotification(transcript schema.SessionId, message schema.SessionMessage) schema.SessionNotification {
	return schema.SessionNotification{SessionID: transcript, Update: schema.SessionUpdate{SessionMessage: &message}}
}

// SessionMessageChunkNotification is the session/update notification that
// appends chunk to a message in the transcript of the session named by
// transcript.
func SessionMessageChunkNotification(transcript schema.SessionId, chunk schema.SessionMessageChunk) schema.SessionNotification {
	return schema.SessionNotification{SessionID: transcript, Update: schema.SessionUpdate{SessionMessageChunk: &chunk}}
}

// NotifySubagentUpdate sends a subagent_update on the parent session. An
// agent must announce a child this way before any request or notification
// bearing the child's session ID, and must report a working directory that
// differs from the parent's before child traffic that depends on it.
//
// It fails before writing for a blank parent or child ID and a cwd or
// additional directory that is not absolute. The v1
// agent runtime's client refuses it, also before writing, when the client did
// not advertise clientCapabilities.subagents.
func NotifySubagentUpdate(ctx context.Context, client agent.Client, parent schema.SessionId, update schema.SubagentUpdate) error {
	if client == nil {
		return errors.New("subagent_update requires a client")
	}
	if err := validateSubagentUpdate(parent, update.SessionID, update.Cwd, update.AdditionalDirectories); err != nil {
		return err
	}
	return client.Notify(ctx, schema.SessionUpdateMethodName, SubagentUpdateNotification(parent, update))
}

// NotifySubagentState reports a child's current work state on its parent's
// association, the only v1 path for it: running or idle whenever observed
// foreground work starts, resumes, or stops; requires_action while it is
// blocked on the user; unknown when observability is lost.
func NotifySubagentState(ctx context.Context, client agent.Client, parent, child schema.SessionId, state schema.StateUpdate) error {
	return NotifySubagentUpdate(ctx, client, parent, schema.SubagentUpdate{SessionID: child, State: Value(state)})
}

// NotifySessionMessage upserts a session-directed message in the transcript
// of the session named by transcript. It fails before writing for a blank
// transcript or message ID. The v1
// agent runtime's client refuses it without clientCapabilities.subagents.
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
// transcript or message ID. The v1 agent runtime's client refuses it without
// clientCapabilities.subagents.
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

func validateSubagentUpdate[P ~string](parent, child schema.SessionId, cwd schema.Nullable[P], additional schema.Nullable[[]P]) error {
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
