package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	"github.com/BrokkAi/acp-go/v2/agent"
	"github.com/BrokkAi/acp-go/v2/mcp"
	acpunstable "github.com/BrokkAi/acp-go/v2/unstable"
)

type forkAgent struct {
	*acptest.V2Agent
	requests chan schema.ForkSessionRequest
	forkedID schema.SessionId
}

func (a *forkAgent) ForkSession(_ context.Context, request schema.ForkSessionRequest) (schema.ForkSessionResponse, error) {
	a.requests <- request
	return schema.ForkSessionResponse{SessionID: a.forkedID}, nil
}

// forkFixture runs the v2 agent runtime over the shared v2 fake agent, with
// session/fork installed through v2/unstable, and returns an initialized v2
// client connection to it.
func forkFixture(t *testing.T, forkedID schema.SessionId) (*acpv2.Connection, chan schema.ForkSessionRequest) {
	t.Helper()
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	implementation := &forkAgent{V2Agent: &acptest.V2Agent{}, requests: make(chan schema.ForkSessionRequest, 4), forkedID: forkedID}
	runtime := agent.New(implementation)
	runtime.Extensions = acpunstable.Handle(nil, implementation)
	runtime.InitializeResult = acpunstable.Advertise(implementation)
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), pair.B, pair.B) }()
	client := acpv2.Connect(pair.A, pair.A, nil, nil)
	t.Cleanup(func() {
		_ = client.Close()
		pair.Close()
		<-done
	})
	var initialization schema.InitializeResponse
	if _, err := client.InitializeExtended(context.Background(), acpv2.Capabilities{}, acpv2.ClientInfo{Name: "client", Version: "1"}, &initialization); err != nil {
		t.Fatal(err)
	}
	if initialization.Capabilities.Session.Fork == nil {
		t.Fatal("runtime did not advertise session/fork")
	}
	return client, implementation.requests
}

func forkInitialization(transports *schema.McpCapabilities) schema.InitializeResponse {
	return schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{Session: &schema.SessionCapabilities{
		Fork:                  &schema.SessionForkCapabilities{},
		AdditionalDirectories: &schema.SessionAdditionalDirectoriesCapabilities{},
		MCP:                   transports,
	}}}
}

func allTransports() *schema.McpCapabilities {
	return &schema.McpCapabilities{
		Stdio: &schema.McpStdioCapabilities{},
		HTTP:  &schema.McpHttpCapabilities{},
		ACP:   &schema.McpAcpCapabilities{},
	}
}

func stdioServer(name, command string) schema.McpServer {
	return schema.McpServer{Stdio: &schema.McpServerStdio{Name: name, Command: schema.AbsolutePath(command)}}
}

func httpServer(name, url string) schema.McpServer {
	return schema.McpServer{HTTP: &schema.McpServerHttp{Name: name, URL: url}}
}

func acpServer(name, id string) schema.McpServer {
	return schema.McpServer{ACP: &schema.McpServerAcp{Name: name, ServerID: schema.McpServerAcpId(id)}}
}

