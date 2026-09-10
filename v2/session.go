package v2

import (
	"context"
	"errors"
	"fmt"

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
	permissions    *CancellablePermissions
}

// SessionHandleOptions connects optional connection-level state to one command
// handle. Tracker must be installed as a notification handler, and Permissions
// must be installed as the connection permission host.
type SessionHandleOptions struct {
	Tracker     *SessionTracker
	Permissions *CancellablePermissions
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
		permissions:    options.Permissions,
	}, nil
}

func (s *SessionHandle) ID() SessionID { return s.sessionID }

func (s *SessionHandle) Prompt(ctx context.Context, prompt string) error {
	return s.PromptContent(ctx, []Content{NewTextContent(prompt)})
}

// PromptContent submits a prompt and returns after acceptance. Output and
// completion remain independent session updates.
func (s *SessionHandle) PromptContent(ctx context.Context, prompt []Content) error {
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

func (s *SessionHandle) WaitForIdle(ctx context.Context) (WorkResult, error) {
	work, err := s.ActiveWork()
	if err != nil {
		return WorkResult{}, err
	}
	return work.Wait(ctx)
}

// CancelActiveWork cancels pending local permission requests and sends the
// v2 session-wide cancellation notification. Like Rust, it does not wait for
// idle; use CancelActiveWorkAndWait for a cancellation completion barrier.
func (s *SessionHandle) CancelActiveWork(ctx context.Context) error {
	if s.permissions != nil {
		s.permissions.CancelPermissionRequests(s.sessionID)
	}
	return s.connection.Notify(ctx, schema.SessionCancelMethodName, schema.CancelSessionNotification{
		SessionID: s.sessionID,
	})
}

func (s *SessionHandle) CancelActiveWorkAndWait(ctx context.Context) (WorkResult, error) {
	if err := s.CancelActiveWork(ctx); err != nil {
		return WorkResult{}, err
	}
	result, err := s.WaitForIdle(ctx)
	if err != nil {
		return result, err
	}
	if result.StopReason == nil || *result.StopReason != schema.StopReasonCancelled {
		return result, fmt.Errorf("active work stopped without cancelled reason: %v", result.StopReason)
	}
	return result, nil
}

func (s *SessionHandle) SetConfigOptionID(ctx context.Context, configID, valueID string) ([]schema.SessionConfigOption, error) {
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
	if request.ConfigID == "" {
		return nil, errors.New("configuration ID is required")
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

func (s *SessionHandle) Close(ctx context.Context) error {
	return s.connection.CloseSession(ctx, s.initialization, s.sessionID)
}
