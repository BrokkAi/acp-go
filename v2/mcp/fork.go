package mcp

import (
	"context"

	"github.com/BrokkAi/acp-go/internal/v2session"
	unstable "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// ForkSession sends session/fork with MCP servers, the MCP-carrying
// counterpart of github.com/BrokkAi/acp-go/v2/unstable.ForkSession.
//
// It applies the v2/unstable fork checks (capabilities.session.fork, a
// non-blank session ID, an absolute cwd, gated absolute additional
// directories, the systemPrompt rules, and a non-blank forked session ID in
// the response) and the transport checks of NewSession and ResumeSession:
// each server needs its advertised capabilities.session.mcp transport and a
// name plus its command, URL, or server ID. request.MCPServers is the complete set for the
// forked session; agents do not carry servers over from the source session.
func ForkSession(ctx context.Context, connection *acpv2.Connection, initialization unstable.InitializeResponse, request unstable.ForkSessionRequest) (unstable.ForkSessionResponse, error) {
	var result unstable.ForkSessionResponse
	if err := v2session.ValidateForkRequest(initialization, request); err != nil {
		return result, err
	}
	if err := validateServers(initialization, request.MCPServers); err != nil {
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