func TestForkSessionValidatesBeforeWriting(t *testing.T) {
	ctx := context.Background()
	connection, requests := forkFixture(t, "forked")
	fork := func(servers ...schema.McpServer) schema.ForkSessionRequest {
		return schema.ForkSessionRequest{SessionID: "source", Cwd: "/repo", MCPServers: servers}
	}
	for name, test := range map[string]struct {
		initialization schema.InitializeResponse
		request        schema.ForkSessionRequest
		want           string
	}{
		"no fork capability": {schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{
			Session: &schema.SessionCapabilities{MCP: allTransports()},
		}}, fork(), "session/fork"},
		"blank session ID": {forkInitialization(allTransports()), schema.ForkSessionRequest{Cwd: "/repo"}, "session ID is required"},
		"relative cwd":     {forkInitialization(allTransports()), schema.ForkSessionRequest{SessionID: "source", Cwd: "repo"}, "must be absolute"},
		"stdio without capability": {forkInitialization(&schema.McpCapabilities{HTTP: &schema.McpHttpCapabilities{}}),
			fork(stdioServer("local", "/opt/mcp")), "stdio MCP server support"},
		"http without capability": {forkInitialization(&schema.McpCapabilities{Stdio: &schema.McpStdioCapabilities{}}),
			fork(httpServer("remote", "https://mcp.example.test")), "HTTP MCP server support"},
		"acp without capability": {forkInitialization(&schema.McpCapabilities{Stdio: &schema.McpStdioCapabilities{}, HTTP: &schema.McpHttpCapabilities{}}),
			fork(acpServer("proxy", "server-1")), "MCP-over-ACP server support"},
		"no transports advertised": {forkInitialization(nil), fork(httpServer("remote", "https://mcp.example.test")), "HTTP MCP server support"},
		"relative stdio command":   {forkInitialization(allTransports()), fork(stdioServer("local", "mcp")), "must be absolute"},
		"stdio without name":       {forkInitialization(allTransports()), fork(stdioServer("", "/opt/mcp")), "requires a name and command"},
		"http without URL":         {forkInitialization(allTransports()), fork(httpServer("remote", "")), "requires a name and URL"},
		"acp without server ID":    {forkInitialization(allTransports()), fork(acpServer("proxy", "")), "requires a name and server ID"},
		"unknown transport": {forkInitialization(allTransports()), fork(schema.McpServer{
			Other: &schema.McpServerOther{Raw: json.RawMessage(`{"type":"sse","name":"legacy","url":"https://mcp.example.test"}`)},
		}), "no supported transport variant"},
	} {
		if _, err := mcp.ForkSession(ctx, connection, test.initialization, test.request); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: error = %v, want it to mention %q", name, err, test.want)
		}
	}
	select {
	case request := <-requests:
		t.Fatalf("validation wrote a fork request to the wire: %+v", request)
	default:
	}
}

func TestForkSessionCarriesServers(t *testing.T) {
	connection, requests := forkFixture(t, "forked")
	request := schema.ForkSessionRequest{
		SessionID:             "source",
		Cwd:                   "/repo",
		AdditionalDirectories: []schema.AbsolutePath{`C:\agent\extra`},
		MCPServers: []schema.McpServer{
			stdioServer("local", `C:\tools\mcp.exe`),
			httpServer("remote", "https://mcp.example.test"),
			acpServer("proxy", "server-1"),
		},
	}
	result, err := mcp.ForkSession(context.Background(), connection, forkInitialization(allTransports()), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "forked" {
		t.Fatalf("forked session = %q", result.SessionID)
	}
	received := <-requests
	if received.SessionID != "source" || len(received.MCPServers) != 3 {
		t.Fatalf("agent received %+v", received)
	}
	if received.MCPServers[0].Stdio == nil || received.MCPServers[1].HTTP == nil || received.MCPServers[2].ACP == nil || received.MCPServers[2].ACP.ServerID != "server-1" {
		t.Fatalf("servers did not round-trip: %+v", received.MCPServers)
	}
}

func TestForkSessionRejectsBlankForkedSessionID(t *testing.T) {
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	server := acp.Connect(pair.B, pair.B, func(context.Context, string, json.RawMessage) (any, error) {
		return schema.ForkSessionResponse{}, nil
	}, nil)
	connection := acpv2.Connect(pair.A, pair.A, nil, nil)
	t.Cleanup(func() {
		_ = connection.Close()
		_ = server.Close()
		pair.Close()
	})
	_, err := mcp.ForkSession(context.Background(), connection, forkInitialization(allTransports()), schema.ForkSessionRequest{SessionID: "source", Cwd: "/repo"})
	if err == nil || !strings.Contains(err.Error(), "forked session ID is required") {
		t.Fatalf("blank forked session ID error = %v", err)
	}
}
