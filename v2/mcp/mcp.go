// Package mcp provides the draft MCP-over-ACP session options. It mirrors the
// Rust SDK's separately enabled unstable_mcp_over_acp feature: importing this
// package is the explicit Go opt-in.
package mcp

import (
	"context"
	"fmt"
	"path/filepath"

	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// NewSessionOptions carries session/new fields that include MCP servers.
type NewSessionOptions struct {
	AdditionalDirectories []string
	Servers               []schema.McpServer
}

func NewStdioServer(name, command string, args []string, env []schema.EnvVariable) schema.McpServer {
	return schema.McpServer{Stdio: &schema.McpServerStdio{
		Name: name, Command: schema.AbsolutePath(command), Args: args, Env: env,
	}}
}

func NewHTTPServer(name, url string, headers []schema.HttpHeader) schema.McpServer {
	return schema.McpServer{HTTP: &schema.McpServerHttp{Name: name, URL: url, Headers: headers}}
}

func NewSession(ctx context.Context, connection *acpv2.Connection, initialization acpv2.Initialization, directory string, options NewSessionOptions) (acpv2.Session, error) {
	var session acpv2.Session
	if err := validateSessionCapabilities(initialization); err != nil {
		return session, err
	}
	if err := validatePaths(initialization, directory, options.AdditionalDirectories); err != nil {
		return session, err
	}
	if err := validateServers(initialization, options.Servers); err != nil {
		return session, err
	}
	request := schema.NewSessionRequest{
		Cwd:                   schema.AbsolutePath(directory),
		AdditionalDirectories: paths(options.AdditionalDirectories),
		MCPServers:            options.Servers,
	}
	if err := connection.Call(ctx, schema.SessionNewMethodName, request, &session); err != nil {
		return session, err
	}
	if session.SessionID == "" {
		return session, fmt.Errorf("agent returned an empty session ID")
	}
	return session, nil
}

func ResumeSession(ctx context.Context, connection *acpv2.Connection, initialization acpv2.Initialization, request schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	var result schema.ResumeSessionResponse
	if err := validateSessionCapabilities(initialization); err != nil {
		return result, err
	}
	directories := make([]string, len(request.AdditionalDirectories))
	for i, directory := range request.AdditionalDirectories {
		directories[i] = string(directory)
	}
	if err := validatePaths(initialization, string(request.Cwd), directories); err != nil {
		return result, err
	}
	if request.SessionID == "" {
		return result, fmt.Errorf("session ID is required")
	}
	if err := validateServers(initialization, request.MCPServers); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.SessionResumeMethodName, request, &result)
}

func validateSessionCapabilities(initialization acpv2.Initialization) error {
	if initialization.Capabilities == nil || initialization.Capabilities.Session == nil {
		return fmt.Errorf("agent did not advertise the ACP v2 session method surface")
	}
	return nil
}

func validatePaths(initialization acpv2.Initialization, directory string, additional []string) error {
	if !filepath.IsAbs(directory) {
		return fmt.Errorf("ACP path must be absolute: %q", directory)
	}
	if len(additional) > 0 {
		session := initialization.Capabilities.Session
		if session == nil || session.AdditionalDirectories == nil {
			return fmt.Errorf("agent did not advertise additionalDirectories support")
		}
	}
	for _, path := range additional {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("ACP path must be absolute: %q", path)
		}
	}
	return nil
}

func validateServers(initialization acpv2.Initialization, servers []schema.McpServer) error {
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
			if !filepath.IsAbs(string(server.Stdio.Command)) {
				return fmt.Errorf("stdio MCP server %d command path must be absolute", i)
			}
		case server.HTTP != nil:
			if capabilities == nil || capabilities.HTTP == nil {
				return fmt.Errorf("agent did not advertise HTTP MCP server support (server %d)", i)
			}
			if server.HTTP.Name == "" || server.HTTP.URL == "" {
				return fmt.Errorf("HTTP MCP server %d requires a name and URL", i)
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
