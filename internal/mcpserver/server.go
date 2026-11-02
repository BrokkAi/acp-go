// Package mcpserver is the version-independent core of the MCP server
// framework that github.com/BrokkAi/acp-go/mcp (ACP v1) and
// github.com/BrokkAi/acp-go/v2/mcp (draft ACP v2) re-export.
//
// It holds what does not depend on the ACP version: services, request
// contexts, the typed tool registry, standalone serving, and the
// request-scoped mcp/message dispatch engine. The public packages supply the
// version-specific carrier types through a Codec and own session attachment.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ProtocolVersion is the only MCP revision the native MCP-over-ACP binding
// carries.
const ProtocolVersion = "2026-07-28"

// LatestLegacyProtocolVersion is the newest MCP revision that still uses the
// initialize handshake. Standalone serving answers initialize with it when the
// client asks for a revision this package does not know.
const LatestLegacyProtocolVersion = "2025-11-25"

// knownProtocolVersions lists the MCP revisions the tool server understands,
// oldest first.
var knownProtocolVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", LatestLegacyProtocolVersion, ProtocolVersion}

// DefaultServerVersion is the serverInfo version a tool server reports when
// none is given, matching the reference SDK.
const DefaultServerVersion = "0.0.0"

// Outer ACP error codes of the MCP-over-ACP binding.
const (
	// ErrorCodeResourceLimitExceeded reports that a binding resource limit,
	// such as the active-operation limit, was exceeded.
	ErrorCodeResourceLimitExceeded = -33000
	// ErrorCodeServerUnavailable reports that the addressed server
	// registration does not exist or was removed.
	ErrorCodeServerUnavailable = -33001
	// ErrorCodeBackendFailure reports that the backend failed without a valid
	// MCP outcome.
	ErrorCodeBackendFailure = -33002
)

// Inner MCP error codes used by the framework.
const (
	codeMethodNotFound             = -32601
	codeInvalidParams              = -32602
	codeInternalError              = -32603
	CodeUnsupportedProtocolVersion = -32022
)

// Metadata keys MCP 2026-07-28 carries in every request's params._meta.
const (
	MetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	MetaServerInfo         = "io.modelcontextprotocol/serverInfo"
)

// Reasons an operation was cancelled. Use context.Cause on the context passed
// to Service.Execute to tell them apart.
var (
	// ErrRequestCancelled means the caller cancelled the outer ACP request.
	ErrRequestCancelled = errors.New("mcp: request cancelled")
	// ErrServerUnavailable means the server registration was removed.
	ErrServerUnavailable = errors.New("mcp: server registration removed")
	// ErrConnectionClosed means the ACP connection or its input closed.
	ErrConnectionClosed = errors.New("mcp: connection closed")
)

// Error is an inner MCP JSON-RPC error object. It travels inside the
// mcp/message response carrier and is never an outer ACP error.
//
// Data distinguishes omission (nil) from an explicit JSON null ("null").
type Error struct {
	Code    int
	Message string
	Data    json.RawMessage
}

func (e *Error) Error() string { return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message) }

// MarshalJSON encodes the error object, omitting data only when Data is nil.
func (e Error) MarshalJSON() ([]byte, error) {
	type wire struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data,omitempty"`
	}
	return json.Marshal(wire(e))
}

func newError(code int, message string, data any) *Error {
	err := &Error{Code: code, Message: message}
	if data != nil {
		if raw, encodeErr := json.Marshal(data); encodeErr == nil {
			err.Data = raw
		}
	}
	return err
}

func unsupportedVersion(requested any, supported []string) *Error {
	return newError(CodeUnsupportedProtocolVersion, "Unsupported protocol version", map[string]any{
		"requested": requested,
		"supported": supported,
	})
}

// Request is one MCP invocation against a Service. There is no implicit
// initialize or discovery exchange before it.
type Request struct {
	// Method is the inner MCP method, such as tools/call.
	Method string
	// Params is the inner params object, including _meta, exactly as received.
	// It is nil when the request carried no params or null.
	Params json.RawMessage
}

// Outcome is an MCP outcome, distinct from a failure of the ACP binding.
// Exactly one of Result and Error is meaningful: when Error is nil, Result is
// the opaque MCP result, and a nil Result is sent as JSON null.
type Outcome struct {
	Result json.RawMessage
	Error  *Error
}

// IsError reports whether the outcome carries an inner MCP error.
func (o Outcome) IsError() bool { return o.Error != nil }

// ResultOutcome encodes value as a successful MCP result.
func ResultOutcome(value any) (Outcome, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Result: raw}, nil
}

// ErrorOutcome returns an inner MCP error outcome.
func ErrorOutcome(code int, message string, data any) Outcome {
	return Outcome{Error: newError(code, message, data)}
}

