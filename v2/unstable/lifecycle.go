package unstable

import (
	"context"
	"fmt"

	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	"github.com/BrokkAi/acp-go/internal/v2session"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// NewSession sends the unstable session/new, the request that can carry a
// client-provided system prompt (the counterpart of the reference SDK's
// v2 build_session_from). The agent must advertise the baseline
// capabilities.session surface; cwd must be absolute; additional directories
// need capabilities.session.additionalDirectories and must each be absolute;
// a systemPrompt must pass ValidateSessionSystemPrompt. Inline MCP servers are
// rejected so MCP stays behind the explicit github.com/BrokkAi/acp-go/v2/mcp
// import, whose NewSession also carries a system prompt. The response must
// carry a non-blank session ID.
func NewSession(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	var result schema.NewSessionResponse
	if err := v2session.ValidateNewSessionRequest(initialization, request); err != nil {
		return result, err
	}
	if len(request.MCPServers) > 0 {
		return result, fmt.Errorf("MCP servers require the explicit github.com/BrokkAi/acp-go/v2/mcp package")
	}
	if err := connection.Call(ctx, schema.SessionNewMethodName, request, &result); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("session ID", string(result.SessionID)); err != nil {
		return result, err
	}
	return result, nil
}

// ResumeSession sends the unstable session/resume with the same checks as
// NewSession and a non-blank session ID. Clients should re-send the
// systemPrompt the session was created with. Like the stable
// v2.Connection.ResumeSession, it only sends the request; replayFrom and the
// replayed updates work as they do there.
func ResumeSession(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	var result schema.ResumeSessionResponse
	if err := v2session.ValidateResumeSessionRequest(initialization, request); err != nil {
		return result, err
	}
	if len(request.MCPServers) > 0 {
		return result, fmt.Errorf("MCP servers require the explicit github.com/BrokkAi/acp-go/v2/mcp package")
	}
	return result, connection.Call(ctx, schema.SessionResumeMethodName, request, &result)
}
