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

	// Extensions, when set, serves every request the runtime does not handle
	// itself, such as the unstable methods that
	// github.com/BrokkAi/acp-go/v2/unstable.Handle dispatches. It runs behind
	// the runtime's initialization gate. Unset, those requests answer -32601.
	Extensions acp.Handler

	// InitializeResult, when set, receives the agent's initialize response
	// and returns the value sent instead, typically the unstable initialize
	// response that advertises extension capabilities (see
	// github.com/BrokkAi/acp-go/v2/unstable.Advertise). The runtime decodes the
	// returned value as a stable v2 initialize response and validates that
	// view before sending the value.
	InitializeResult func(schema.InitializeResponse) (any, error)
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
	state := &server{agent: r.Agent, client: client, extensions: r.Extensions, initializeResult: r.InitializeResult}
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
	agent            Agent
	client           *clientConnection
	extensions       acp.Handler
	initializeResult func(schema.InitializeResponse) (any, error)

	mu           sync.Mutex
	initialized  bool
	initializing bool
	caps         *schema.AgentCapabilities
	auth         bool
}

func (s *server) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if method == schema.InitializeMethodName {
		return s.initialize(ctx, raw)
	}
	if !s.isInitialized() {
		return nil, initializationRequired(method)
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
		response, err := s.agent.Prompt(ctx, s.client, request, updates)
		if err != nil {
			return nil, err
		}
		if response.MessageID == "" {
			return nil, &acp.RPCError{Code: -32603, Message: "agent returned a prompt response without the required user message ID"}
		}
		return response, nil
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
		return s.serveExtension(ctx, method, raw)
	}
}

func (s *server) initialize(ctx context.Context, raw json.RawMessage) (any, error) {
	request, err := decodeRequest[schema.InitializeRequest](raw)
	if err != nil {
		return nil, err
	}
	if request.ProtocolVersion != version {
		return nil, &acp.RPCError{Code: -32600, Message: fmt.Sprintf(
			"unsupported ACP protocol version %d; this endpoint only supports ACP protocol version 2",
			request.ProtocolVersion,
		)}
	}
	s.mu.Lock()
	if s.initialized {
		s.mu.Unlock()
		// The reference SDK's refusal for a repeat it does not accept. It
		// also accepts a repeat sent before any other request; until #62
		// implements that, every repeat is refused here.
		return nil, invalidRequest("ACP initialization cannot be repeated after any other request; reconnect to initialize again")
	}
	if s.initializing {
		s.mu.Unlock()
		return nil, invalidRequest("ACP initialization is already in progress on this connection")
	}
	s.initializing = true
	s.mu.Unlock()
	// Clear the in-progress state on every exit so that a failed attempt
	// leaves the connection uninitialized and a later initialize can retry.
	defer func() {
		s.mu.Lock()
		s.initializing = false
		s.mu.Unlock()
	}()

	response, err := s.agent.Initialize(ctx, s.client, request)
	if err != nil {
		return nil, err
	}
	result, response, err := s.extendInitialize(response)
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
	return result, nil
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

// invalidRequest builds a -32600 error in the reference SDK's shape: the
// generic JSON-RPC message, with the explanation as a JSON string in data.
func invalidRequest(detail string) error {
	data, _ := json.Marshal(detail)
	return &acp.RPCError{Code: -32600, Message: "Invalid request", Data: data}
}

// initializationRequired refuses a request that arrives before initialize
// has completed, matching the reference SDK.
func initializationRequired(method string) error {
	return invalidRequest(fmt.Sprintf("ACP initialization must complete before `%s` can be used", method))
}

func methodNotFound(method string) error {
	return &acp.RPCError{Code: -32601, Message: "agent does not implement method: " + method}
}
