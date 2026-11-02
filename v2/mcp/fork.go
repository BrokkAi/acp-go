package mcp

import (
	"context"
	"fmt"

	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	"github.com/BrokkAi/acp-go/internal/v2session"
	unstable "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// ForkSession sends session/fork with MCP servers, the MCP-carrying
// counterpart of github.com/BrokkAi/acp-go/v2/unstable.ForkSession. session/fork
// and the native acp transport exist only in the unstable draft-v2 schema, so
// both the request and the capability gate use its types.
//
// It applies the v2/unstable fork checks (capabilities.session.fork, a
// non-blank session ID, an absolute cwd, gated absolute additional
// directories, and a non-blank forked session ID in the response) and the
// transport checks of NewSession and ResumeSession: each server needs its
// advertised capabilities.session.mcp transport and a name plus its
// command, URL, or server ID. request.MCPServers is the complete set for the
// forked session; agents do not carry servers over from the source session.
func ForkSession(ctx context.Context, connection *acpv2.Connection, initialization unstable.InitializeResponse, request unstable.ForkSessionRequest) (unstable.ForkSessionResponse, error) {
	var result unstable.ForkSessionResponse
	if err := v2session.ValidateForkRequest(initialization, request); err != nil {
		return result, err
	}
	if err := validateForkServers(initialization, request.MCPServers); err != nil {
		return result, err
	}
	if err := connection.Call(ctx, unstable.SessionForkMethodName, request, &result); err != nil {
		return result, err
	}
	if err := v2session.ValidateForkResponse(result); err != nil {
		return result, err
	}
	return result, nil
}

// validateForkServers is validateServers for the unstable server union, which
// adds the native acp transport gated on capabilities.session.mcp.acp.
func validateForkServers(initialization unstable.InitializeResponse, servers []unstable.McpServer) error {
	var capabilities *unstable.McpCapabilities
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
			if server.ACP.Name == "" || server.ACP.ServerID == "" {
				return fmt.Errorf("acp MCP server %d requires a name and server ID", i)
			}
		default:
			return fmt.Errorf("MCP server %d has no supported transport variant", i)
		}
	}
	return nil
}
