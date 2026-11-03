package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/mcpserver"
	"github.com/BrokkAi/acp-go/internal/rpcparams"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

// Attachment owns the registrations of servers attached to a router: each has
// a generated server ID and serves mcp/message requests until Close.
//
// The attached-session helpers return one per session; close it when the
// session ends. For a proxy, a single Attachment can be declared in every
// session setup request that passes through HandleSessionSetup, and closed
// with the connection.
type Attachment struct {
	router  *MessageRouter
	entries []attachedServer

	mu     sync.Mutex
	closed bool
}

type attachedServer struct {
	server       *Server
	registration *mcpserver.Registration
}

// Attach registers servers under freshly generated, connection-unique server
// IDs. The registrations are live before Attach returns, because an agent may
// call a declared server before it answers the session setup request.
// Attaching fails without registering anything when a server is invalid.
func (r *MessageRouter) Attach(servers ...*Server) (*Attachment, error) {
	if len(servers) == 0 {
		return nil, errors.New("attach requires at least one MCP server")
	}
	for _, server := range servers {
		if err := server.Validate(); err != nil {
			return nil, err
		}
	}
	attachment := &Attachment{router: r}
	for _, server := range servers {
		registration, err := r.binding.RegisterNew(server.Service())
		if err != nil {
			attachment.release(context.Background())
			return nil, err
		}
		attachment.entries = append(attachment.entries, attachedServer{server: server, registration: registration})
	}
	return attachment, nil
}

// ServerIDs returns the generated server IDs, in attachment order.
func (a *Attachment) ServerIDs() []string {
	ids := make([]string, len(a.entries))
	for i, entry := range a.entries {
		ids[i] = entry.registration.ServerID()
	}
	return ids
}

// Declarations returns the native MCP-over-ACP declarations to place in a
// session setup request's mcpServers.
func (a *Attachment) Declarations() []schema.McpServer {
	servers := make([]schema.McpServer, len(a.entries))
	for i, entry := range a.entries {
		servers[i] = schema.McpServer{ACP: &schema.McpServerAcp{
			Name:     entry.server.Name(),
			ServerID: schema.McpServerAcpId(entry.registration.ServerID()),
		}}
	}
	return servers
}

// Close releases the registrations: later requests for them are refused with
// -33001, active operations are cancelled (and answered with -33001), and
// Close waits until every one of them has returned or ctx is done. Closing
// again only waits.
func (a *Attachment) Close(ctx context.Context) error {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()
	return a.release(ctx)
}

func (a *Attachment) release(ctx context.Context) error {
	for _, entry := range a.entries {
		entry.registration.Close()
	}
	var err error
	for _, entry := range a.entries {
		if waitErr := entry.registration.Wait(ctx); waitErr != nil && err == nil {
			err = waitErr
		}
	}
	return err
}

func (a *Attachment) isClosed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed
}

// isSessionSetup reports whether method declares MCP servers in ACP v1.
func isSessionSetup(method string) bool {
	switch method {
	case schema.SessionNewMethodName, schema.SessionLoadMethodName, schema.SessionResumeMethodName, schema.SessionForkMethodName:
		return true
	}
	return false
}

// AppendDeclarations appends the attachment's declarations to the mcpServers
// of a raw session/new, session/load, session/resume, or session/fork params
// object and returns the new params. Every other member keeps its value, so
// fields the generated types do not model survive; the result is semantically
// equal, not byte-identical (members may be re-ordered). Other methods, and every
// method after Close, return params unchanged.
func (a *Attachment) AppendDeclarations(method string, params json.RawMessage) (json.RawMessage, error) {
	if !isSessionSetup(method) || a.isClosed() {
		return params, nil
	}
	if err := validateSessionSetup(method, params); err != nil {
		return nil, err
	}
	declarations, err := a.encodedDeclarations()
	if err != nil {
		return nil, err
	}
	return mcpserver.AppendDeclarations(params, declarations, false)
}

// validateSessionSetup decodes the params of a session setup request as the
// reference SDK's validate_session_setup does before it declares a server:
// params that do not decode as the method's request answer json_cast_params'
// -32602 and are not forwarded.
func validateSessionSetup(method string, params json.RawMessage) error {
	var err error
	switch method {
	case schema.SessionNewMethodName:
		_, err = rpcparams.Decode[schema.NewSessionRequest](params)
	case schema.SessionLoadMethodName:
		_, err = rpcparams.Decode[schema.LoadSessionRequest](params)
	case schema.SessionResumeMethodName:
		_, err = rpcparams.Decode[schema.ResumeSessionRequest](params)
	case schema.SessionForkMethodName:
		_, err = rpcparams.Decode[schema.ForkSessionRequest](params)
	}
	return err
}

