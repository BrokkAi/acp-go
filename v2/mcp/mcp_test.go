package mcp

import (
	"testing"

	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

func initialization(mcp bool) acpv2.Initialization {
	result := acpv2.Initialization{Capabilities: &schema.AgentCapabilities{
		Session: &schema.SessionCapabilities{},
	}}
	if mcp {
		result.Capabilities.Session.MCP = &schema.McpCapabilities{
			Stdio: &schema.McpStdioCapabilities{},
			HTTP:  &schema.McpHttpCapabilities{},
		}
	}
	return result
}

func TestValidateServersRequiresAdvertisedTransports(t *testing.T) {
	server := NewHTTPServer("remote", "https://example.test", nil)
	if err := validateServers(initialization(false), []schema.McpServer{server}); err == nil {
		t.Fatal("HTTP server accepted without capability")
	}
	withCapabilities := initialization(true).Capabilities.Session.MCP
	withCapabilities.HTTP = nil
	if err := validateServers(acpv2.Initialization{Capabilities: &schema.AgentCapabilities{
		Session: &schema.SessionCapabilities{MCP: withCapabilities},
	}}, []schema.McpServer{server}); err == nil {
		t.Fatal("HTTP server accepted without HTTP capability")
	}
	if err := validateServers(initialization(true), []schema.McpServer{server}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateServersRejectsIncompleteStdioConfiguration(t *testing.T) {
	if err := validateServers(initialization(true), []schema.McpServer{
		NewStdioServer("", "/opt/mcp", nil, nil),
	}); err == nil {
		t.Fatal("empty name accepted")
	}
	if err := validateServers(initialization(true), []schema.McpServer{
		NewStdioServer("local", "mcp", nil, nil),
	}); err == nil {
		t.Fatal("relative command accepted")
	}
}

func TestValidatePathsGatesAdditionalDirectories(t *testing.T) {
	err := validatePaths(initialization(true), "/repo", []string{"/additional"})
	if err == nil || err.Error() != "agent did not advertise additionalDirectories support" {
		t.Fatalf("additional directories error = %v", err)
	}
	withAdditional := initialization(true)
	withAdditional.Capabilities.Session.AdditionalDirectories = &schema.SessionAdditionalDirectoriesCapabilities{}
	if err := validatePaths(withAdditional, "/repo", []string{"/additional"}); err != nil {
		t.Fatal(err)
	}
}
