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
	// The handler reaches the connection's Client through ClientFromContext.
	Extensions acp.Handler

	// InitializeResult, when set, receives the agent's initialize response
	// and returns the value sent instead, typically the unstable initialize
	// response that advertises extension capabilities (see
	// github.com/BrokkAi/acp-go/v2/unstable.Advertise). The runtime decodes the
	// returned value as a stable v2 initialize response and validates that
	// view before sending the value.
	InitializeResult func(schema.InitializeResponse) (any, error)

	// Overrides, when set, serves the named methods in place of the runtime's
	// own dispatch to Agent, keyed by method name. An override receives the
	// raw params, so it can read and answer fields the stable schema does not
	// carry. It runs behind the initialization gate and the method's
	// capability check (session/list still requires the baseline
	// capabilities.session, for example), and takes precedence over
	// Extensions for methods the runtime does not own. An override for
	// auth/login, auth/logout, or session/delete stands in for the Agent
	// interface those capabilities otherwise require. initialize and
	// session/prompt cannot be overridden, because the runtime itself
	// validates the handshake and the prompt response, and notifications
	// such as session/cancel cannot either, because overrides answer
	// requests; Serve refuses those keys and nil handlers before reading from
	// the connection. An override, like an Extensions handler, reaches the
	// connection's Client through ClientFromContext.
	// github.com/BrokkAi/acp-go/v2/unstable.Install sets the overrides the
	// unstable facades need, such as session/list with the archived filter.
	Overrides map[string]acp.Handler
}

func New(implementation Agent) *Runtime {
	return &Runtime{Agent: implementation}
}

// Serve serves one ACP v2 stdio connection.
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
	connection := acp.Connect(in, out, state.handle, state.notification, acp.WithProtocolVersion(uint16(version)),
		rpcctl.WithRequestGuard(state.guard).(acp.ConnectOption),
		rpcctl.WithNotificationGuard(func(method string) error { return state.allowed(method, true) }).(acp.ConnectOption),
		rpcctl.WithOutgoingGuard(state.outgoing).(acp.ConnectOption),
		rpcctl.WithOutgoingResponseGuard(state.outgoingResponse).(acp.ConnectOption),
		rpcctl.WithIncomingResponseGuard(state.incomingResponse).(acp.ConnectOption))
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

// initState is the reference SDK's v2 responder lifecycle.
type initState int

const (
	uninitialized initState = iota
	initializing
	ready
)

type server struct {
	agent            Agent
	client           *clientConnection
	extensions       acp.Handler
	initializeResult func(schema.InitializeResponse) (any, error)
	overrides        map[string]acp.Handler

	mu    sync.Mutex
	state initState
	// initialized records whether any initialize has completed, so a failed
	// repeat restores the previous initialization.
	initialized bool
	// repeatable records whether a completed initialize may still be
	// repeated. The client may repeat initialize after the previous one
	// completed and before it sends any other request; the first other
	// request closes this window for the rest of the connection.
	repeatable bool
	caps       *schema.AgentCapabilities
	auth       bool
	// sent is the encoded initialize response, which handlers read through
	// initresult.From.
	sent json.RawMessage
	// pending is the initialize whose response is about to be queued.
	pending *initializeCommit
}

