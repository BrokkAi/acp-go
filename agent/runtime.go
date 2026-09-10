package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
)

// Runtime owns the JSON-RPC loop for an Agent implementation.
type Runtime struct {
	Agent Agent
}

func New(implementation Agent) *Runtime {
	return &Runtime{Agent: implementation}
}

// Serve serves one ACP stdio connection. It returns nil for ordinary pipe
// closure and preserves non-EOF transport failures.
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

	mu                sync.Mutex
	initialized       bool
	agentCapabilities *schema.AgentCapabilities
	promptCancels     map[schema.SessionId]*promptCancel
}

type promptCancel struct {
	cancel context.CancelFunc
}

func (s *server) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if method == schema.InitializeMethodName {
		return s.initialize(ctx, raw)
	}
	if !s.isInitialized() {
		return nil, &acp.RPCError{Code: -32600, Message: "agent is not initialized"}
	}

	switch method {
	case schema.AuthenticateMethodName:
		implementation, ok := s.agent.(Authenticator)
		if !ok {
			return nil, methodNotFound(method)
		}
		request, err := decodeRequest[schema.AuthenticateRequest](raw)
		if err != nil {
			return nil, err
		}
		return implementation.Authenticate(ctx, s.client, request)
	case schema.SessionNewMethodName:
		request, err := decodeRequest[schema.NewSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		return s.agent.NewSession(ctx, s.client, request)
	case schema.SessionLoadMethodName:
		if !s.advertisesMethod(method) {
			return nil, methodNotFound(method)
		}
		implementation, ok := s.agent.(SessionLoader)
		if !ok {
			return nil, methodNotFound(method)
		}
		request, err := decodeRequest[schema.LoadSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		return implementation.LoadSession(ctx, s.client, request)
	case schema.SessionResumeMethodName:
		if !s.advertisesMethod(method) {
			return nil, methodNotFound(method)
		}
		implementation, ok := s.agent.(SessionResumer)
		if !ok {
			return nil, methodNotFound(method)
		}
		request, err := decodeRequest[schema.ResumeSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		return implementation.ResumeSession(ctx, s.client, request)
	case schema.SessionCloseMethodName:
		if !s.advertisesMethod(method) {
			return nil, methodNotFound(method)
		}
		implementation, ok := s.agent.(SessionCloser)
		if !ok {
			return nil, methodNotFound(method)
		}
		request, err := decodeRequest[schema.CloseSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		return implementation.CloseSession(ctx, s.client, request)
	case schema.SessionListMethodName:
		if !s.advertisesMethod(method) {
			return nil, methodNotFound(method)
		}
		implementation, ok := s.agent.(SessionLister)
		if !ok {
			return nil, methodNotFound(method)
		}
		request, err := decodeRequest[schema.ListSessionsRequest](raw)
		if err != nil {
			return nil, err
		}
		return implementation.ListSessions(ctx, s.client, request)
	case schema.SessionDeleteMethodName:
		if !s.advertisesMethod(method) {
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
		return s.prompt(ctx, raw)
	case schema.SessionSetModeMethodName:
		implementation, ok := s.agent.(ModeSetter)
		if !ok {
			return nil, methodNotFound(method)
		}
		request, err := decodeRequest[schema.SetSessionModeRequest](raw)
		if err != nil {
			return nil, err
		}
		return implementation.SetMode(ctx, s.client, request)
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
	case schema.LogoutMethodName:
		if !s.advertisesMethod(method) {
			return nil, methodNotFound(method)
		}
		implementation, ok := s.agent.(LoggerOuter)
		if !ok {
			return nil, methodNotFound(method)
		}
		request, err := decodeRequest[schema.LogoutRequest](raw)
		if err != nil {
			return nil, err
		}
		return implementation.Logout(ctx, s.client, request)
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
	if response.ProtocolVersion != acp.Version {
		return nil, fmt.Errorf("agent selected unsupported ACP version %d", response.ProtocolVersion)
	}
	s.client.initialize(request.ClientCapabilities)
	s.mu.Lock()
	s.initialized = true
	s.agentCapabilities = response.AgentCapabilities
	s.mu.Unlock()
	return response, nil
}

func (s *server) advertisesMethod(method string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agentCapabilities == nil {
		return false
	}
	switch method {
	case schema.SessionLoadMethodName:
		return s.agentCapabilities.LoadSession != nil && *s.agentCapabilities.LoadSession
	case schema.SessionResumeMethodName:
		return s.agentCapabilities.SessionCapabilities != nil && s.agentCapabilities.SessionCapabilities.Resume != nil
	case schema.SessionCloseMethodName:
		return s.agentCapabilities.SessionCapabilities != nil && s.agentCapabilities.SessionCapabilities.Close != nil
	case schema.SessionListMethodName:
		return s.agentCapabilities.SessionCapabilities != nil && s.agentCapabilities.SessionCapabilities.List != nil
	case schema.SessionDeleteMethodName:
		return s.agentCapabilities.SessionCapabilities != nil && s.agentCapabilities.SessionCapabilities.Delete != nil
	case schema.LogoutMethodName:
		return s.agentCapabilities.Auth != nil && s.agentCapabilities.Auth.Logout != nil
	default:
		return true
	}
}

func (s *server) prompt(parent context.Context, raw json.RawMessage) (any, error) {
	request, err := decodeRequest[schema.PromptRequest](raw)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	prompt := s.trackPrompt(request.SessionID, cancel)
	defer func() {
		cancel()
		s.untrackPrompt(request.SessionID, prompt)
	}()
	updates := &promptUpdater{connection: s.client.connection(), sessionID: request.SessionID}
	return s.agent.Prompt(ctx, s.client, request, updates)
}

func (s *server) notification(method string, raw json.RawMessage) error {
	if method != schema.SessionCancelMethodName {
		return nil
	}
	var notification schema.CancelNotification
	if err := json.Unmarshal(raw, &notification); err != nil {
		return err
	}
	s.mu.Lock()
	prompt := s.promptCancels[notification.SessionID]
	s.mu.Unlock()
	if prompt != nil {
		prompt.cancel()
	}
	if implementation, ok := s.agent.(SessionCanceler); ok {
		return implementation.CancelSession(context.Background(), notification)
	}
	return nil
}

func (s *server) isInitialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initialized
}

func (s *server) trackPrompt(sessionID schema.SessionId, cancel context.CancelFunc) *promptCancel {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.promptCancels == nil {
		s.promptCancels = make(map[schema.SessionId]*promptCancel)
	}
	prompt := &promptCancel{cancel: cancel}
	s.promptCancels[sessionID] = prompt
	return prompt
}

func (s *server) untrackPrompt(sessionID schema.SessionId, prompt *promptCancel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.promptCancels[sessionID]; ok && current == prompt {
		delete(s.promptCancels, sessionID)
	}
}

type promptUpdater struct {
	connection *acp.Connection
	sessionID  schema.SessionId
}

func (u *promptUpdater) Update(update schema.SessionUpdate) error {
	return u.connection.Notify(context.Background(), schema.SessionUpdateMethodName, acp.Update{
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
