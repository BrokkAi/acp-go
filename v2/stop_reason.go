package v2

import (
	"errors"
	"fmt"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// WorkError reports foreground work that the Agent ended with the
// StopReasonError stop reason. WorkResult.Err and the v2 runner return it.
// The prompt that started the work was still accepted, and any output the
// work produced stays in the WorkResult.
type WorkError struct {
	// Failure is the JSON-RPC error object the Agent reported beside the stop
	// reason, or nil when it reported no failure details. Its Code can be
	// handled as for an error response; for example, -32000 (authentication
	// required) can prompt the user to authenticate before the next prompt.
	Failure *schema.Error
}

func (e *WorkError) Error() string {
	if e.Failure == nil {
		return "agent reported that foreground work failed"
	}
	return fmt.Sprintf("agent reported that foreground work failed: ACP error %d: %s", e.Failure.Code, e.Failure.Message)
}

// ErrWorkNotCancelled is returned, wrapped, by
// SessionHandle.CancelActiveWorkAndWait when the active work became idle with
// a stop reason other than cancelled, or with none. Only cancelled confirms a
// cancellation: the Agent must report it, not error, even when aborting
// raised errors, and it reports another reason when the work ended for its
// own reasons before the cancellation took effect.
var ErrWorkNotCancelled = errors.New("active work became idle without the cancelled stop reason")

// NewIdleStateUpdate returns an idle state_update session update. A non-empty
// reason is reported as its stop reason; an empty reason reports none, for an
// idle update that does not end foreground work or whose cause the Agent
// cannot determine. Use NewErrorIdleStateUpdate to attach failure details to
// StopReasonError. A custom reason must begin with an underscore; like the
// reference SDK, the helper does not check it.
func NewIdleStateUpdate(reason schema.StopReason) schema.SessionUpdate {
	idle := &schema.IdleStateUpdate{}
	if reason != "" {
		idle.StopReason = &reason
	}
	return schema.SessionUpdate{
		Kind:        schema.SessionUpdateKindStateUpdate,
		StateUpdate: &schema.StateUpdate{Kind: schema.StateUpdateKindIdle, Idle: idle},
	}
}

// NewErrorIdleStateUpdate returns an idle state_update session update with
// the error stop reason, for foreground work that ended because something
// failed. failure is the JSON-RPC error object sent beside the stop reason;
// the Agent should include it, and nil omits it.
//
// The error stop reason is only for failures that end foreground work. A
// failure before the prompt's user message is inserted still needs an error
// response to session/prompt, a failure the Agent recovers from is a notice
// or a failed tool call, and cancelled work ends with StopReasonCancelled
// even when aborting it raised errors.
func NewErrorIdleStateUpdate(failure *schema.Error) schema.SessionUpdate {
	update := NewIdleStateUpdate(schema.StopReasonError)
	update.StateUpdate.Idle.Error = failure
	return update
}
