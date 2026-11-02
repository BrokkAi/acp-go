package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/BrokkAi/acp-go"
)

// Connection is the part of an ACP connection the binding uses. Both
// *acp.Connection and the draft v2 connection satisfy it.
type Connection interface {
	Notify(ctx context.Context, method string, params any) error
	Done() <-chan struct{}
	InputClosed() <-chan struct{}
}

// Envelope is a decoded agent-to-provider mcp/message request.
type Envelope struct {
	ServerID  string
	RequestID string
	Method    string
	// Params is the raw inner params object, nil when absent or null.
	Params json.RawMessage
}

// Codec adapts the binding to one ACP version's generated carrier types.
type Codec struct {
	// Method is the outer ACP method, mcp/message.
	Method string
	// Decode parses an mcp/message request envelope.
	Decode func(json.RawMessage) (Envelope, error)
	// Response builds the version's response carrier for an outcome.
	Response func(Outcome) (any, error)
	// Notification builds the version's request-scoped notification.
	Notification func(serverID, requestID, method string, params json.RawMessage) (any, error)
}

// Binding is the provider side of the request-scoped MCP-over-ACP binding for
// one ACP connection: a registry of server IDs and the dispatcher for their
// mcp/message requests.
type Binding struct {
	connection Connection
	codec      Codec

	mu            sync.Mutex
	registrations map[string]*Registration
	closing       map[string]*Registration
	retired       map[string]struct{}
	active        int
	maxActive     int
}

// NewBinding returns a binding that sends notifications over connection, which
// may be nil when no notifications are needed.
func NewBinding(connection Connection, codec Codec) *Binding {
	return &Binding{
		connection:    connection,
		codec:         codec,
		registrations: map[string]*Registration{},
		closing:       map[string]*Registration{},
		retired:       map[string]struct{}{},
	}
}

// SetMaxActive limits the number of operations active at once across every
// registration; further requests are refused with
// ErrorCodeResourceLimitExceeded. Zero or a negative value removes the limit.
func (b *Binding) SetMaxActive(limit int) {
	b.mu.Lock()
	b.maxActive = limit
	b.mu.Unlock()
}

// Registration is one server ID bound to a service.
type Registration struct {
	binding  *Binding
	serverID string
	service  Service
	native   bool

	// Guarded by binding.mu.
	closed     bool
	operations map[string]*operation
	idle       chan struct{}
}

// ServerID returns the registration's server ID.
func (r *Registration) ServerID() string { return r.serverID }

