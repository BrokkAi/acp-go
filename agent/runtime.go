package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/initresult"
	"github.com/BrokkAi/acp-go/internal/negotiate"
	"github.com/BrokkAi/acp-go/internal/rpcctl"
	"github.com/BrokkAi/acp-go/schema"
)

// Runtime owns the JSON-RPC loop for an Agent implementation.
type Runtime struct {
	Agent Agent

	// Extensions, when set, serves every request the runtime does not handle
	// itself, such as the unstable methods that
	// github.com/BrokkAi/acp-go/unstable.Handle dispatches. It runs behind the
	// runtime's initialization gate. Unset, those requests answer -32601.
	// The handler reaches the connection's Client through ClientFromContext.
	Extensions acp.Handler

	// InitializeResult, when set, receives the agent's initialize response
	// and returns the value sent instead, typically the unstable initialize
	// response that advertises extension capabilities (see
	// github.com/BrokkAi/acp-go/unstable.Advertise). The runtime decodes the
	// returned value as a stable initialize response and validates that view
	// before sending the value.
	InitializeResult func(schema.InitializeResponse) (any, error)

	// Overrides, when set, serves the named methods in place of the runtime's
	// own dispatch to Agent, keyed by method name. An override receives the
	// raw params, so it can read and answer fields the stable schema does not
	// carry. It runs behind the initialization gate and the method's
	// capability check (session/list still requires
	// sessionCapabilities.list, for example), and takes precedence over
	// Extensions for methods the runtime does not own. initialize and
	// session/prompt cannot be overridden, because the runtime itself
	// validates the handshake and tracks prompt cancellation, and
	// notifications such as session/cancel cannot either, because overrides
	// answer requests; Serve refuses those keys and nil handlers before
	// reading from the connection. An override, like an Extensions handler,
	// reaches the connection's Client through ClientFromContext, so a
	// session/load override can stream its replay as session/update
	// notifications before it answers.
	// github.com/BrokkAi/acp-go/unstable.Install sets the overrides the
	// unstable facades need, such as session/list with the archived filter.
	Overrides map[string]acp.Handler
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
	overrides, err := checkOverrides(r.Overrides)
	if err != nil {
		return err
	}
	client := &clientConnection{connectionReady: make(chan struct{})}
	state := &server{agent: r.Agent, client: client, extensions: r.Extensions, initializeResult: r.InitializeResult, overrides: overrides}
	connection := acp.Connect(in, out, state.handle, state.notification,
		rpcctl.WithRequestGuard(state.guard).(acp.ConnectOption),
		rpcctl.WithOutgoingGuard(outgoing).(acp.ConnectOption),
		rpcctl.WithOutgoingResponseGuard(state.outgoingResponse).(acp.ConnectOption))
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
	overrides        map[string]acp.Handler

	mu                sync.Mutex
	initialized       bool
	agentCapabilities *schema.AgentCapabilities
	promptCancels     map[schema.SessionId]*promptCancel
	// sent is the encoded initialize response, which handlers read through
	// initresult.From.
	sent json.RawMessage
	// pending holds each validated initialize, by request id, until its
	// response is queued.
	pending map[string]initializeCommit
}

type promptCancel struct {
	cancel context.CancelFunc
}

func (s *server) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if method == schema.InitializeMethodName {
		return s.initialize(ctx, raw)
	}
	// The request guard has already refused requests before initialization.
	if !s.advertisesMethod(method) {
		return nil, methodNotFound(method)
	}
	ctx = initresult.With(withClient(ctx, s.client), s.sentInitialize())
	if override, ok := s.overrides[method]; ok {
		return override(ctx, method, raw)
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

// initialize negotiates one initialize request like the reference SDK's v1
// agent, which keeps v1's fallback-to-latest rule: every request with a valid
// protocolVersion selects v1, and a request for a newer version is normalized
// to keep what parses under v1. The v1 lifecycle is unchecked, as in the
// reference SDK: every initialize reaches the agent, including a repeat at any
// time and one sent while another is in progress. Other requests are refused
// only until the first initialize succeeds. A successful repeat replaces the
// advertised capabilities; a failed one keeps the previous initialization.
//
// As in the reference SDK, the response always carries protocolVersion 1,
// whatever the agent or the InitializeResult hook set.
func (s *server) initialize(ctx context.Context, raw json.RawMessage) (any, error) {
	params, err := negotiate.V1Params(raw)
	if err != nil {
		return nil, err
	}
	request, err := decodeRequest[schema.InitializeRequest](params)
	if err != nil {
		return nil, err
	}

	response, err := s.agent.Initialize(ctx, s.client, request)
	if err != nil {
		return nil, err
	}
	response.ProtocolVersion = acp.Version
	result, response, err := s.extendInitialize(response)
	if err != nil {
		return nil, err
	}
	sent, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode initialize response: %w", err)
	}
	id, _ := rpcctl.RequestID(ctx)
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string]initializeCommit{}
	}
	s.pending[string(id)] = initializeCommit{clientCapabilities: request.ClientCapabilities, agentCapabilities: response.AgentCapabilities, sent: sent}
	s.mu.Unlock()
	return json.RawMessage(sent), nil
}

