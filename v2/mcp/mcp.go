// Package mcp provides the draft MCP-over-ACP session options and the
// request-scoped mcp/message mover (MessageClient and MessageRouter). It
// mirrors the Rust SDK's separately enabled unstable_mcp_over_acp feature:
// importing this package is the explicit Go opt-in.
//
// The session helpers (NewSession, ResumeSession, and ForkSession) send the
// unstable draft-v2 requests, whose McpServer union adds the native acp
// transport and which can carry a client-provided system prompt. They gate on
// the unstable initialize response, which
// github.com/BrokkAi/acp-go/v2.Connection.InitializeExtended decodes:
//
//	var initialization unstable.InitializeResponse // schema/v2/unstable
//	_, err := connection.InitializeExtended(ctx, capabilities, info, &initialization)
//	session, err := mcp.NewSession(ctx, connection, initialization, directory, mcp.NewSessionOptions{
//		Servers: []unstable.McpServer{mcp.NewStdioServer("docs", "/usr/local/bin/docs-mcp", nil, nil)},
//	})
//
// The package shares a standard-library MCP server framework with
// github.com/BrokkAi/acp-go/mcp: Server, Service, RequestContext, ToolRegistry,
// and NewToolServer are the same types in both, so a server defined once
// attaches to v1 and v2 sessions. NewAttachedSession, ResumeAttachedSession,
// and ForkAttachedSession attach servers with generated server IDs to draft
// v2 sessions, reading the acp capability from the unstable initialize
// response, and reject a server whose name the request already declares with
// -32602 before writing.
// Attachment.HandleSessionSetup attaches servers to every session/new,
// session/resume, and session/fork a proxy forwards.
package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	"github.com/BrokkAi/acp-go/internal/v2session"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// NewSessionOptions carries session/new fields that include MCP servers.
type NewSessionOptions struct {
	AdditionalDirectories []string
	Servers               []schema.McpServer
	// SystemPrompt is the optional client-provided system prompt. It needs
	// capabilities.session.systemPrompt, and override mode needs
	// capabilities.session.systemPrompt.mode.override; see
	// github.com/BrokkAi/acp-go/v2/unstable.ValidateSessionSystemPrompt.
	SystemPrompt *schema.SystemPrompt
}

func NewStdioServer(name, command string, args []string, env []schema.EnvVariable) schema.McpServer {
	return schema.McpServer{Stdio: &schema.McpServerStdio{
		Name: name, Command: schema.AbsolutePath(command), Args: args, Env: env,
	}}
}

func NewHTTPServer(name, url string, headers []schema.HttpHeader) schema.McpServer {
	return schema.McpServer{HTTP: &schema.McpServerHttp{Name: name, URL: url, Headers: headers}}
}

// NewSession sends session/new with MCP servers and an optional system
// prompt. It applies the github.com/BrokkAi/acp-go/v2/unstable.NewSession
// checks (the baseline capabilities.session surface, an absolute cwd, gated
// absolute additional directories, and the systemPrompt rules) plus the
// transport checks: each server needs its advertised capabilities.session.mcp
// transport and a name plus its command, URL, or server ID.
func NewSession(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, directory string, options NewSessionOptions) (acpv2.Session, error) {
	var session acpv2.Session
	request := schema.NewSessionRequest{
		Cwd:                   schema.AbsolutePath(directory),
		AdditionalDirectories: paths(options.AdditionalDirectories),
		MCPServers:            options.Servers,
		SystemPrompt:          options.SystemPrompt,
	}
	if err := validateNewSession(initialization, request); err != nil {
		return session, err
	}
	if err := connection.Call(ctx, schema.SessionNewMethodName, request, &session); err != nil {
		return session, err
	}
	if session.SessionID == "" {
		return session, fmt.Errorf("agent returned an empty session ID")
	}
	return session, nil
}

// ResumeSession sends session/resume with MCP servers, with the
// github.com/BrokkAi/acp-go/v2/unstable.ResumeSession checks (including the
// systemPrompt rules) and the NewSession transport checks.
func ResumeSession(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	var result schema.ResumeSessionResponse
	if err := v2session.ValidateResumeSessionRequest(initialization, request); err != nil {
		return result, err
	}
	if err := validateServers(initialization, request.MCPServers); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.SessionResumeMethodName, request, &result)
}

// validateNewSession applies the session/new checks shared by NewSession and
// NewAttachedSession.
func validateNewSession(initialization schema.InitializeResponse, request schema.NewSessionRequest) error {
	if err := v2session.ValidateNewSessionRequest(initialization, request); err != nil {
		return err
	}
	return validateServers(initialization, request.MCPServers)
}

// validateServers requires each server's advertised capabilities.session.mcp
// transport, including the native acp transport, and its identifying fields.
func validateServers(initialization schema.InitializeResponse, servers []schema.McpServer) error {
	var capabilities *schema.McpCapabilities
	if initialization.Capabilities != nil && initialization.Capabilities.Session != nil {
		capabilities = initialization.Capabilities.Session.MCP
	}
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
			if server.ACP.Name == "" || strings.TrimSpace(string(server.ACP.ServerID)) == "" {
				return fmt.Errorf("acp MCP server %d requires a name and server ID", i)
			}
		default:
			return fmt.Errorf("MCP server %d has no supported transport variant", i)
		}
	}
	return nil
}

func paths(values []string) []schema.AbsolutePath {
	if values == nil {
		return nil
	}
	result := make([]schema.AbsolutePath, len(values))
	for i, value := range values {
		result[i] = schema.AbsolutePath(value)
	}
	return result
}
