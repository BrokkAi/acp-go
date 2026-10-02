package cookbook_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	"github.com/BrokkAi/acp-go/mcp"
	"github.com/BrokkAi/acp-go/schema"
	"github.com/BrokkAi/acp-go/schema/unstable"
)

// initializeForMCP negotiates ACP v1 and returns the result decoded twice. The
// stable facade decodes only stable fields, and the unstable MCP capability
// flags that the mcp package checks (acp, http, sse) are not all among them,
// so the raw result is captured once and decoded into both shapes. It replaces
// InitializeWithInfo for this connection: never call both on one connection.
func initializeForMCP(ctx context.Context, connection *acp.Connection, info acp.ClientInfo) (acp.Initialization, unstable.InitializeResponse, error) {
	var stable acp.Initialization
	var optional unstable.InitializeResponse
	var encoded json.RawMessage
	if err := connection.Call(ctx, schema.InitializeMethodName, schema.InitializeRequest{
		ProtocolVersion:    acp.Version,
		ClientCapabilities: &acp.Capabilities{},
		ClientInfo:         &info,
	}, &encoded); err != nil {
		return stable, optional, err
	}
	if err := json.Unmarshal(encoded, &stable); err != nil {
		return stable, optional, err
	}
	if err := json.Unmarshal(encoded, &optional); err != nil {
		return stable, optional, err
	}
	if stable.ProtocolVersion != acp.Version {
		// Like InitializeWithInfo, refuse to keep a connection whose version
		// this client does not speak.
		_ = connection.Close()
		return stable, optional, fmt.Errorf("agent selected unsupported ACP version %d", stable.ProtocolVersion)
	}
	return stable, optional, nil
}

// mcpDeclarationAgent is a raw v1 agent handler that advertises the native and
// HTTP MCP transports and reports the servers each session/new declares. It is
// raw because the stable agent runtime cannot advertise unstable capabilities.
func mcpDeclarationAgent(declared chan<- []unstable.McpServer) acp.Handler {
	enabled := true
	return func(_ context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case unstable.InitializeMethodName:
			return unstable.InitializeResponse{
				ProtocolVersion: 1,
				AgentCapabilities: &unstable.AgentCapabilities{
					MCPCapabilities: &unstable.McpCapabilities{ACP: &enabled, HTTP: &enabled},
				},
			}, nil
		case unstable.SessionNewMethodName:
			var request unstable.NewSessionRequest
			if err := json.Unmarshal(params, &request); err != nil {
				return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
			}
			declared <- request.MCPServers
			return unstable.NewSessionResponse{SessionID: "tools-session"}, nil
		default:
			return nil, &acp.RPCError{Code: -32601, Message: "method not found: " + method}
		}
	}
}

func describeServers(servers []unstable.McpServer) string {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		switch {
		case server.Stdio != nil:
			names = append(names, server.Stdio.Name+" (stdio)")
		case server.HTTP != nil:
			names = append(names, server.HTTP.Name+" (http)")
		case server.SSE != nil:
			names = append(names, server.SSE.Name+" (sse)")
		case server.ACP != nil:
			names = append(names, server.ACP.Name+" (acp)")
		}
	}
	return strings.Join(names, ", ")
}

func Example_mcpServers() {
	ctx := context.Background()
	link := acptest.NewPair()
	declared := make(chan []unstable.McpServer, 2)
	agentConnection := link.B.Connect(mcpDeclarationAgent(declared), nil)
	defer agentConnection.Close()

	connection := acp.Connect(link.A, link.A, nil, nil)
	defer connection.Close()
	initialization, optional, err := initializeForMCP(ctx, connection, acp.ClientInfo{
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

	// Stable v1 needs no opt-in: stdio servers are baseline, and HTTP and SSE
	// servers are checked against the agent's mcpCapabilities before sending.
	if _, err := connection.NewSessionWithOptions(ctx, initialization, directory, acp.NewSessionOptions{
		MCPServers: []schema.McpServer{
			acp.NewStdioMCPServer("docs", "/usr/local/bin/docs-mcp", []string{"--stdio"}, nil),
			acp.NewHTTPMCPServer("tickets", "https://mcp.example.com/tickets"),
		},
	}); err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("stable session:", describeServers(<-declared))

	// Importing mcp opts in to the unstable transports. A native acp server
	// asks the agent to reach the MCP server over this ACP connection with
	// the request-scoped mcp/message binding. acp-go does not serve that
	// binding yet (#39), so an application that declares one answers it in
	// its own acp.Handler.
	if _, err := mcp.NewSession(ctx, connection, optional, directory, mcp.NewSessionOptions{
		Servers: []unstable.McpServer{{ACP: &unstable.McpServerAcp{Name: "workspace-tools", ServerID: "tools-1"}}},
	}); err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println("opt-in session:", describeServers(<-declared))

	// Transport gating happens before anything is written to the wire.
	_, err = mcp.NewSession(ctx, connection, optional, directory, mcp.NewSessionOptions{
		Servers: []unstable.McpServer{{SSE: &unstable.McpServerSse{Name: "legacy", URL: "https://mcp.example.com/sse"}}},
	})
	fmt.Println("refused:", err)
	// Output:
	// stable session: docs (stdio), tickets (http)
	// opt-in session: workspace-tools (acp)
	// refused: agent did not advertise SSE MCP server support (server 0)
}
