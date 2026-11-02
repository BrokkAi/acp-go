package unstable

import (
	"context"
	"fmt"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	"github.com/BrokkAi/acp-go/internal/systemprompt"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

// NewSession sends the unstable session/new, the request that can carry a
// client-provided system prompt (the counterpart of the reference SDK's
// build_session_from). cwd must be absolute; additional directories need
// sessionCapabilities.additionalDirectories and must each be absolute; a
// systemPrompt must pass the checks described on ValidateSessionSystemPrompt.
// Inline MCP servers are rejected so MCP stays behind the explicit
// github.com/BrokkAi/acp-go/mcp import, whose NewSession also carries a system
// prompt. The response must carry a non-blank session ID.
func NewSession(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, request schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	var result schema.NewSessionResponse
	if err := validateSessionSetup(initialization, request.Cwd, request.AdditionalDirectories, request.MCPServers, request.SystemPrompt); err != nil {
		return result, err
	}
	// mcpServers is required in v1, so send an empty array, not null.
	request.MCPServers = []schema.McpServer{}
	if err := connection.Call(ctx, schema.SessionNewMethodName, request, &result); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("session ID", string(result.SessionID)); err != nil {
		return result, err
	}
	return result, nil
}

// LoadSession sends session/load with the same checks as NewSession, gated on
// agentCapabilities.loadSession and a non-blank session ID. Clients should
// re-send the systemPrompt the session was created with.
func LoadSession(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, request schema.LoadSessionRequest) (schema.LoadSessionResponse, error) {
	var result schema.LoadSessionResponse
	if capabilities := initialization.AgentCapabilities; capabilities == nil || capabilities.LoadSession == nil || !*capabilities.LoadSession {
		return result, fmt.Errorf("agent did not advertise session/load support")
	}
	if err := acpvalidate.Identifier("session ID", string(request.SessionID)); err != nil {
		return result, err
	}
	if err := validateSessionSetup(initialization, request.Cwd, request.AdditionalDirectories, request.MCPServers, request.SystemPrompt); err != nil {
		return result, err
	}
	request.MCPServers = []schema.McpServer{}
	return result, connection.Call(ctx, schema.SessionLoadMethodName, request, &result)
}

// ResumeSession sends session/resume with the same checks as NewSession,
// gated on sessionCapabilities.resume and a non-blank session ID. Clients
// should re-send the systemPrompt the session was created with.
func ResumeSession(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, request schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	var result schema.ResumeSessionResponse
	if capabilities := sessionCapabilities(initialization); capabilities == nil || capabilities.Resume == nil {
		return result, fmt.Errorf("agent did not advertise session/resume support")
	}
	if err := acpvalidate.Identifier("session ID", string(request.SessionID)); err != nil {
		return result, err
	}
	if err := validateSessionSetup(initialization, request.Cwd, request.AdditionalDirectories, request.MCPServers, request.SystemPrompt); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.SessionResumeMethodName, request, &result)
}

// ValidateSessionSystemPrompt is the client-side check NewSession,
// LoadSession, ResumeSession, ForkSession, and the github.com/BrokkAi/acp-go/mcp
// session helpers run on a systemPrompt before it reaches the wire: the agent
// must advertise sessionCapabilities.systemPrompt, the mode must be present
// and either Append or Override, Override needs
// sessionCapabilities.systemPrompt.mode.override, and the content must be a
// non-empty list of text blocks. A nil prompt is valid.
func ValidateSessionSystemPrompt(initialization schema.InitializeResponse, prompt *SystemPrompt) error {
	return systemprompt.V1Client(initialization, prompt)
}

func validateSessionSetup(initialization schema.InitializeResponse, cwd string, additional []string, servers []schema.McpServer, prompt *SystemPrompt) error {
	if err := acpvalidate.AbsolutePath("session working directory", cwd); err != nil {
		return err
	}
	if len(additional) > 0 && !advertisesAdditionalDirectories(initialization) {
		return fmt.Errorf("agent did not advertise additionalDirectories support")
	}
	for _, directory := range additional {
		if err := acpvalidate.AbsolutePath("session additional directory", directory); err != nil {
			return err
		}
	}
	if len(servers) > 0 {
		return fmt.Errorf("MCP servers require the explicit github.com/BrokkAi/acp-go/mcp package")
	}
	return ValidateSessionSystemPrompt(initialization, prompt)
}