// Register binds serverID to service. A native registration validates the
// MCP 2026-07-28 request metadata and restricts discovery to that revision;
// a raw one leaves the MCP protocol to the service. A server ID is never
// rebound, even after its registration was removed.
func (b *Binding) Register(serverID string, service Service, native bool) (*Registration, error) {
	if strings.TrimSpace(serverID) == "" {
		return nil, errors.New("mcp server ID is required")
	}
	if service == nil {
		return nil, errors.New("mcp service is required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.registrations[serverID]; exists {
		return nil, fmt.Errorf("mcp server %q is already registered", serverID)
	}
	if _, retired := b.retired[serverID]; retired {
		return nil, fmt.Errorf("mcp server ID %q was already used on this connection", serverID)
	}
	return b.registerLocked(serverID, service, native), nil
}

// RegisterNew binds a freshly generated, connection-unique server ID.
func (b *Binding) RegisterNew(service Service) (*Registration, error) {
	if service == nil {
		return nil, errors.New("mcp service is required")
	}
	for {
		serverID, err := newServerID()
		if err != nil {
			return nil, err
		}
		b.mu.Lock()
		_, exists := b.registrations[serverID]
		_, retired := b.retired[serverID]
		if !exists && !retired {
			registration := b.registerLocked(serverID, service, true)
			b.mu.Unlock()
			return registration, nil
		}
		b.mu.Unlock()
	}
}

func (b *Binding) registerLocked(serverID string, service Service, native bool) *Registration {
	registration := &Registration{
		binding:    b,
		serverID:   serverID,
		service:    service,
		native:     native,
		operations: map[string]*operation{},
		idle:       make(chan struct{}),
	}
	b.registrations[serverID] = registration
	return registration
}

// newServerID returns "mcp-server:" followed by a random UUID, the reference
// SDK's format.
func newServerID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate MCP server ID: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("mcp-server:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Unregister closes the registration for serverID, if any, without waiting.
func (b *Binding) Unregister(serverID string) {
	b.mu.Lock()
	registration := b.registrations[serverID]
	b.mu.Unlock()
	if registration != nil {
		registration.Close()
	}
}

// UnregisterAndWait closes the registration for serverID and waits until
// every operation it admitted has returned or ctx is done. It also waits for
// a registration that an earlier Unregister closed.
func (b *Binding) UnregisterAndWait(ctx context.Context, serverID string) error {
	b.mu.Lock()
	registration := b.registrations[serverID]
	if registration == nil {
		registration = b.closing[serverID]
	}
	b.mu.Unlock()
	if registration == nil {
		return nil
	}
	registration.Close()
	return registration.Wait(ctx)
}

// Lookup returns the live registration for serverID.
func (b *Binding) Lookup(serverID string) *Registration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.registrations[serverID]
}

// Close revokes admission for the registration and cancels its active
// operations with ErrServerUnavailable. It returns once no notification of
// those operations is being written; it does not wait for them to return.
func (r *Registration) Close() {
	b := r.binding
	b.mu.Lock()
	if r.closed {
		b.mu.Unlock()
		return
	}
	r.closed = true
	if b.registrations[r.serverID] == r {
		delete(b.registrations, r.serverID)
	}
	if len(r.operations) > 0 {
		b.closing[r.serverID] = r
	}
	b.retired[r.serverID] = struct{}{}
	operations := make([]*operation, 0, len(r.operations))
	for _, op := range r.operations {
		operations = append(operations, op)
	}
	r.signalIdleLocked()
	b.mu.Unlock()
	for _, op := range operations {
		op.stop(ErrServerUnavailable)
	}
}

// Wait blocks until the registration is closed and every operation it
// admitted has returned, or until ctx is done.
func (r *Registration) Wait(ctx context.Context) error {
	select {
	case <-r.idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Registration) signalIdleLocked() {
	if r.closed && len(r.operations) == 0 {
		if r.binding.closing[r.serverID] == r {
			delete(r.binding.closing, r.serverID)
		}
		select {
		case <-r.idle:
		default:
			close(r.idle)
		}
	}
}

type operation struct {
	registration *Registration
	requestID    string
	cancel       context.CancelCauseFunc
	gate         operationGate
}

// stop cancels the operation, then waits for an in-flight notification to
// finish so none is written afterwards.
func (o *operation) stop(cause error) {
	o.cancel(cause)
	o.gate.close()
}

func rpcError(code int, message string, data any) *acp.RPCError {
	err := &acp.RPCError{Code: code, Message: message}
	if data != nil {
		if raw, encodeErr := json.Marshal(data); encodeErr == nil {
			err.Data = raw
		}
	}
	return err
}

func backendFailure(detail string) *acp.RPCError {
	return rpcError(ErrorCodeBackendFailure, "MCP backend failure", detail)
}

func providerRemoved() *acp.RPCError {
	return rpcError(ErrorCodeServerUnavailable, "MCP provider removed", nil)
}

func cancelled() *acp.RPCError { return &acp.RPCError{Code: -32800, Message: "request cancelled"} }

// Handle serves one mcp/message request. The returned value is the version's
// response carrier; binding failures are returned as *acp.RPCError.
func (b *Binding) Handle(ctx context.Context, raw json.RawMessage) (any, error) {
	envelope, err := b.codec.Decode(raw)
	if err != nil {
		return nil, rpcError(-32602, "malformed mcp/message envelope: "+err.Error(), nil)
	}
	if strings.TrimSpace(envelope.ServerID) == "" || strings.TrimSpace(envelope.RequestID) == "" || strings.TrimSpace(envelope.Method) == "" {
		return nil, rpcError(-32602, "mcp/message requires serverId, requestId, and method", nil)
	}
	registration := b.Lookup(envelope.ServerID)
	if registration == nil {
		return nil, rpcError(ErrorCodeServerUnavailable, fmt.Sprintf("mcp server %q is not registered", envelope.ServerID), nil)
	}
	var meta json.RawMessage
	if registration.native {
		var invalid *Error
		meta, invalid = validateNativeRequest(envelope.Method, envelope.Params)
		if invalid != nil {
			return b.response(Outcome{Error: invalid})
		}
	} else {
		meta = metaOf(envelope.Params)
	}

	opCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancel(nil)
	op := &operation{registration: registration, requestID: envelope.RequestID, cancel: cancel}
	if refusal := b.admit(registration, op); refusal != nil {
		return nil, refusal
	}
	defer b.release(op)

	go b.watch(ctx, opCtx, op)

	rc := &RequestContext{
		serverID:      envelope.ServerID,
		requestID:     envelope.RequestID,
		meta:          meta,
		requestDone:   ctx.Done(),
		operationDone: opCtx.Done(),
		notify: func(notifyCtx context.Context, method string, params json.RawMessage) error {
			return b.notify(notifyCtx, opCtx, op, method, params)
		},
	}
	outcome, err := registration.service.Execute(opCtx, Request{Method: envelope.Method, Params: envelope.Params}, rc)
	// Revoke notification rights before the response can be written.
	op.gate.close()
	if opCtx.Err() != nil {
		// Cancellation wins a race with completion: the result is discarded.
		switch cause := context.Cause(opCtx); {
		case errors.Is(cause, ErrServerUnavailable):
			return nil, providerRemoved()
		default:
			return nil, cancelled()
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, cancelled()
		}
		return nil, backendFailure(err.Error())
	}
	if registration.native && envelope.Method == "server/discover" {
		outcome, err = projectDiscovery(outcome)
		if err != nil {
			return nil, backendFailure(err.Error())
		}
	}
	return b.response(outcome)
}

func (b *Binding) response(outcome Outcome) (any, error) {
	response, err := b.codec.Response(outcome)
	if err != nil {
		return nil, backendFailure(err.Error())
	}
	return response, nil
}

func (b *Binding) admit(registration *Registration, op *operation) *acp.RPCError {
	b.mu.Lock()
	defer b.mu.Unlock()
	if registration.closed {
		return providerRemoved()
	}
	if _, duplicate := registration.operations[op.requestID]; duplicate {
		return rpcError(-32602, fmt.Sprintf("mcp/message request %q for server %q is already active", op.requestID, registration.serverID), nil)
	}
	if b.maxActive > 0 && b.active >= b.maxActive {
		return rpcError(ErrorCodeResourceLimitExceeded, "MCP binding resource limit exceeded", fmt.Sprintf("at most %d MCP operations may be active", b.maxActive))
	}
	registration.operations[op.requestID] = op
	b.active++
	return nil
}

// release frees the logical request ID once the service has returned.
func (b *Binding) release(op *operation) {
	b.mu.Lock()
	defer b.mu.Unlock()
	registration := op.registration
	if registration.operations[op.requestID] == op {
		delete(registration.operations, op.requestID)
		b.active--
	}
	registration.signalIdleLocked()
}

// watch turns outer cancellation and input EOF into operation cancellation.
// Registration removal cancels through Registration.Close.
func (b *Binding) watch(requestCtx, opCtx context.Context, op *operation) {
	var inputClosed, connectionDone <-chan struct{}
	if b.connection != nil {
		inputClosed = b.connection.InputClosed()
		connectionDone = b.connection.Done()
	}
	select {
	case <-opCtx.Done():
	case <-inputClosed:
		op.stop(ErrConnectionClosed)
	case <-requestCtx.Done():
		select {
		case <-connectionDone:
			op.stop(ErrConnectionClosed)
		default:
			op.stop(ErrRequestCancelled)
		}
	}
}

func (b *Binding) notify(ctx, opCtx context.Context, op *operation, method string, params json.RawMessage) error {
	op.gate.mu.Lock()
	defer op.gate.mu.Unlock()
	if op.gate.done || opCtx.Err() != nil {
		err := fmt.Errorf("mcp/message request %q for server %q is no longer active", op.requestID, op.registration.serverID)
		if cause := context.Cause(opCtx); cause != nil {
			err = fmt.Errorf("%w: %w", err, cause)
		}
		return err
	}
	if b.connection == nil {
		return errors.New("mcp binding has no connection for notifications")
	}
	notification, err := b.codec.Notification(op.registration.serverID, op.requestID, method, params)
	if err != nil {
		return err
	}
	return b.connection.Notify(ctx, b.codec.Method, notification)
}

func metaOf(params json.RawMessage) json.RawMessage {
	if len(params) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(params, &fields) != nil {
		return nil
	}
	meta := fields["_meta"]
	if len(meta) == 0 || meta[0] != '{' {
		return nil
	}
	return meta
}

// validateNativeRequest applies the binding's per-request MCP checks and
// returns the validated metadata. Failures are inner MCP errors.
func validateNativeRequest(method string, params json.RawMessage) (json.RawMessage, *Error) {
	if method == "initialize" {
		return nil, methodNotFound("native MCP requests do not use initialize")
	}
	meta := metaOf(params)
	if meta == nil {
		return nil, invalidParams("inner params._meta must be an object")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(meta, &fields) != nil {
		return nil, invalidParams("inner params._meta must be an object")
	}
	var version string
	if raw, ok := fields[MetaProtocolVersion]; !ok || json.Unmarshal(raw, &version) != nil {
		return nil, invalidParams("inner params._meta requires " + MetaProtocolVersion)
	}
	if version != ProtocolVersion {
		return nil, unsupportedVersion(version, []string{ProtocolVersion})
	}
	if raw := fields[MetaClientCapabilities]; len(raw) == 0 || raw[0] != '{' {
		return nil, invalidParams("inner params._meta requires " + MetaClientCapabilities + " object")
	}
	return meta, nil
}

// projectDiscovery restricts a discovery result's supportedVersions to the
// revision this binding carries and preserves every other field.
func projectDiscovery(outcome Outcome) (Outcome, error) {
	if outcome.Error != nil {
		return outcome, nil
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(outcome.Result, &result); err != nil || result == nil {
		return Outcome{}, errors.New("invalid MCP discovery supportedVersions")
	}
	var versions []string
	if err := json.Unmarshal(result["supportedVersions"], &versions); err != nil || versions == nil {
		return Outcome{}, errors.New("invalid MCP discovery supportedVersions")
	}
	supported := false
	for _, version := range versions {
		supported = supported || version == ProtocolVersion
	}
	if !supported {
		return Outcome{Error: newError(CodeUnsupportedProtocolVersion, "Unsupported protocol version", map[string]any{
			"requested": ProtocolVersion,
			"supported": versions,
		})}, nil
	}
	result["supportedVersions"] = json.RawMessage(`["` + ProtocolVersion + `"]`)
	encoded, err := json.Marshal(result)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Result: encoded}, nil
}
