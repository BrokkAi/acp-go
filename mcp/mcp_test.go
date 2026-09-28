package mcp_test

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/mcp"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

func fixture(t *testing.T, respond func(method string) any) (*acp.Connection, chan string) {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	connection := acp.Connect(local, local, nil, nil)
	requests := make(chan string, 8)
	go func() {
		decoder := json.NewDecoder(peer)
		encoder := json.NewEncoder(peer)
		for {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := decoder.Decode(&request); err != nil {
				return
			}
			requests <- request.Method
			result, _ := json.Marshal(respond(request.Method))
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": json.RawMessage(result)})
		}
	}()
	t.Cleanup(func() {
		_ = peer.Close()
		_ = connection.Close()
	})
	return connection, requests
}

func boolPtr(value bool) *bool { return &value }

func acpCapableInitialization() schema.InitializeResponse {
	return schema.InitializeResponse{AgentCapabilities: &schema.AgentCapabilities{
		MCPCapabilities: &schema.McpCapabilities{ACP: boolPtr(true)},
	}}
}

func TestNewSessionValidatesTransports(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	t.Run("native transport needs the capability", func(t *testing.T) {
		connection, requests := fixture(t, func(string) any { return map[string]any{"sessionId": "s"} })
		_, err := mcp.NewSession(ctx, connection, schema.InitializeResponse{}, directory, mcp.NewSessionOptions{
			Servers: []schema.McpServer{{ACP: &schema.McpServerAcp{Name: "native", ServerID: "server-1"}}},
		})
		if err == nil || !strings.Contains(err.Error(), "native MCP-over-ACP") {
			t.Fatalf("error = %v", err)
		}
		select {
		case method := <-requests:
			t.Fatalf("validation wrote %s to the wire", method)
		default:
		}
	})

	t.Run("stdio is baseline", func(t *testing.T) {
		connection, requests := fixture(t, func(string) any { return map[string]any{"sessionId": "s-1"} })
		session, err := mcp.NewSession(ctx, connection, schema.InitializeResponse{}, directory, mcp.NewSessionOptions{
			Servers: []schema.McpServer{{Stdio: &schema.McpServerStdio{Name: "docs", Command: "npx"}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if session.SessionID != "s-1" {
			t.Fatalf("session = %q", session.SessionID)
		}
		if method := <-requests; method != schema.SessionNewMethodName {
			t.Fatalf("method = %s", method)
		}
	})

	t.Run("native transport is sent", func(t *testing.T) {
		seen := make(chan string, 1)
		local, peer := net.Pipe()
		deadline := time.Now().Add(5 * time.Second)
		_ = local.SetDeadline(deadline)
		_ = peer.SetDeadline(deadline)
		connection := acp.Connect(local, local, nil, nil)
		defer func() { _ = peer.Close(); _ = connection.Close() }()
		go func() {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.NewDecoder(peer).Decode(&request); err != nil {
				return
			}
			seen <- string(request.Params)
			_ = json.NewEncoder(peer).Encode(map[string]any{
				"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"sessionId": "s-2"},
			})
		}()
		if _, err := mcp.NewSession(ctx, connection, acpCapableInitialization(), directory, mcp.NewSessionOptions{
			Servers: []schema.McpServer{{ACP: &schema.McpServerAcp{Name: "native", ServerID: "server-1"}}},
		}); err != nil {
			t.Fatal(err)
		}
		params := <-seen
		if !strings.Contains(params, `"type":"acp"`) || !strings.Contains(params, `"serverId":"server-1"`) {
			t.Fatalf("session/new params = %s", params)
		}
	})

	t.Run("relative path rejected", func(t *testing.T) {
		connection, requests := fixture(t, func(string) any { return map[string]any{"sessionId": "s"} })
		if _, err := mcp.NewSession(ctx, connection, schema.InitializeResponse{}, "relative", mcp.NewSessionOptions{}); err == nil {
			t.Fatal("relative directory was allowed")
		}
		select {
		case method := <-requests:
			t.Fatalf("validation wrote %s to the wire", method)
		default:
		}
	})
}

func TestResumeAndForkGateOnCapabilities(t *testing.T) {
	ctx := context.Background()
	connection, requests := fixture(t, func(string) any { return map[string]any{"sessionId": "s"} })
	if _, err := mcp.ResumeSession(ctx, connection, schema.InitializeResponse{}, schema.ResumeSessionRequest{Cwd: "/tmp", SessionID: "old"}); err == nil {
		t.Fatal("session/resume without the capability was allowed")
	}
	if _, err := mcp.ForkSession(ctx, connection, schema.InitializeResponse{}, schema.ForkSessionRequest{Cwd: "/tmp", SessionID: "old"}); err == nil {
		t.Fatal("session/fork without the capability was allowed")
	}
	select {
	case method := <-requests:
		t.Fatalf("gating wrote %s to the wire", method)
	default:
	}

	initialization := schema.InitializeResponse{AgentCapabilities: &schema.AgentCapabilities{
		SessionCapabilities: &schema.SessionCapabilities{
			Resume: &schema.SessionResumeCapabilities{},
			Fork:   &schema.SessionForkCapabilities{},
		},
	}}
	if _, err := mcp.ResumeSession(ctx, connection, initialization, schema.ResumeSessionRequest{Cwd: "/tmp", SessionID: "old"}); err != nil {
		t.Fatal(err)
	}
	if method := <-requests; method != schema.SessionResumeMethodName {
		t.Fatalf("method = %s", method)
	}
	if _, err := mcp.ForkSession(ctx, connection, initialization, schema.ForkSessionRequest{Cwd: "/tmp", SessionID: "old"}); err != nil {
		t.Fatal(err)
	}
	if method := <-requests; method != schema.SessionForkMethodName {
		t.Fatalf("method = %s", method)
	}
}
