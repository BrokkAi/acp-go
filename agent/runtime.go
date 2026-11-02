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

	// Extensions, when set, serves every request the runtime does not handle
	// itself, such as the unstable methods that
	// github.com/BrokkAi/acp-go/unstable.Handle dispatches. It runs behind the
	// runtime's initialization gate. Unset, those requests answer -32601.
	Extensions acp.Handler

	// InitializeResult, when set, receives the agent's initialize response
	// and returns the value sent instead, typically the unstable initialize
	// response that advertises extension capabilities (see
	// github.com/BrokkAi/acp-go/unstable.Advertise). The runtime decodes the
	// returned value as a stable initialize response and validates that view
	// before sending the value.
	InitializeResult func(schema.InitializeResponse) (any, error)
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

	mu                sync.Mutex
	initialized       bool
	initializing      bool
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
		return nil, initializationRequired(method)
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
		return s.serveExtension(ctx, method, raw)
	}
}

func (s *server) initialize(ctx context.Context, raw json.RawMessage) (any, error) {
	request, err := decodeRequest[schema.InitializeRequest](raw)
	if err != nil {
		return nil, err
	}
	if request.ProtocolVersion != acp.Version {
		return nil, &acp.RPCError{Code: -32600, Message: fmt.Sprintf(
			"unsupported ACP protocol version %d; this endpoint only supports ACP protocol version %d",
			request.ProtocolVersion, acp.Version,
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
	if response.ProtocolVersion != acp.Version {
		return nil, fmt.Errorf("agent selected unsupported ACP version %d", response.ProtocolVersion)
	}
	s.client.initialize(request.ClientCapabilities)
	s.mu.Lock()
	s.initialized = true
	s.agentCapabilities = response.AgentCapabilities
	s.mu.Unlock()
	return result, nil
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
	updates := &promptUpdater{client: s.client, sessionID: request.SessionID}
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
	client    *clientConnection
	sessionID schema.SessionId
}

// Update sends through the client connection's Notify, which refuses, before
// writing to the wire, an update that needs a client capability the client
// did not advertise (see acp.RequireSessionUpdateCapability).
func (u *promptUpdater) Update(update schema.SessionUpdate) error {
	return u.client.Notify(context.Background(), schema.SessionUpdateMethodName, acp.Update{
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
