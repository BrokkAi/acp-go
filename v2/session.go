package v2

import (
	"context"
	"errors"
	"fmt"

	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// SessionHandle is a command handle for one draft-v2 session. Like the Rust
// V2Session type, it does not own inbound updates or interactive requests;
// install tracker and host handlers when the connection is created.
type SessionHandle struct {
	connection     *Connection
	initialization Initialization
	sessionID      SessionID
	tracker        *SessionTracker
}

// SessionHandleOptions connects optional connection-level state to one command
// handle. Tracker must be installed as a notification handler. Permission
// requests are the connection permission host's, such as
// CancellablePermissions; the handle does not cancel them.
type SessionHandleOptions struct {
	Tracker *SessionTracker
}

func NewSessionHandle(connection *Connection, initialization Initialization, session Session, options SessionHandleOptions) (*SessionHandle, error) {
	if connection == nil {
		return nil, errors.New("connection is required")
	}
	if session.SessionID == "" {
		return nil, errors.New("session ID is required")
	}
	if err := requireSessionCapabilities(initialization); err != nil {
		return nil, err
	}
	return &SessionHandle{
		connection:     connection,
		initialization: initialization,
		sessionID:      session.SessionID,
		tracker:        options.Tracker,
	}, nil
}

func (s *SessionHandle) ID() SessionID { return s.sessionID }

func (s *SessionHandle) Prompt(ctx context.Context, prompt string) (MessageID, error) {
	return s.PromptContent(ctx, []Content{NewTextContent(prompt)})
}

// PromptContent submits a prompt and returns the ID of the user message the
// agent inserted into the conversation. Output and completion remain
// independent session updates. With a SessionTracker it first begins new
// tracked work, assuming the prompt starts new foreground work; see
// ActiveWork, and bound WaitForIdle with ctx.
func (s *SessionHandle) PromptContent(ctx context.Context, prompt []Content) (MessageID, error) {
	s.beginWork()
	return s.connection.PromptContent(
		ctx,
		s.initialization,
		Session{SessionID: s.sessionID},
		prompt,
	)
}

func (s *SessionHandle) beginWork() *ActiveWork {
	if s.tracker == nil {
		return nil
	}
	return s.tracker.BeginWork(s.sessionID)
}

// ActiveWork returns the current work projection for this session.
func (s *SessionHandle) ActiveWork() (*ActiveWork, error) {
	if s.tracker == nil {
		return nil, errors.New("session handle has no SessionTracker")
	}
	return s.tracker.Work(s.sessionID), nil
}

// WaitForIdle waits for the session's tracked work to complete. Tracking
// assumes each prompt starts new foreground work (see ActiveWork), so a wait
// can last until ctx ends; bound it with ctx.
func (s *SessionHandle) WaitForIdle(ctx context.Context) (WorkResult, error) {
	work, err := s.ActiveWork()
	if err != nil {
		return WorkResult{}, err
	}
	return work.Wait(ctx)
}

// CancelActiveWork sends the v2 session/cancel notification, which cancels
// the session's foreground work. Work in the latest background list is not
// covered and keeps running; cancel it individually with CancelToolCall or
// CancelTerminal. Like Rust, it does not wait for idle; use
// CancelActiveWorkAndWait for a cancellation completion barrier.
//
// Like Rust's cancel_active_work, it only sends the notification and cancels
// no permission request locally: only the Agent knows which work a request
// belongs to. The Agent stops waiting for the cancelled work's requests and
// cancels each with $/cancel_request, which CancellablePermissions answers
// with the cancelled outcome; requests for background work stay open. A
// request's subject is for display, not for choosing which to cancel. Call
// CancellablePermissions.CancelPermissionRequests to cancel every pending
// request of the session explicitly.
func (s *SessionHandle) CancelActiveWork(ctx context.Context) error {
	return s.connection.Notify(ctx, schema.SessionCancelMethodName, schema.CancelSessionNotification{
		SessionID: s.sessionID,
	})
}

// CancelActiveWorkAndWait cancels like CancelActiveWork, then waits for the
// tracked work to become idle. Only the cancelled stop reason confirms the
// cancellation, which per the v2 prompt lifecycle covers stops the user or
// Client chose. Any other idle, including error, a custom or unknown reason,
// or none, returns the work's result with an error wrapping
// ErrWorkNotCancelled (and the result's *WorkError for error).
//
// The result is that of the tracked work, the work begun for the latest
// prompt sent through this handle. If that work had already completed before
// the cancellation was sent, CancelActiveWorkAndWait returns its earlier
// result at once: a nil error when that work had itself ended cancelled, and
// ErrWorkNotCancelled otherwise. Neither confirms that this cancellation took
// effect; check ActiveWork's Done first when that matters. When no foreground
// work is active the Agent sends no idle update, and work that never reported
// an active state stays incomplete (see ActiveWork), so bound the wait with
// ctx.
func (s *SessionHandle) CancelActiveWorkAndWait(ctx context.Context) (WorkResult, error) {
	if err := s.CancelActiveWork(ctx); err != nil {
		return WorkResult{}, err
	}
	result, err := s.WaitForIdle(ctx)
	if err != nil {
		return result, err
	}
	if result.StopReason != nil && *result.StopReason == schema.StopReasonCancelled {
		return result, nil
	}
	reason := "no stop reason"
	if result.StopReason != nil {
		reason = fmt.Sprintf("stop reason %q", *result.StopReason)
	}
	if failure := result.Err(); failure != nil {
		return result, fmt.Errorf("%w (%s): %w", ErrWorkNotCancelled, reason, failure)
	}
	return result, fmt.Errorf("%w (%s)", ErrWorkNotCancelled, reason)
}

// CancelToolCall sends tool_call/cancel for one of the session's tool calls,
// as SessionTracker.CancelToolCall does; it requires a Tracker. The response
// does not confirm the cancellation: the tool call's final status does.
func (s *SessionHandle) CancelToolCall(ctx context.Context, toolCallID schema.ToolCallId) error {
	if s.tracker == nil {
		return errors.New("session handle has no SessionTracker")
	}
	return s.tracker.CancelToolCall(ctx, s.connection, s.sessionID, toolCallID)
}

// CancelTerminal sends terminal/cancel for one of the session's terminals,
// as SessionTracker.CancelTerminal does; it requires a Tracker. The response
// does not confirm the cancellation: the terminal's exitStatus does.
func (s *SessionHandle) CancelTerminal(ctx context.Context, terminalID schema.TerminalId) error {
	if s.tracker == nil {
		return errors.New("session handle has no SessionTracker")
	}
	return s.tracker.CancelTerminal(ctx, s.connection, s.sessionID, terminalID)
}

func (s *SessionHandle) SetConfigOptionID(ctx context.Context, configID, valueID string) ([]schema.SessionConfigOption, error) {
	if err := acpvalidate.Identifier("configuration value ID", valueID); err != nil {
		return nil, err
	}
	return s.SetConfigOption(ctx, schema.SetSessionConfigOptionRequest{
		SessionID: s.sessionID,
		ConfigID:  schema.SessionConfigId(configID),
		ID: &schema.SetSessionConfigOptionRequestID{
			Value: schema.SessionConfigValueId(valueID),
		},
	})
}

func (s *SessionHandle) SetConfigOptionBoolean(ctx context.Context, configID string, value bool) ([]schema.SessionConfigOption, error) {
	return s.SetConfigOption(ctx, schema.SetSessionConfigOptionRequest{
		SessionID: s.sessionID,
		ConfigID:  schema.SessionConfigId(configID),
		Boolean: &schema.SetSessionConfigOptionRequestBoolean{
			Value: value,
		},
	})
}

// SetConfigOption sends a generated request and returns the agent's
// authoritative replacement option set. The handle caches no mutable state.
func (s *SessionHandle) SetConfigOption(ctx context.Context, request schema.SetSessionConfigOptionRequest) ([]schema.SessionConfigOption, error) {
	request.SessionID = s.sessionID
	if err := acpvalidate.Identifier("configuration ID", string(request.ConfigID)); err != nil {
		return nil, err
	}
	set := 0
	if request.ID != nil {
		set++
	}
	if request.Boolean != nil {
		set++
	}
	if request.Other != nil {
		set++
	}
	if set != 1 {
		return nil, errors.New("exactly one configuration value variant is required")
	}
	var result schema.SetSessionConfigOptionResponse
	if err := s.connection.Call(ctx, schema.SessionSetConfigOptionMethodName, request, &result); err != nil {
		return nil, err
	}
	return result.ConfigOptions, nil
}

// Close closes the session and, once the agent confirms, drops its tracked
// projection and work.
func (s *SessionHandle) Close(ctx context.Context) error {
	if err := s.connection.CloseSession(ctx, s.initialization, s.sessionID); err != nil {
		return err
	}
	if s.tracker != nil {
		s.tracker.Forget(s.sessionID)
	}
	return nil
}
