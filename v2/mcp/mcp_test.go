package mcp

import (
	"testing"

	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
)

func initialization(mcp bool) schema.InitializeResponse {
	result := schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{
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
	if err := validateServers(schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{
		Session: &schema.SessionCapabilities{MCP: withCapabilities},
	}}, []schema.McpServer{server}); err == nil {
		t.Fatal("HTTP server accepted without HTTP capability")
	}
	if err := validateServers(initialization(true), []schema.McpServer{server}); err != nil {
		t.Fatal(err)
	}
	native := schema.McpServer{ACP: &schema.McpServerAcp{Name: "tools", ServerID: "tools-1"}}
	if err := validateServers(initialization(true), []schema.McpServer{native}); err == nil {
		t.Fatal("acp server accepted without the acp capability")
	}
	withACP := initialization(true)
	withACP.Capabilities.Session.MCP.ACP = &schema.McpAcpCapabilities{}
	if err := validateServers(withACP, []schema.McpServer{native}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateServersRejectsIncompleteStdioConfiguration(t *testing.T) {
	if err := validateServers(initialization(true), []schema.McpServer{
		NewStdioServer("", hostRoot+"/opt/mcp", nil, nil),
	}); err == nil {
		t.Fatal("empty name accepted")
	}
	if err := validateServers(initialization(true), []schema.McpServer{
		NewStdioServer("local", "mcp", nil, nil),
	}); err == nil {
		t.Fatal("relative command accepted")
	}
}

func TestValidateServersAcceptsAbsoluteCommandsFromEitherPlatform(t *testing.T) {
	if err := validateServers(initialization(true), []schema.McpServer{
		NewStdioServer("local", "/opt/mcp", nil, nil),
		NewStdioServer("windows", `C:\tools\mcp.exe`, nil, nil),
	}); err != nil {
		t.Fatal(err)
	}
}
