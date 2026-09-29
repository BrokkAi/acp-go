//go:build !windows

// The documented output names a Unix stdio command path, which the facade
// rejects as relative on Windows.

package cookbook_test

import (
	"context"
	"fmt"
	"os"

	"github.com/BrokkAi/acp-go/acptest"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	agentv2 "github.com/BrokkAi/acp-go/v2/agent"
	mcpv2 "github.com/BrokkAi/acp-go/v2/mcp"
)

// stdioToolsAgent is historyAgent advertising only stdio MCP servers and
// reporting the servers each session/new declares.
type stdioToolsAgent struct {
	*historyAgent
	declared chan []schema.McpServer
}

func (a stdioToolsAgent) Initialize(ctx context.Context, client agentv2.Client, request schema.InitializeRequest) (schema.InitializeResponse, error) {
	response, err := a.historyAgent.Initialize(ctx, client, request)
	response.Capabilities.Session.MCP = &schema.McpCapabilities{Stdio: &schema.McpStdioCapabilities{}}
	return response, err
}

func (a stdioToolsAgent) NewSession(ctx context.Context, client agentv2.Client, request schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	a.declared <- request.MCPServers
	return a.historyAgent.NewSession(ctx, client, request)
}

func Example_v2McpServers() {
	ctx := context.Background()
	declared := make(chan []schema.McpServer, 1)
	link := acptest.NewPair()
	go agentv2.New(stdioToolsAgent{historyAgent: newHistoryAgent(nil), declared: declared}).Serve(ctx, link.B, link.B)

	connection := acpv2.Connect(link.A, link.A, acpv2.HandlePermissions(nil, cancelPermissionsV2{}), nil)
	defer connection.Close()
	initialization, err := connection.InitializeWithInfo(ctx, acpv2.Capabilities{}, acpv2.ClientInfo{
		Name: "cookbook-tools", Version: "0.1.0",
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	directory, err := os.Getwd()
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	// The core v2 facade refuses MCP servers; importing v2/mcp is the explicit
	// opt-in. Every transport, stdio included, needs the agent's advertised
	// session.mcp capability, and a stdio command must be an absolute path.
	session, err := mcpv2.NewSession(ctx, connection, initialization, directory, mcpv2.NewSessionOptions{
		Servers: []schema.McpServer{mcpv2.NewStdioServer("docs", "/usr/local/bin/docs-mcp", []string{"--stdio"}, nil)},
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, server := range <-declared {
		fmt.Printf("%s: %s runs %s %v\n", session.SessionID, server.Stdio.Name, server.Stdio.Command, server.Stdio.Args)
	}

	_, err = mcpv2.NewSession(ctx, connection, initialization, directory, mcpv2.NewSessionOptions{
		Servers: []schema.McpServer{mcpv2.NewHTTPServer("tickets", "https://mcp.example.com/tickets", nil)},
	})
	fmt.Println("refused:", err)
	// Output:
	// session-1: docs runs /usr/local/bin/docs-mcp [--stdio]
	// refused: agent did not advertise HTTP MCP server support (server 0)
}