// Service is reusable MCP application state. Execute runs one operation.
//
// ctx is the operation context: it is cancelled when the caller cancels the
// outer ACP request, when the server registration is removed, or when the ACP
// connection closes, and context.Cause reports which. Execute must stop its
// work and finish owned cleanup before returning; the binding waits for it and
// holds the logical request ID until then.
//
// A returned error is a binding failure, answered with
// ErrorCodeBackendFailure. Report MCP errors through Outcome.Error instead.
type Service interface {
	Execute(ctx context.Context, request Request, rc *RequestContext) (Outcome, error)
}

// ServiceFunc adapts a function to Service.
type ServiceFunc func(ctx context.Context, request Request, rc *RequestContext) (Outcome, error)

// Execute implements Service.
func (f ServiceFunc) Execute(ctx context.Context, request Request, rc *RequestContext) (Outcome, error) {
	return f(ctx, request, rc)
}

// RequestContext is the authority of a single operation, not of the service.
// It must not be used to infer the identity or capabilities of another
// request.
type RequestContext struct {
	standalone    bool
	serverID      string
	requestID     string
	meta          json.RawMessage
	requestDone   <-chan struct{}
	operationDone <-chan struct{}
	notify        func(ctx context.Context, method string, params json.RawMessage) error
}

// Standalone reports whether the operation arrived over a direct MCP
// connection rather than an ACP mcp/message request.
func (c *RequestContext) Standalone() bool { return c.standalone }

// ServerID is the serverId the operation was addressed to, or "" when
// standalone.
func (c *RequestContext) ServerID() string { return c.serverID }

// RequestID is the logical MCP request ID of the operation, or "" when
// standalone. It stays reserved until the operation's cleanup finishes.
func (c *RequestContext) RequestID() string { return c.requestID }

// Metadata returns a fresh copy of the request's params._meta object, or nil
// when it had none. Over ACP it has passed validation: it carries the
// supported protocol version and a client-capabilities object. Numbers decode
// as json.Number.
func (c *RequestContext) Metadata() map[string]any {
	if len(c.meta) == 0 {
		return nil
	}
	var meta map[string]any
	if decodeNumbers(c.meta, &meta) != nil {
		return nil
	}
	return meta
}

// ProtocolVersion returns the MCP protocol version the request declared in
// its metadata, or "".
func (c *RequestContext) ProtocolVersion() string {
	version, _ := c.Metadata()[MetaProtocolVersion].(string)
	return version
}

// ClientCapabilities returns the MCP client capabilities the request declared
// in its metadata, or nil.
func (c *RequestContext) ClientCapabilities() map[string]any {
	capabilities, _ := c.Metadata()[MetaClientCapabilities].(map[string]any)
	return capabilities
}

// RequestCancelled is closed when the outer request is cancelled by its
// caller or its connection stops.
func (c *RequestContext) RequestCancelled() <-chan struct{} { return c.requestDone }

// OperationCancelled is closed when the operation loses its authority for
// any reason: outer cancellation, registration removal, or connection close.
// It is the Done channel of the context passed to Execute.
func (c *RequestContext) OperationCancelled() <-chan struct{} { return c.operationDone }

// Notify sends an operation-scoped MCP notification, such as
// notifications/progress. Over ACP it travels as an mcp/message notification
// carrying this operation's server and request IDs; standalone it is sent
// directly. params must encode to a JSON object or null. Notify fails once
// the operation is cancelled or has returned, so late notifications are never
// delivered.
func (c *RequestContext) Notify(ctx context.Context, method string, params any) error {
	if method == "" {
		return errors.New("mcp notification requires an inner MCP method")
	}
	var raw json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return err
		}
		if string(encoded) != "null" {
			if len(encoded) == 0 || encoded[0] != '{' {
				return errors.New("mcp notification params must be a JSON object")
			}
			raw = encoded
		}
	}
	return c.notify(ctx, method, raw)
}

// Server is an MCP server: a name used in ACP declarations and the Service
// that executes its operations. The same Server can be served standalone and
// attached to any number of ACP v1 or v2 sessions; each attachment gets its
// own server ID.
type Server struct {
	name    string
	service Service
}

// NewServer returns a server with the given declaration name and service.
func NewServer(name string, service Service) *Server {
	return &Server{name: name, service: service}
}

// Name is the name advertised in ACP declarations.
func (s *Server) Name() string { return s.name }

// Service returns the server's service.
func (s *Server) Service() Service { return s.service }

// Validate reports whether the server can be attached or served.
func (s *Server) Validate() error {
	switch {
	case s == nil:
		return errors.New("mcp server is required")
	case s.name == "":
		return errors.New("mcp server requires a name")
	case s.service == nil:
		return fmt.Errorf("mcp server %q requires a service", s.name)
	}
	return nil
}

// operationGate serializes notifications with the end of an operation, so no
// notification is written after the operation stopped.
type operationGate struct {
	mu   sync.Mutex
	done bool
}

func (g *operationGate) close() {
	g.mu.Lock()
	g.done = true
	g.mu.Unlock()
}

func decodeNumbers(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(target)
}
