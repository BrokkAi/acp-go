package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/BrokkAi/acp-go/schema"
)

// NewSessionOptions carries optional fields for session/new.
type NewSessionOptions struct {
	AdditionalDirectories []string
	MCPServers            []schema.McpServer
}

func requireAbsolutePath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("ACP path must be absolute: %q", path)
	}
	return nil
}

func requireCapability(supported bool, method string) error {
	if !supported {
		return fmt.Errorf("agent did not advertise %s support", method)
	}
	return nil
}

func validateAdditionalDirectories(init Initialization, directories []string) error {
	if len(directories) == 0 {
		return nil
	}
	if init.AgentCapabilities == nil {
		return fmt.Errorf("agent did not advertise additionalDirectories support")
	}
	for _, directory := range directories {
		if err := requireAbsolutePath(directory); err != nil {
			return err
		}
	}
	capabilities := init.AgentCapabilities.SessionCapabilities
	return requireCapability(capabilities != nil && capabilities.AdditionalDirectories != nil, "additionalDirectories")
}

func validateMCPServers(init Initialization, servers []schema.McpServer) error {
	var capabilities *schema.McpCapabilities
	if init.AgentCapabilities != nil {
		capabilities = init.AgentCapabilities.MCPCapabilities
	}
	enabled := func(field *bool) bool { return field != nil && *field }
	for i, server := range servers {
		switch {
		case server.Stdio != nil:
			// stdio MCP servers are mandatory for agents.
			if server.Stdio.Name == "" || server.Stdio.Command == "" {
				return fmt.Errorf("MCP server %d requires a name and command", i)
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
		default:
			return fmt.Errorf("MCP server %d has no transport variant", i)
		}
	}
	return nil
}

func (c *Connection) NewSessionWithOptions(ctx context.Context, init Initialization, directory string, options NewSessionOptions) (Session, error) {
	var session Session
	if err := requireAbsolutePath(directory); err != nil {
		return session, err
	}
	if err := validateAdditionalDirectories(init, options.AdditionalDirectories); err != nil {
		return session, err
	}
	if err := validateMCPServers(init, options.MCPServers); err != nil {
		return session, err
	}
	if options.MCPServers == nil {
		options.MCPServers = make([]schema.McpServer, 0)
	}
	request := schema.NewSessionRequest{
		Cwd:                   directory,
		MCPServers:            options.MCPServers,
		AdditionalDirectories: options.AdditionalDirectories,
	}
	if err := c.Call(ctx, schema.SessionNewMethodName, request, &session); err != nil {
		return session, err
	}
	if session.SessionID == "" {
		return session, fmt.Errorf("agent returned an empty session ID")
	}
	return session, nil
}

func (c *Connection) LoadSession(ctx context.Context, init Initialization, request schema.LoadSessionRequest) (schema.LoadSessionResponse, error) {
	var result schema.LoadSessionResponse
	var load bool
	if init.AgentCapabilities != nil && init.AgentCapabilities.LoadSession != nil {
		load = *init.AgentCapabilities.LoadSession
	}
	if err := requireCapability(load, schema.SessionLoadMethodName); err != nil {
		return result, err
	}
	if err := requireAbsolutePath(request.Cwd); err != nil {
		return result, err
	}
	if request.SessionID == "" {
		return result, fmt.Errorf("session ID is required")
	}
	if err := validateAdditionalDirectories(init, request.AdditionalDirectories); err != nil {
		return result, err
	}
	if err := validateMCPServers(init, request.MCPServers); err != nil {
		return result, err
	}
	if request.MCPServers == nil {
		request.MCPServers = make([]schema.McpServer, 0)
	}
	return result, c.Call(ctx, schema.SessionLoadMethodName, request, &result)
}

func (c *Connection) ResumeSession(ctx context.Context, init Initialization, request schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	var result schema.ResumeSessionResponse
	var capabilities *schema.SessionCapabilities
	if init.AgentCapabilities != nil {
		capabilities = init.AgentCapabilities.SessionCapabilities
	}
	if err := requireCapability(capabilities != nil && capabilities.Resume != nil, schema.SessionResumeMethodName); err != nil {
		return result, err
	}
	if err := requireAbsolutePath(request.Cwd); err != nil {
		return result, err
	}
	if request.SessionID == "" {
		return result, fmt.Errorf("session ID is required")
	}
	if err := validateAdditionalDirectories(init, request.AdditionalDirectories); err != nil {
		return result, err
	}
	if err := validateMCPServers(init, request.MCPServers); err != nil {
		return result, err
	}
	return result, c.Call(ctx, schema.SessionResumeMethodName, request, &result)
}

func (c *Connection) CloseSession(ctx context.Context, init Initialization, sessionID SessionID) error {
	var capabilities *schema.SessionCapabilities
	if init.AgentCapabilities != nil {
		capabilities = init.AgentCapabilities.SessionCapabilities
	}
	if err := requireCapability(capabilities != nil && capabilities.Close != nil, schema.SessionCloseMethodName); err != nil {
		return err
	}
	if sessionID == "" {
		return fmt.Errorf("session ID is required")
	}
	var result schema.CloseSessionResponse
	return c.Call(ctx, schema.SessionCloseMethodName, schema.CloseSessionRequest{SessionID: sessionID}, &result)
}

func (c *Connection) ListSessions(ctx context.Context, init Initialization, request schema.ListSessionsRequest) (schema.ListSessionsResponse, error) {
	var result schema.ListSessionsResponse
	var capabilities *schema.SessionCapabilities
	if init.AgentCapabilities != nil {
		capabilities = init.AgentCapabilities.SessionCapabilities
	}
	if err := requireCapability(capabilities != nil && capabilities.List != nil, schema.SessionListMethodName); err != nil {
		return result, err
	}
	if request.Cwd != nil {
		if err := requireAbsolutePath(*request.Cwd); err != nil {
			return result, err
		}
	}
	return result, c.Call(ctx, schema.SessionListMethodName, request, &result)
}

func (c *Connection) DeleteSession(ctx context.Context, init Initialization, sessionID SessionID) error {
	var capabilities *schema.SessionCapabilities
	if init.AgentCapabilities != nil {
		capabilities = init.AgentCapabilities.SessionCapabilities
	}
	if err := requireCapability(capabilities != nil && capabilities.Delete != nil, schema.SessionDeleteMethodName); err != nil {
		return err
	}
	if sessionID == "" {
		return fmt.Errorf("session ID is required")
	}
	var result schema.DeleteSessionResponse
	return c.Call(ctx, schema.SessionDeleteMethodName, schema.DeleteSessionRequest{SessionID: sessionID}, &result)
}

func (c *Connection) Logout(ctx context.Context, init Initialization) error {
	var auth *schema.AgentAuthCapabilities
	if init.AgentCapabilities != nil && init.AgentCapabilities.Auth != nil {
		auth = init.AgentCapabilities.Auth
	}
	if auth == nil || auth.Logout == nil {
		return fmt.Errorf("agent did not advertise logout support")
	}
	var result schema.LogoutResponse
	return c.Call(ctx, schema.LogoutMethodName, schema.LogoutRequest{}, &result)
}

func (c *Connection) PromptContent(ctx context.Context, init Initialization, session Session, prompt []Content) (schema.StopReason, error) {
	var result schema.PromptResponse
	if session.SessionID == "" {
		return result.StopReason, fmt.Errorf("session ID is required")
	}
	if err := validatePromptCapabilities(init, prompt); err != nil {
		return result.StopReason, err
	}
	request := schema.PromptRequest{SessionID: session.SessionID, Prompt: prompt}
	err := c.Call(ctx, schema.SessionPromptMethodName, request, &result)
	if err != nil && ctx.Err() != nil {
		cancelCtx, stop := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_ = c.Notify(cancelCtx, schema.SessionCancelMethodName, schema.CancelNotification{SessionID: session.SessionID})
		stop()
	}
	return result.StopReason, err
}

func (c *Connection) CancelSession(ctx context.Context, sessionID SessionID) error {
	if sessionID == "" {
		return fmt.Errorf("session ID is required")
	}
	return c.Notify(ctx, schema.SessionCancelMethodName, schema.CancelNotification{SessionID: sessionID})
}

// SessionUpdates adapts the raw connection notification callback to the
// generated, discriminated-union SessionNotification type. Other ACP
// notifications are ignored, as required by the transport layer.
func SessionUpdates(handler func(Update) error) Notifications {
	return func(method string, raw json.RawMessage) error {
		if method != schema.SessionUpdateMethodName {
			return nil
		}
		var update Update
		if err := json.Unmarshal(raw, &update); err != nil {
			return err
		}
		return handler(update)
	}
}