func (a *Attachment) encodedDeclarations() ([]mcpserver.Declaration, error) {
	declarations := make([]mcpserver.Declaration, len(a.entries))
	for i, server := range a.Declarations() {
		encoded, err := json.Marshal(server)
		if err != nil {
			return nil, err
		}
		declarations[i] = mcpserver.Declaration{Name: server.ACP.Name, JSON: encoded}
	}
	return declarations, nil
}

// HandleSessionSetup is connection-scoped (global) attachment for proxies:
// it wraps the handler that forwards client requests to the agent and
// declares the attachment in every session setup request on the way, through
// AppendDeclarations. Malformed setup params are answered with -32602 without
// calling next. The attachment's router must serve the connection to the
// agent, where its mcp/message requests arrive.
func (a *Attachment) HandleSessionSetup(next acp.Handler) acp.Handler {
	return func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		rewritten, err := a.AppendDeclarations(method, params)
		if err != nil {
			return nil, err
		}
		if next == nil {
			return nil, rpcparams.MethodNotFound(method)
		}
		return next(ctx, method, rewritten)
	}
}

// attachTo registers servers, lets call send the session setup request with
// their declarations appended to existing, and releases the registrations when
// validation or the request fails. It is shared by every attached-session
// helper so new setup methods only supply call.
func attachTo[T any](ctx context.Context, router *MessageRouter, servers []*Server, existing []schema.McpServer, call func(declared []schema.McpServer) (T, error)) (T, *Attachment, error) {
	var zero T
	if router == nil || router.connection == nil {
		return zero, nil, errors.New("attached MCP servers require a router bound to a connection")
	}
	attachment, err := router.Attach(servers...)
	if err != nil {
		return zero, nil, err
	}
	declared := append(slices.Clone(existing), attachment.Declarations()...)
	result, err := call(declared)
	if err != nil {
		// The agent may already have called a declared server; join that work
		// before reporting the failure, even when ctx was the cause.
		_ = attachment.Close(context.WithoutCancel(ctx))
		return zero, nil, err
	}
	return result, attachment, nil
}

// NewAttachedSession attaches servers to the router and creates a session
// that declares them after options.Servers. The registrations are live before
// session/new is written and are released when validation or the request
// fails. On success, close the returned Attachment when the session ends.
// The agent must advertise mcpCapabilities.acp.
func NewAttachedSession(ctx context.Context, router *MessageRouter, initialization schema.InitializeResponse, directory string, options NewSessionOptions, servers ...*Server) (acp.Session, *Attachment, error) {
	return attachTo(ctx, router, servers, options.Servers, func(declared []schema.McpServer) (acp.Session, error) {
		options.Servers = declared
		return NewSession(ctx, router.connection, initialization, directory, options)
	})
}

// LoadAttachedSession is NewAttachedSession for session/load.
func LoadAttachedSession(ctx context.Context, router *MessageRouter, initialization schema.InitializeResponse, request schema.LoadSessionRequest, servers ...*Server) (schema.LoadSessionResponse, *Attachment, error) {
	return attachTo(ctx, router, servers, request.MCPServers, func(declared []schema.McpServer) (schema.LoadSessionResponse, error) {
		request.MCPServers = declared
		return LoadSession(ctx, router.connection, initialization, request)
	})
}

// ResumeAttachedSession is NewAttachedSession for session/resume.
func ResumeAttachedSession(ctx context.Context, router *MessageRouter, initialization schema.InitializeResponse, request schema.ResumeSessionRequest, servers ...*Server) (schema.ResumeSessionResponse, *Attachment, error) {
	return attachTo(ctx, router, servers, request.MCPServers, func(declared []schema.McpServer) (schema.ResumeSessionResponse, error) {
		request.MCPServers = declared
		return ResumeSession(ctx, router.connection, initialization, request)
	})
}

// ForkAttachedSession is NewAttachedSession for the unstable session/fork.
func ForkAttachedSession(ctx context.Context, router *MessageRouter, initialization schema.InitializeResponse, request schema.ForkSessionRequest, servers ...*Server) (schema.ForkSessionResponse, *Attachment, error) {
	return attachTo(ctx, router, servers, request.MCPServers, func(declared []schema.McpServer) (schema.ForkSessionResponse, error) {
		request.MCPServers = declared
		return ForkSession(ctx, router.connection, initialization, request)
	})
}
