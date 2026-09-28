// Package mcp provides the v1 counterpart to
// github.com/BrokkAi/acp-go/v2/mcp: session operations that accept the
// unstable MCP server transports, including the native MCP-over-ACP variant
// that the stable v1 schema does not model.
//
// Importing this package is the explicit opt-in, mirroring the Rust SDK's
// separately enabled unstable_mcp_over_acp feature. Capability gating reads the
// unstable initialize response, whose McpCapabilities carry the acp/http/sse
// flags:
//
//	var initialization unstable.InitializeResponse
//	json.Unmarshal(encodedInitializeResult, &initialization)
//	session, err := mcp.NewSession(ctx, connection, initialization, directory, mcp.NewSessionOptions{
//		Servers: []unstable.McpServer{{Stdio: &unstable.McpServerStdio{Name: "docs", Command: "npx"}}},
//	})
package mcp

import (
	"context"
	"fmt"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

// NewSessionOptions carries session/new fields that include MCP servers.
type NewSessionOptions struct {
	AdditionalDirectories []string
	Servers               []schema.McpServer
}

// NewSession creates a session whose MCP servers may use any unstable
// transport, including native MCP-over-ACP.
func NewSession(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, directory string, options NewSessionOptions) (acp.Session, error) {
	var session acp.Session
	if err := validatePaths(initialization, directory, options.AdditionalDirectories); err != nil {
		return session, err
	}
	if err := validateServers(initialization, options.Servers); err != nil {
		return session, err
	}
	request := schema.NewSessionRequest{
		Cwd:                   directory,
		AdditionalDirectories: options.AdditionalDirectories,
		MCPServers:            options.Servers,
	}
	if err := connection.Call(ctx, schema.SessionNewMethodName, request, &session); err != nil {
		return session, err
	}
	if err := acpvalidate.Identifier("session ID", string(session.SessionID)); err != nil {
		return session, err
	}
	return session, nil
}

// ResumeSession resumes a session with the unstable MCP server transports.
func ResumeSession(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, request schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	var result schema.ResumeSessionResponse
	if err := requireResume(initialization); err != nil {
		return result, err
	}
	if err := validatePaths(initialization, request.Cwd, request.AdditionalDirectories); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("session ID", string(request.SessionID)); err != nil {
		return result, err
	}
	if err := validateServers(initialization, request.MCPServers); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.SessionResumeMethodName, request, &result)
}

// ForkSession forks a session with the unstable MCP server transports.
func ForkSession(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, request schema.ForkSessionRequest) (schema.ForkSessionResponse, error) {
	var result schema.ForkSessionResponse
	if err := requireFork(initialization); err != nil {
		return result, err
	}
	if err := validatePaths(initialization, request.Cwd, request.AdditionalDirectories); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("session ID", string(request.SessionID)); err != nil {
		return result, err
	}
	if err := validateServers(initialization, request.MCPServers); err != nil {
		return result, err
	}
	if err := connection.Call(ctx, schema.SessionForkMethodName, request, &result); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("forked session ID", string(result.SessionID)); err != nil {
		return result, err
	}
	return result, nil
}

func requireResume(initialization schema.InitializeResponse) error {
	var capabilities *schema.SessionCapabilities
	if initialization.AgentCapabilities != nil {
		capabilities = initialization.AgentCapabilities.SessionCapabilities
	}
	if capabilities == nil || capabilities.Resume == nil {
		return fmt.Errorf("agent did not advertise session/resume support")
	}
	return nil
}

func requireFork(initialization schema.InitializeResponse) error {
	var capabilities *schema.SessionCapabilities
	if initialization.AgentCapabilities != nil {
		capabilities = initialization.AgentCapabilities.SessionCapabilities
	}
	if capabilities == nil || capabilities.Fork == nil {
		return fmt.Errorf("agent did not advertise session/fork support")
	}
	return nil
}

func validatePaths(initialization schema.InitializeResponse, directory string, additional []string) error {
	if err := acpvalidate.AbsolutePath("ACP path", directory); err != nil {
		return err
	}
	if len(additional) > 0 {
		var capabilities *schema.SessionCapabilities
		if initialization.AgentCapabilities != nil {
			capabilities = initialization.AgentCapabilities.SessionCapabilities
		}
		if capabilities == nil || capabilities.AdditionalDirectories == nil {
			return fmt.Errorf("agent did not advertise additionalDirectories support")
		}
	}
	for _, value := range additional {
		if err := acpvalidate.AbsolutePath("ACP path", value); err != nil {
			return err
		}
	}
	return nil
}

// validateServers mirrors the reference rules: stdio is baseline, while HTTP,
// SSE, and native ACP transports each need their advertised capability.
func validateServers(initialization schema.InitializeResponse, servers []schema.McpServer) error {
	var capabilities *schema.McpCapabilities
	if initialization.AgentCapabilities != nil {
		capabilities = initialization.AgentCapabilities.MCPCapabilities
	}
	enabled := func(field *bool) bool { return field != nil && *field }
	for i, server := range servers {
		switch {
		case server.Stdio != nil:
			if server.Stdio.Name == "" || server.Stdio.Command == "" {
				return fmt.Errorf("stdio MCP server %d requires a name and command", i)
			}
		case server.HTTP != nil:
			if capabilities == nil || !enabled(capabilities.HTTP) {
				return fmt.Errorf("agent did not advertise HTTP MCP server support (server %d)", i)
			}
			if server.HTTP.Name == "" || server.HTTP.URL == "" {
				return fmt.Errorf("HTTP MCP server %d requires a name and URL", i)
			}
		case server.SSE != nil:
			if capabilities == nil || !enabled(capabilities.SSE) {
				return fmt.Errorf("agent did not advertise SSE MCP server support (server %d)", i)
			}
			if server.SSE.Name == "" || server.SSE.URL == "" {
				return fmt.Errorf("SSE MCP server %d requires a name and URL", i)
			}
		case server.ACP != nil:
			if capabilities == nil || !enabled(capabilities.ACP) {
				return fmt.Errorf("agent did not advertise native MCP-over-ACP server support (server %d)", i)
			}
			if server.ACP.Name == "" {
				return fmt.Errorf("MCP-over-ACP server %d requires a name", i)
			}
			if err := acpvalidate.Identifier("MCP-over-ACP server ID", string(server.ACP.ServerID)); err != nil {
				return fmt.Errorf("server %d: %w", i, err)
			}
		default:
			return fmt.Errorf("MCP server %d has no transport variant", i)
		}
	}
	return nil
}