// outgoing applies the reference SDK's outgoing_message to the unchecked v1
// agent: a request that already wraps an initialize in proxy/successor is
// refused, and an initialize the agent sends carries protocolVersion 1.
func outgoing(method string, params json.RawMessage, notification bool) (json.RawMessage, error) {
	if notification {
		return nil, nil
	}
	if negotiate.SuccessorEnclosesInitialize(method, params) {
		return nil, negotiate.InvalidPrewrappedInitialize()
	}
	if method == schema.InitializeMethodName {
		return negotiate.OutgoingV1Params(params)
	}
	return nil, nil
}

// initializeCommit is a validated initialize waiting for its response to be
// queued.
type initializeCommit struct {
	clientCapabilities *schema.ClientCapabilities
	agentCapabilities  *schema.AgentCapabilities
	sent               json.RawMessage
}

// outgoingResponse records an initialization when its response is queued, as
// the reference SDK's outgoing_response_to completes it, so nothing the agent
// sends afterwards can precede the response on the wire. Client and agent
// capabilities are recorded in one critical section, so concurrent
// initializations cannot leave the client capabilities of one and the agent
// capabilities of another. A failed initialize keeps the previous one.
func (s *server) outgoingResponse(id json.RawMessage, method string, result json.RawMessage, err error) (json.RawMessage, error) {
	if method != schema.InitializeMethodName {
		return result, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	commit, ok := s.pending[string(id)]
	delete(s.pending, string(id))
	if err != nil || !ok {
		return result, err
	}
	s.client.initialize(commit.clientCapabilities)
	s.initialized = true
	s.agentCapabilities = commit.agentCapabilities
	s.sent = commit.sent
	return result, nil
}

// guard applies the reference SDK's unchecked v1 lifecycle to each request in
// wire order, on the connection's read loop: an initialize request with an
// invalid protocolVersion is refused with -32602 before it reaches the agent,
// and any other request is refused until an initialize has succeeded.
func (s *server) guard(method string, raw json.RawMessage) error {
	if method == schema.InitializeMethodName {
		_, err := negotiate.V1Params(raw)
		return err
	}
	if !s.isInitialized() {
		return negotiate.InitializationRequired(method)
	}
	return nil
}

func (s *server) advertisesMethod(method string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	capabilities := s.agentCapabilities
	if capabilities == nil {
		capabilities = &schema.AgentCapabilities{}
	}
	switch method {
	case schema.SessionLoadMethodName:
		return capabilities.LoadSession != nil && *capabilities.LoadSession
	case schema.SessionResumeMethodName:
		return capabilities.SessionCapabilities != nil && capabilities.SessionCapabilities.Resume != nil
	case schema.SessionCloseMethodName:
		return capabilities.SessionCapabilities != nil && capabilities.SessionCapabilities.Close != nil
	case schema.SessionListMethodName:
		return capabilities.SessionCapabilities != nil && capabilities.SessionCapabilities.List != nil
	case schema.SessionDeleteMethodName:
		return capabilities.SessionCapabilities != nil && capabilities.SessionCapabilities.Delete != nil
	case schema.LogoutMethodName:
		return capabilities.Auth != nil && capabilities.Auth.Logout != nil
	default:
		// Methods without a capability gate, including extensions.
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

// sentInitialize is the encoded initialize response sent on the connection.
func (s *server) sentInitialize() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
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

func methodNotFound(method string) error {
	return &acp.RPCError{Code: -32601, Message: "agent does not implement method: " + method}
}
