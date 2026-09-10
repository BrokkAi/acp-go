package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

const version = schema.ProtocolVersion(2)

// Runtime owns the JSON-RPC loop for a draft-v2 Agent implementation.
type Runtime struct {
	Agent Agent
}

func New(implementation Agent) *Runtime {
	return &Runtime{Agent: implementation}
}

// Serve serves one ACP v2 stdio connection.
func (r *Runtime) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	if r.Agent == nil {
		return errors.New("agent implementation is required")
	}
	client := &clientConnection{connectionReady: make(chan struct{})}
	state := &server{agent: r.Agent, client: client}
	connection := acp.Connect(in, out, state.handle, state.notification)
	client.attach(connection)
	defer connection.Close()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-connection.Done():
		if err := connection.Err(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			return err
		}
		return nil
	}
}

type server struct {
	agent  Agent
	client *clientConnection

	mu          sync.Mutex
	initialized bool
	caps        *schema.AgentCapabilities
	auth        bool
}

func (s *server) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if method == schema.InitializeMethodName {
		return s.initialize(ctx, raw)
	}
	if !s.isInitialized() {
		return nil, &acp.RPCError{Code: -32600, Message: "agent is not initialized"}
	}

	switch method {
	case schema.AuthLoginMethodName:
		if !s.advertisesAuth() {
			return nil, methodNotFound(method)
		}
		implementation, ok := s.agent.(AuthLoginer)
		if !ok {
			return nil, methodNotFound(method)
		}
		request, err := decodeRequest[schema.LoginAuthRequest](raw)
		if err != nil {
			return nil, err
		}
		return implementation.AuthLogin(ctx, s.client, request)
	case schema.AuthLogoutMethodName:
		if !s.advertisesAuth() {
			return nil, methodNotFound(method)
		}
		implementation, ok := s.agent.(AuthLogouter)
		if !ok {
			return nil, methodNotFound(method)
		}
		request, err := decodeRequest[schema.LogoutAuthRequest](raw)
		if err != nil {
			return nil, err
		}
		return implementation.AuthLogout(ctx, s.client, request)
	case schema.SessionNewMethodName:
		if err := s.requireSessions(); err != nil {
			return nil, err
		}
		request, err := decodeRequest[schema.NewSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		return s.agent.NewSession(ctx, s.client, request)
	case schema.SessionResumeMethodName:
		if err := s.requireSessions(); err != nil {
			return nil, err
		}
		request, err := decodeRequest[schema.ResumeSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		return s.agent.ResumeSession(ctx, s.client, request)
	case schema.SessionCloseMethodName:
		if err := s.requireSessions(); err != nil {
			return nil, err
		}
		request, err := decodeRequest[schema.CloseSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		return s.agent.CloseSession(ctx, s.client, request)
	case schema.SessionListMethodName:
		if err := s.requireSessions(); err != nil {
			return nil, err
		}
		request, err := decodeRequest[schema.ListSessionsRequest](raw)
		if err != nil {
			return nil, err
		}
		return s.agent.ListSessions(ctx, s.client, request)
	case schema.SessionDeleteMethodName:
		if !s.advertisesDelete() {
			return nil, methodNotFound(method)
		}
		implementation, ok := s.agent.(SessionDeleter)
		if !ok {
			return nil, methodNotFound(method)
		}
		request, err := decodeRequest[schema.DeleteSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		return implementation.DeleteSession(ctx, s.client, request)
	case schema.SessionPromptMethodName:
		if err := s.requireSessions(); err != nil {
			return nil, err
		}
		request, err := decodeRequest[schema.PromptRequest](raw)
		if err != nil {
			return nil, err
		}
		updates := &sessionUpdater{connection: s.client.connection(), sessionID: request.SessionID}
		return s.agent.Prompt(ctx, s.client, request, updates)
	case schema.SessionSetConfigOptionMethodName:
		implementation, ok := s.agent.(ConfigOptionSetter)
		if !ok {
			return nil, methodNotFound(method)
		}
		request, err := decodeRequest[schema.SetSessionConfigOptionRequest](raw)
		if err != nil {
			return nil, err
		}
		return implementation.SetConfigOption(ctx, s.client, request)
	default:
		return nil, methodNotFound(method)
	}
}

func (s *server) initialize(ctx context.Context, raw json.RawMessage) (any, error) {
	request, err := decodeRequest[schema.InitializeRequest](raw)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.initialized {
		s.mu.Unlock()
		return nil, &acp.RPCError{Code: -32600, Message: "agent is already initialized"}
	}
	s.mu.Unlock()

	response, err := s.agent.Initialize(ctx, s.client, request)
	if err != nil {
		return nil, err
	}
	if response.ProtocolVersion != version {
		return nil, fmt.Errorf("agent selected unsupported ACP version %d", response.ProtocolVersion)
	}
	if response.Capabilities == nil || response.Capabilities.Session == nil {
		return nil, errors.New("v2 agent must advertise baseline session capabilities")
	}
	if len(response.AuthMethods) > 0 {
		_, canLogin := s.agent.(AuthLoginer)
		_, canLogout := s.agent.(AuthLogouter)
		if !canLogin || !canLogout {
			return nil, errors.New("authMethods requires both auth/login and auth/logout implementations")
		}
	}
	if response.Capabilities.Session.Delete != nil {
		if _, ok := s.agent.(SessionDeleter); !ok {
			return nil, errors.New("session.delete capability requires a SessionDeleter implementation")
		}
	}
	s.client.initialize(request.Capabilities)
	s.mu.Lock()
	s.initialized = true
	s.caps = response.Capabilities
	s.auth = len(response.AuthMethods) > 0
	s.mu.Unlock()
	return response, nil
}

func (s *server) notification(method string, raw json.RawMessage) error {
	if method != schema.SessionCancelMethodName {
		return nil
	}
	var notification schema.CancelSessionNotification
	if err := json.Unmarshal(raw, &notification); err != nil {
		return err
	}
	return s.agent.CancelSession(notification)
}

func (s *server) isInitialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initialized
}

func (s *server) requireSessions() error {
	if _, err := s.sessionCaps(); err != nil {
		return err
	}
	return nil
}

func (s *server) sessionCaps() (*schema.SessionCapabilities, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.caps == nil || s.caps.Session == nil {
		return nil, methodNotFound(schema.SessionNewMethodName)
	}
	return s.caps.Session, nil
}

func (s *server) advertisesAuth() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.auth
}

func (s *server) advertisesDelete() bool {
	caps, err := s.sessionCaps()
	return err == nil && caps.Delete != nil
}

type sessionUpdater struct {
	connection *acp.Connection
	sessionID  schema.SessionId
}

func (u *sessionUpdater) Update(update schema.SessionUpdate) error {
	return u.connection.Notify(context.Background(), schema.SessionUpdateMethodName, schema.UpdateSessionNotification{
		SessionID: u.sessionID,
		Update:    update,
	})
}

func decodeRequest[T any](raw json.RawMessage) (T, error) {
	var request T
	if err := json.Unmarshal(raw, &request); err != nil {
		return request, &acp.RPCError{Code: -32602, Message: err.Error()}
	}
	return request, nil
}

func methodNotFound(method string) error {
	return &acp.RPCError{Code: -32601, Message: "agent does not implement method: " + method}
}
