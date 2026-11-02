package mcp

import (
	"context"
	"encoding/json"

	"github.com/BrokkAi/acp-go/internal/mcpserver"
)

// The server framework is independent of the ACP version: these names are
// the same types in this package and in github.com/BrokkAi/acp-go/mcp (ACP
// v1), so a Server defined once attaches to v1 and v2 sessions alike.
type (
	// Server is an MCP server: a declaration name and the Service that
	// executes its operations. Serve runs it standalone; Attach,
	// NewAttachedSession and the other attached-session helpers expose it to
	// an agent over ACP.
	Server = mcpserver.Server
	// Service executes one MCP operation per call. See mcpserver.Service for
	// the cancellation and cleanup contract.
	Service = mcpserver.Service
	// ServiceFunc adapts a function to Service.
	ServiceFunc = mcpserver.ServiceFunc
	// Request is one MCP invocation: the inner method and raw params.
	Request = mcpserver.Request
	// Outcome is an MCP result or inner MCP error.
	Outcome = mcpserver.Outcome
	// Error is an inner MCP error object, never an outer ACP error.
	Error = mcpserver.Error
	// RequestContext carries one operation's server ID, logical request ID,
	// validated metadata, cancellation, and notification rights.
	RequestContext = mcpserver.RequestContext
	// Tool describes an MCP tool with caller-supplied JSON Schemas.
	Tool = mcpserver.Tool
	// ToolHandler runs one tools/call.
	ToolHandler = mcpserver.ToolHandler
	// ToolResult is an explicit tools/call result.
	ToolResult = mcpserver.ToolResult
	// ToolRegistry holds tools, allow/deny filtering, and instructions.
	ToolRegistry = mcpserver.ToolRegistry
	// UnknownToolError reports enabling or disabling an unregistered tool.
	UnknownToolError = mcpserver.UnknownToolError
)

// ProtocolVersion is the MCP revision the native binding carries.
const ProtocolVersion = mcpserver.ProtocolVersion

// DefaultServerVersion is the serverInfo version of a tool server built
// without one.
const DefaultServerVersion = mcpserver.DefaultServerVersion

// Outer ACP error codes of the MCP-over-ACP binding.
const (
	// ErrorCodeResourceLimitExceeded (-33000): a binding resource limit, such
	// as MessageRouter.SetMaxActive, was exceeded.
	ErrorCodeResourceLimitExceeded = mcpserver.ErrorCodeResourceLimitExceeded
	// ErrorCodeServerUnavailable (-33001): the addressed server registration
	// does not exist or was removed.
	ErrorCodeServerUnavailable = mcpserver.ErrorCodeServerUnavailable
	// ErrorCodeBackendFailure (-33002): the backend failed without a valid MCP
	// outcome.
	ErrorCodeBackendFailure = mcpserver.ErrorCodeBackendFailure
)

// Causes reported by context.Cause on an operation's context.
var (
	ErrRequestCancelled  = mcpserver.ErrRequestCancelled
	ErrServerUnavailable = mcpserver.ErrServerUnavailable
	ErrConnectionClosed  = mcpserver.ErrConnectionClosed
)

// NewServer returns a server with the given declaration name and service.
func NewServer(name string, service Service) *Server { return mcpserver.NewServer(name, service) }

// NewToolRegistry returns an empty registry with every tool enabled.
func NewToolRegistry() *ToolRegistry { return mcpserver.NewToolRegistry() }

// NewToolServer returns a server that answers server/discover, tools/list,
// and tools/call from registry and reports name and version (default
// DefaultServerVersion) as its serverInfo.
func NewToolServer(name, version string, registry *ToolRegistry) *Server {
	return mcpserver.NewToolServer(name, version, registry)
}

// NewTool returns a tool whose arguments decode into In and whose result is
// encoded from Out. Arguments that do not match inputSchema or do not decode
// become an isError result.
func NewTool[In, Out any](name, description string, inputSchema json.RawMessage, fn func(ctx context.Context, input In, rc *RequestContext) (Out, error)) Tool {
	return mcpserver.NewTool(name, description, inputSchema, fn)
}

// ResultOutcome encodes value as a successful MCP result.
func ResultOutcome(value any) (Outcome, error) { return mcpserver.ResultOutcome(value) }

// ErrorOutcome returns an inner MCP error outcome.
func ErrorOutcome(code int, message string, data any) Outcome {
	return mcpserver.ErrorOutcome(code, message, data)
}

// TextContent returns an MCP text content block.
func TextContent(text string) map[string]any { return mcpserver.TextContent(text) }
