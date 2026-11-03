package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	v1mcp "github.com/BrokkAi/acp-go/mcp"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	"github.com/BrokkAi/acp-go/v2/mcp"
)

// A server is defined once and attaches to v1 and v2 sessions alike.
var _ *v1mcp.Server = (*mcp.Server)(nil)

type v2Host struct {
	router *mcp.MessageRouter
	agent  *acp.Connection
	caller *mcp.MessageClient
	setups chan json.RawMessage
}

func newV2Host(t *testing.T) *v2Host {
	t.Helper()
	pair := acptest.NewPairWithDeadline(time.Now().Add(10 * time.Second))
	h := &v2Host{setups: make(chan json.RawMessage, 4)}
	ready := make(chan struct{})
	client := acpv2.Connect(pair.A, pair.A, func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		<-ready
		return h.router.Handle(nil)(ctx, method, raw)
	}, nil)
	h.router = mcp.NewMessageRouter(client)
	h.agent = acp.Connect(pair.B, pair.B, agentSide(func(_ context.Context, _ string, raw json.RawMessage) (any, error) {
		h.setups <- append(json.RawMessage(nil), raw...)
		return map[string]any{"sessionId": "v2-session"}, nil
	}), func(method string, raw json.RawMessage) error {
		<-ready
		return h.caller.Notifications(nil)(method, raw)
	}, acp.WithProtocolVersion(2))
	h.caller = mcp.NewMessageClient(h.agent)
	close(ready)
	initializeClient(t, client)
	t.Cleanup(func() {
		_ = h.agent.Close()
		_ = client.Close()
		pair.Close()
	})
	return h
}

func acpInitialization() schema.InitializeResponse {
	return schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{Session: &schema.SessionCapabilities{
		MCP: &schema.McpCapabilities{ACP: &schema.McpAcpCapabilities{}, HTTP: &schema.McpHttpCapabilities{}},
	}}}
}

func toolServer(t *testing.T, name string) *mcp.Server {
	t.Helper()
	registry := mcp.NewToolRegistry()
	if err := registry.Register(mcp.NewTool("echo", "Echo", nil, func(_ context.Context, input map[string]any, _ *mcp.RequestContext) (map[string]any, error) {
		return input, nil
	})); err != nil {
		t.Fatal(err)
	}
	return mcp.NewToolServer(name, "2.0.0", registry)
}

func modernParams(params map[string]any) map[string]any {
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	return params
}