func (s *server) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if method == schema.InitializeMethodName {
		return s.initialize(ctx, raw)
	}
	// The request guard has already refused requests before initialization.
	if err := s.gate(method); err != nil {
		return nil, err
	}
	ctx = initresult.With(withClient(ctx, s.client), s.sentInitialize())
	if override, ok := s.overrides[method]; ok {
		return override(ctx, method, raw)
	}

	switch method {
	case schema.AuthLoginMethodName:
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
		request, err := decodeRequest[schema.NewSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		return s.agent.NewSession(ctx, s.client, request)
	case schema.SessionResumeMethodName:
		request, err := decodeRequest[schema.ResumeSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		return s.agent.ResumeSession(ctx, s.client, request)
	case schema.SessionCloseMethodName:
		request, err := decodeRequest[schema.CloseSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		return s.agent.CloseSession(ctx, s.client, request)
	case schema.SessionListMethodName:
		request, err := decodeRequest[schema.ListSessionsRequest](raw)
		if err != nil {
			return nil, err
		}
		return s.agent.ListSessions(ctx, s.client, request)
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

// guard applies the reference SDK's v2 responder lifecycle to each request in
// wire order, on the connection's read loop, as the reference incoming actor
// does. An initialize request is negotiated: a request below v2 gets -32022
// listing v2, a request for a newer version keeps what parses under v2, and a
// request for exactly v2 must carry a valid info. A negotiated initialize then
// begins, which is refused while another is in progress or after another
// request was admitted. Any other request is refused until an initialize has
// completed, including while a repeat is in progress; an admitted one closes
// the repeat window. A refusal leaves the previous initialization in place.
func (s *server) guard(method string, raw json.RawMessage) error {
	if method == schema.InitializeMethodName {
		if _, err := negotiate.V2Params(raw); err != nil {
			return err
		}
		return s.beginInitialize()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != ready {
		return negotiate.InitializationRequired(method)
	}
	s.repeatable = false
	return nil
}

// allowed applies the reference SDK's ensure_initialized and, for a
// notification, ensure_notification_allowed: traffic other than the
// handshake flows only once an initialization has completed, except that
// $/cancel_request may cancel an initialize in progress. It vets incoming
// notifications, which are dropped when refused, and the agent's outgoing
// requests and notifications, which fail without being written.
func (s *server) allowed(method string, notification bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.state == ready:
		return nil
	case notification && s.state == initializing && method == schema.CancelRequestMethodName:
		return nil
	}
	return negotiate.InitializationRequired(method)
}

// outgoing vets a request or notification the agent sends. As in the
// reference SDK's responder, an initialize the agent sends is set to v2 and
// validated like any outgoing initialize, then refused: agents never send it.
func (s *server) outgoing(method string, params json.RawMessage, notification bool) (json.RawMessage, error) {
	if !notification && negotiate.SuccessorEnclosesInitialize(method, params) {
		return nil, negotiate.InvalidPrewrappedInitialize()
	}
	if !notification && method == schema.InitializeMethodName {
		if _, err := negotiate.OutgoingV2Params(params); err != nil {
			return nil, err
		}
		return nil, negotiate.InvalidInitializeDirection()
	}
	return nil, s.allowed(method, notification)
}

// outgoingResponse completes or fails an initialize when its response is
// queued, as the reference SDK's outgoing_response_to does, so nothing the
// agent sends afterwards can precede it on the wire. Any other successful
// response is refused while no initialization is complete, including during
// a repeat (ensure_initialized).
func (s *server) outgoingResponse(_ json.RawMessage, method string, result json.RawMessage, err error) (json.RawMessage, error) {
	if method == schema.InitializeMethodName {
		s.mu.Lock()
		defer s.mu.Unlock()
		commit := s.pending
		s.pending = nil
		if err != nil || commit == nil {
			if s.initialized {
				s.state = ready
				s.repeatable = true
			} else {
				s.state = uninitialized
			}
			if err == nil {
				err = negotiate.InvalidRequest("received an initialize response before an initialize request was received")
			}
			return nil, err
		}
		s.client.initialize(commit.clientCapabilities)
		s.state = ready
		s.initialized = true
		s.repeatable = true
		s.caps = commit.caps
		s.auth = commit.auth
		s.sent = commit.sent
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	if err := s.allowed(method, false); err != nil {
		return nil, err
	}
	return result, nil
}

// incomingResponse refuses a successful response to the agent's own request
// that arrives while no initialization is complete, as the reference SDK's
// incoming_response does.
func (s *server) incomingResponse(method string, _ bool, result json.RawMessage, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	if err := s.allowed(method, false); err != nil {
		return nil, err
	}
	return result, nil
}

// initializeCommit is a validated initialize waiting for its response to be
// queued.
type initializeCommit struct {
	clientCapabilities *schema.ClientCapabilities
	caps               *schema.AgentCapabilities
	auth               bool
	sent               json.RawMessage
}

// initialize runs an initialize request the guard has begun and validates the
// agent's response. The initialization completes, or fails and restores the
// previous state, when the response is queued (outgoingResponse). As in the
// reference SDK, the response always carries protocolVersion 2, whatever the
// agent or the InitializeResult hook set.
func (s *server) initialize(ctx context.Context, raw json.RawMessage) (any, error) {
	params, err := negotiate.V2Params(raw)
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
	response.ProtocolVersion = version
	result, response, err := s.extendInitialize(response)
	if err != nil {
		return nil, err
	}
	if response.Capabilities == nil || response.Capabilities.Session == nil {
		return nil, errors.New("v2 agent must advertise baseline session capabilities")
	}
	if len(response.AuthMethods) > 0 {
		_, canLogin := s.agent.(AuthLoginer)
		_, canLogout := s.agent.(AuthLogouter)
		canLogin = canLogin || s.overrides[schema.AuthLoginMethodName] != nil
		canLogout = canLogout || s.overrides[schema.AuthLogoutMethodName] != nil
		if !canLogin || !canLogout {
			return nil, errors.New("authMethods requires both auth/login and auth/logout implementations")
		}
	}
	if response.Capabilities.Session.Delete != nil {
		if _, ok := s.agent.(SessionDeleter); !ok && s.overrides[schema.SessionDeleteMethodName] == nil {
			return nil, errors.New("session.delete capability requires a SessionDeleter implementation")
		}
	}
	sent, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode initialize response: %w", err)
	}
	s.mu.Lock()
	s.pending = &initializeCommit{
		clientCapabilities: request.Capabilities,
		caps:               response.Capabilities,
		auth:               len(response.AuthMethods) > 0,
		sent:               sent,
	}
	s.mu.Unlock()
	return json.RawMessage(sent), nil
}

// beginInitialize starts a first or repeated initialization, refusing one
// while another is in progress or after another request has been admitted.
func (s *server) beginInitialize() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.state == initializing:
		return negotiate.InvalidRequest("ACP initialization is already in progress on this connection")
	case s.state == ready && !s.repeatable:
		return negotiate.InvalidRequest("ACP initialization cannot be repeated after any other request; reconnect to initialize again")
	}
	s.state = initializing
	s.repeatable = false
	return nil
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

// sentInitialize is the encoded initialize response sent on the connection.
func (s *server) sentInitialize() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sent
}

// gate applies the capability check of a method the runtime owns, before
// either its own dispatch or an override serves it.
func (s *server) gate(method string) error {
	switch method {
	case schema.AuthLoginMethodName, schema.AuthLogoutMethodName:
		if !s.advertisesAuth() {
			return methodNotFound(method)
		}
	case schema.SessionNewMethodName, schema.SessionResumeMethodName, schema.SessionCloseMethodName,
		schema.SessionListMethodName, schema.SessionPromptMethodName:
		return s.requireSessions()
	case schema.SessionDeleteMethodName:
		if !s.advertisesDelete() {
			return methodNotFound(method)
		}
	}
	return nil
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
