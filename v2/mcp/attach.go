package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	"github.com/BrokkAi/acp-go/internal/mcpserver"
	unstable "github.com/BrokkAi/acp-go/schema/v2/unstable"
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
			_ = attachment.release(context.Background())
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
func (a *Attachment) Declarations() []unstable.McpServer {
	servers := make([]unstable.McpServer, len(a.entries))
	for i, entry := range a.entries {
		servers[i] = unstable.McpServer{ACP: &unstable.McpServerAcp{
			Name:     entry.server.Name(),
			ServerID: unstable.McpServerAcpId(entry.registration.ServerID()),
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

// isSessionSetup reports whether method declares MCP servers in draft ACP v2.
func isSessionSetup(method string) bool {
	switch method {
	case unstable.SessionNewMethodName, unstable.SessionResumeMethodName, unstable.SessionForkMethodName:
		return true
	}
	return false
}

// AppendDeclarations appends the attachment's declarations to the mcpServers
// of a raw session/new, session/resume, or session/fork params object and
// returns the new params. Every other member keeps its value, so fields the
// generated types do not model survive; the result is semantically equal, not
// byte-identical (members may be re-ordered). Names must be unique within
// mcpServers: when the params already declare a server with an attached
// server's name, it fails with -32602 and the request must not be forwarded.
// Other methods, and every method after Close, return params unchanged.
func (a *Attachment) AppendDeclarations(method string, params json.RawMessage) (json.RawMessage, error) {
	if !isSessionSetup(method) || a.isClosed() {
		return params, nil
	}
	declarations, err := a.encodedDeclarations()
	if err != nil {
		return nil, err
	}
	return mcpserver.AppendDeclarations(params, declarations, true)
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
// AppendDeclarations. A name collision or malformed setup params are answered
// with -32602 without calling next. The attachment's router must serve the
// connection to the agent, where its mcp/message requests arrive.
func (a *Attachment) HandleSessionSetup(next acp.Handler) acp.Handler {
	return func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		rewritten, err := a.AppendDeclarations(method, params)
		if err != nil {
			return nil, err
		}
		if next == nil {
			return nil, &acp.RPCError{Code: -32601, Message: "unsupported method: " + method}
		}
		return next(ctx, method, rewritten)
	}
}

// attachTo registers servers, rejects a name already declared in existing
// with -32602 before writing, lets call send the session setup request with
// the declarations appended, and releases the registrations when validation
// or the request fails. Every attached-session helper is built on it, so a
// new setup method only supplies call.
func attachTo[T any](ctx context.Context, router *MessageRouter, servers []*Server, existing []unstable.McpServer, call func(declared []unstable.McpServer) (T, error)) (T, *Attachment, error) {
	var zero T
	if router == nil || router.connection == nil {
		return zero, nil, errors.New("attached MCP servers require a router bound to a connection")
	}
	attachment, err := router.Attach(servers...)
	if err != nil {
		return zero, nil, err
	}
	declarations, err := attachment.encodedDeclarations()
	if err == nil {
		err = mcpserver.CheckDeclaredNames(serverNames(existing), declarations)
	}
	var result T
	if err == nil {
		result, err = call(append(slices.Clone(existing), attachment.Declarations()...))
	}
	if err != nil {
		// The agent may already have called a declared server; join that work
		// before reporting the failure, even when ctx was the cause.
		_ = attachment.Close(context.WithoutCancel(ctx))
		return zero, nil, err
	}
	return result, attachment, nil
}

func serverNames(servers []unstable.McpServer) []string {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		switch {
		case server.Stdio != nil:
			names = append(names, server.Stdio.Name)
		case server.HTTP != nil:
			names = append(names, server.HTTP.Name)
		case server.ACP != nil:
			names = append(names, server.ACP.Name)
		case server.Other != nil:
			var declared struct {
				Name *string `json:"name"`
			}
			if json.Unmarshal(server.Other.Raw, &declared) == nil && declared.Name != nil {
				names = append(names, *declared.Name)
			}
		}
	}
	return names
}

// NewAttachedSession attaches servers to the router and sends session/new
// declaring them after request.MCPServers. The registrations are live before
// the request is written and are released when validation or the request
// fails; a server whose name request.MCPServers already declares is rejected
// with -32602 before writing. On success, close the returned Attachment when
// the session ends. The agent must advertise capabilities.session.mcp.acp,
// read from the unstable initialize response.
func NewAttachedSession(ctx context.Context, router *MessageRouter, initialization unstable.InitializeResponse, request unstable.NewSessionRequest, servers ...*Server) (unstable.NewSessionResponse, *Attachment, error) {
	return attachTo(ctx, router, servers, request.MCPServers, func(declared []unstable.McpServer) (unstable.NewSessionResponse, error) {
		var response unstable.NewSessionResponse
		request.MCPServers = declared
		if err := validateUnstableSetup(initialization, string(request.Cwd), request.AdditionalDirectories, request.MCPServers); err != nil {
			return response, err
		}
		if err := router.connection.Call(ctx, unstable.SessionNewMethodName, request, &response); err != nil {
			return response, err
		}
		if response.SessionID == "" {
			return response, fmt.Errorf("agent returned an empty session ID")
		}
		return response, nil
	})
}

// ResumeAttachedSession is NewAttachedSession for session/resume.
func ResumeAttachedSession(ctx context.Context, router *MessageRouter, initialization unstable.InitializeResponse, request unstable.ResumeSessionRequest, servers ...*Server) (unstable.ResumeSessionResponse, *Attachment, error) {
	return attachTo(ctx, router, servers, request.MCPServers, func(declared []unstable.McpServer) (unstable.ResumeSessionResponse, error) {
		var response unstable.ResumeSessionResponse
		request.MCPServers = declared
		if request.SessionID == "" {
			return response, fmt.Errorf("session ID is required")
		}
		if err := validateUnstableSetup(initialization, string(request.Cwd), request.AdditionalDirectories, request.MCPServers); err != nil {
			return response, err
		}
		return response, router.connection.Call(ctx, unstable.SessionResumeMethodName, request, &response)
	})
}

// ForkAttachedSession is NewAttachedSession for the unstable session/fork,
// the counterpart of the reference ForkSessionBuilder::with_mcp_server. The
// agent must advertise capabilities.session.fork, and the response must carry
// a forked session ID.
func ForkAttachedSession(ctx context.Context, router *MessageRouter, initialization unstable.InitializeResponse, request unstable.ForkSessionRequest, servers ...*Server) (unstable.ForkSessionResponse, *Attachment, error) {
	return attachTo(ctx, router, servers, request.MCPServers, func(declared []unstable.McpServer) (unstable.ForkSessionResponse, error) {
		request.MCPServers = declared
		return forkSession(ctx, router, initialization, request)
	})
}

// forkSession validates and sends one session/fork. It stays private so the
// public v2 fork facade (#61) can own that name.
func forkSession(ctx context.Context, router *MessageRouter, initialization unstable.InitializeResponse, request unstable.ForkSessionRequest) (unstable.ForkSessionResponse, error) {
	var response unstable.ForkSessionResponse
	if initialization.Capabilities == nil || initialization.Capabilities.Session == nil || initialization.Capabilities.Session.Fork == nil {
		return response, fmt.Errorf("agent did not advertise session/fork support")
	}
	if request.SessionID == "" {
		return response, fmt.Errorf("session ID is required")
	}
	if err := validateUnstableSetup(initialization, string(request.Cwd), request.AdditionalDirectories, request.MCPServers); err != nil {
		return response, err
	}
	if err := router.connection.Call(ctx, unstable.SessionForkMethodName, request, &response); err != nil {
		return response, err
	}
	if response.SessionID == "" {
		return response, fmt.Errorf("agent returned an empty forked session ID")
	}
	return response, nil
}

// validateUnstableSetup applies the session setup checks to the unstable v2
// request types, whose McpServer models the native acp transport.
func validateUnstableSetup(initialization unstable.InitializeResponse, directory string, additional []unstable.AbsolutePath, servers []unstable.McpServer) error {
	var session *unstable.SessionCapabilities
	if initialization.Capabilities != nil {
		session = initialization.Capabilities.Session
	}
	if session == nil {
		return fmt.Errorf("agent did not advertise the ACP v2 session method surface")
	}
	if err := acpvalidate.AbsolutePath("ACP path", directory); err != nil {
		return err
	}
	if len(additional) > 0 && session.AdditionalDirectories == nil {
		return fmt.Errorf("agent did not advertise additionalDirectories support")
	}
	for _, path := range additional {
		if err := acpvalidate.AbsolutePath("ACP path", string(path)); err != nil {
			return err
		}
	}
	capabilities := session.MCP
	for i, server := range servers {
		switch {
		case server.Stdio != nil:
			if capabilities == nil || capabilities.Stdio == nil {
				return fmt.Errorf("agent did not advertise stdio MCP server support (server %d)", i)
			}
			if server.Stdio.Name == "" || server.Stdio.Command == "" {
				return fmt.Errorf("stdio MCP server %d requires a name and command", i)
			}
			if err := acpvalidate.AbsolutePath(fmt.Sprintf("stdio MCP server %d command path", i), string(server.Stdio.Command)); err != nil {
				return err
			}
		case server.HTTP != nil:
			if capabilities == nil || capabilities.HTTP == nil {
				return fmt.Errorf("agent did not advertise HTTP MCP server support (server %d)", i)
			}
			if server.HTTP.Name == "" || server.HTTP.URL == "" {
				return fmt.Errorf("HTTP MCP server %d requires a name and URL", i)
			}
		case server.ACP != nil:
			if capabilities == nil || capabilities.ACP == nil {
				return fmt.Errorf("agent did not advertise native MCP-over-ACP server support (server %d)", i)
			}
			if server.ACP.Name == "" {
				return fmt.Errorf("MCP-over-ACP server %d requires a name", i)
			}
			if err := acpvalidate.Identifier("MCP-over-ACP server ID", string(server.ACP.ServerID)); err != nil {
				return fmt.Errorf("server %d: %w", i, err)
			}
		default:
			return fmt.Errorf("MCP server %d has no supported transport variant", i)
		}
	}
	return nil
}