func lastDeclaration(t *testing.T, params json.RawMessage) map[string]any {
	t.Helper()
	var request struct {
		MCPServers []map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(params, &request); err != nil || len(request.MCPServers) == 0 {
		t.Fatalf("params = %s, %v", params, err)
	}
	return request.MCPServers[len(request.MCPServers)-1]
}

func TestV2AttachedSessionsDeclareAndServe(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	h := newV2Host(t)
	session, attachment, err := mcp.NewAttachedSession(ctx, h.router, acpInitialization(), schema.NewSessionRequest{Cwd: schema.AbsolutePath(directory)}, toolServer(t, "tools"))
	if err != nil {
		t.Fatal(err)
	}
	declared := lastDeclaration(t, <-h.setups)
	serverID := attachment.ServerIDs()[0]
	if session.SessionID != "v2-session" || declared["type"] != "acp" || declared["serverId"] != serverID {
		t.Fatalf("session %+v declared %v", session, declared)
	}
	outcome, err := h.caller.Call(ctx, serverID, "call", "tools/call", modernParams(map[string]any{"name": "echo", "arguments": map[string]any{"x": 1}}), nil)
	if err != nil || !strings.Contains(string(outcome.Result), `"isError":false`) {
		t.Fatalf("call = %s, %+v, %v", outcome.Result, outcome.Error, err)
	}

	_, resumed, err := mcp.ResumeAttachedSession(ctx, h.router, acpInitialization(), schema.ResumeSessionRequest{SessionID: "v2-session", Cwd: schema.AbsolutePath(directory)}, toolServer(t, "tools"))
	if err != nil {
		t.Fatal(err)
	}
	if lastDeclaration(t, <-h.setups)["serverId"] != resumed.ServerIDs()[0] || resumed.ServerIDs()[0] == serverID {
		t.Fatal("resume must declare a fresh server ID")
	}
	forkInitialization := acpInitialization()
	forkInitialization.Capabilities.Session.Fork = &schema.SessionForkCapabilities{}
	if _, _, err := mcp.ForkAttachedSession(ctx, h.router, acpInitialization(), schema.ForkSessionRequest{SessionID: "v2-session", Cwd: schema.AbsolutePath(directory)}, toolServer(t, "tools")); err == nil {
		t.Fatal("fork without the fork capability was sent")
	}
	forked, forkAttachment, err := mcp.ForkAttachedSession(ctx, h.router, forkInitialization, schema.ForkSessionRequest{SessionID: "v2-session", Cwd: schema.AbsolutePath(directory)}, toolServer(t, "tools"))
	if err != nil {
		t.Fatal(err)
	}
	if forked.SessionID != "v2-session" || lastDeclaration(t, <-h.setups)["serverId"] != forkAttachment.ServerIDs()[0] {
		t.Fatalf("fork = %+v", forked)
	}
	for _, closing := range []*mcp.Attachment{attachment, resumed, forkAttachment} {
		if err := closing.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	_, err = h.caller.Call(ctx, serverID, "late", "tools/list", modernParams(map[string]any{}), nil)
	var rpcErr *acp.RPCError
	if err == nil || !asRPC(err, &rpcErr) || rpcErr.Code != mcp.ErrorCodeServerUnavailable {
		t.Fatalf("late call = %v", err)
	}
}

func TestV2AttachedSessionRejectsNameCollisionBeforeWriting(t *testing.T) {
	h := newV2Host(t)
	request := schema.NewSessionRequest{Cwd: schema.AbsolutePath(t.TempDir()), MCPServers: []schema.McpServer{{
		HTTP: &schema.McpServerHttp{Name: "tools", URL: "https://example.com/mcp"},
	}}}
	_, attachment, err := mcp.NewAttachedSession(context.Background(), h.router, acpInitialization(), request, toolServer(t, "tools"))
	var rpcErr *acp.RPCError
	if attachment != nil || !asRPC(err, &rpcErr) || rpcErr.Code != -32602 || rpcErr.Message != "Invalid params" ||
		string(rpcErr.Data) != "\"MCP server name `tools` is already declared; names must be unique within mcpServers\"" {
		t.Fatalf("collision = %v, %v", attachment, err)
	}
	select {
	case params := <-h.setups:
		t.Fatalf("collision wrote %s", params)
	default:
	}
	_, _, err = mcp.NewAttachedSession(context.Background(), h.router, schema.InitializeResponse{}, schema.NewSessionRequest{Cwd: schema.AbsolutePath(t.TempDir())}, toolServer(t, "tools"))
	if err == nil {
		t.Fatal("missing session capabilities were accepted")
	}
}

func TestV2HandleSessionSetupIsGlobalAttachment(t *testing.T) {
	router := mcp.NewMessageRouter(nil)
	attachment, err := router.Attach(toolServer(t, "global"))
	if err != nil {
		t.Fatal(err)
	}
	var forwarded json.RawMessage
	calls := 0
	handler := attachment.HandleSessionSetup(func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		calls++
		forwarded = params
		return nil, nil
	})
	params := json.RawMessage(`{"sessionId":"s","cwd":"/w","_futureSessionField":{"must":["remain"]},"mcpServers":[{"type":"_future_transport","name":"other","futureOption":{"nested":true}}]}`)
	for _, method := range []string{"session/new", "session/resume", "session/fork"} {
		if _, err := handler(context.Background(), method, params); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		var got map[string]json.RawMessage
		_ = json.Unmarshal(forwarded, &got)
		if string(got["_futureSessionField"]) != `{"must":["remain"]}` || !strings.Contains(string(got["mcpServers"]), `"futureOption":{"nested":true}`) {
			t.Fatalf("%s: params changed: %s", method, forwarded)
		}
		if lastDeclaration(t, forwarded)["serverId"] != attachment.ServerIDs()[0] {
			t.Fatalf("%s: %s", method, forwarded)
		}
	}
	calls = 0
	collision := json.RawMessage(`{"cwd":"/w","mcpServers":[{"type":"http","name":"global","url":"https://example.com"}]}`)
	_, err = handler(context.Background(), "session/new", collision)
	var rpcErr *acp.RPCError
	if !asRPC(err, &rpcErr) || rpcErr.Code != -32602 || calls != 0 ||
		string(rpcErr.Data) != "\"MCP server `global` is configured by the proxy, so mcpServers cannot declare another server with that name; names must be unique within mcpServers\"" {
		t.Fatalf("collision = %v, forwarded %d", err, calls)
	}
}

func asRPC(err error, target **acp.RPCError) bool {
	return errors.As(err, target)
}
