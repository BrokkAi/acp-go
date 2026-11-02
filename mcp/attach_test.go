package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	"github.com/BrokkAi/acp-go/mcp"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

// attachHost wires a client connection that hosts MCP servers through a
// MessageRouter to a fake agent that answers session setup and calls the
// declared servers back over mcp/message.
type attachHost struct {
	router  *mcp.MessageRouter
	client  *acp.Connection
	agent   *acp.Connection
	caller  *mcp.MessageClient
	pair    *acptest.Pair
	setups  chan setupRequest
	onSetup func(ctx context.Context, method string, params json.RawMessage) (any, error)
}

type setupRequest struct {
	method string
	params json.RawMessage
}

func newAttachHost(t *testing.T) *attachHost {
	t.Helper()
	h := &attachHost{
		pair:   acptest.NewPairWithDeadline(time.Now().Add(10 * time.Second)),
		setups: make(chan setupRequest, 8),
	}
	var mu sync.Mutex
	ready := make(chan struct{})
	h.client = h.pair.A.Connect(func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		<-ready
		return h.router.Handle(nil)(ctx, method, raw)
	}, nil)
	h.router = mcp.NewMessageRouter(h.client)
	h.agent = h.pair.B.Connect(func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		<-ready
		h.setups <- setupRequest{method: method, params: append(json.RawMessage(nil), raw...)}
		mu.Lock()
		hook := h.onSetup
		mu.Unlock()
		if hook != nil {
			return hook(ctx, method, raw)
		}
		return map[string]any{"sessionId": "session-1"}, nil
	}, func(method string, raw json.RawMessage) error {
		<-ready
		return h.caller.Notifications(nil)(method, raw)
	})
	h.caller = mcp.NewMessageClient(h.agent)
	close(ready)
	t.Cleanup(func() {
		_ = h.agent.Close()
		_ = h.client.Close()
		h.pair.Close()
	})
	return h
}

func modernParams(params map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	return params
}

type echoInput struct {
	Message string `json:"message"`
}

var echoSchema = json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"],"additionalProperties":false}`)

func echoServer(t *testing.T) *mcp.Server {
	t.Helper()
	registry := mcp.NewToolRegistry()
	if err := registry.Register(mcp.NewTool("echo", "Echo a message", echoSchema, func(_ context.Context, input echoInput, _ *mcp.RequestContext) (string, error) {
		return input.Message, nil
	})); err != nil {
		t.Fatal(err)
	}
	return mcp.NewToolServer("tools", "", registry)
}

func declaredServer(t *testing.T, params json.RawMessage, index int) map[string]any {
	t.Helper()
	var request struct {
		MCPServers []map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(params, &request); err != nil {
		t.Fatal(err)
	}
	if index >= len(request.MCPServers) {
		t.Fatalf("mcpServers = %v, want index %d", request.MCPServers, index)
	}
	return request.MCPServers[index]
}

func TestNewAttachedSessionDeclaresAndServesBeforeAndAfterSetup(t *testing.T) {
	h := newAttachHost(t)
	ctx := context.Background()
	// The agent calls the declared server before answering session/new.
	h.onSetup = func(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
		server := declaredServer(t, raw, 1)
		outcome, err := h.caller.Call(ctx, server["serverId"].(string), "during-setup", "tools/call",
			modernParams(map[string]any{"name": "echo", "arguments": map[string]any{"message": "early"}}), nil)
		if err != nil || outcome.IsError() {
			t.Errorf("call during setup = %+v, %v", outcome, err)
		}
		return map[string]any{"sessionId": "session-1"}, nil
	}
	session, attachment, err := mcp.NewAttachedSession(ctx, h.router, acpCapableInitialization(), t.TempDir(), mcp.NewSessionOptions{
		Servers: []schema.McpServer{{Stdio: &schema.McpServerStdio{Name: "docs", Command: "npx"}}},
	}, echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	if session.SessionID != "session-1" {
		t.Fatalf("session = %+v", session)
	}
	setup := <-h.setups
	declared := declaredServer(t, setup.params, 1)
	serverID, _ := declared["serverId"].(string)
	if declared["type"] != "acp" || declared["name"] != "tools" || !strings.HasPrefix(serverID, "mcp-server:") {
		t.Fatalf("declaration = %v", declared)
	}
	if ids := attachment.ServerIDs(); len(ids) != 1 || ids[0] != serverID {
		t.Fatalf("server IDs = %v, declared %s", ids, serverID)
	}

	outcome, err := h.caller.Call(ctx, serverID, "after-setup", "tools/call",
		modernParams(map[string]any{"name": "echo", "arguments": map[string]any{"message": "hello"}}), nil)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(outcome.Result, &result); err != nil {
		t.Fatal(err)
	}
	content := result["content"].([]any)[0].(map[string]any)
	if result["resultType"] != "complete" || result["isError"] != false || content["text"] != "hello" {
		t.Fatalf("result = %s", outcome.Result)
	}

	if err := attachment.Close(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = h.caller.Call(ctx, serverID, "after-close", "tools/list", modernParams(nil), nil)
	assertRPCError(t, err, mcp.ErrorCodeServerUnavailable)
}

func TestAttachedSessionFailureReleasesRegistrations(t *testing.T) {
	ctx := context.Background()
	t.Run("agent error", func(t *testing.T) {
		h := newAttachHost(t)
		h.onSetup = func(context.Context, string, json.RawMessage) (any, error) {
			return nil, &acp.RPCError{Code: -32603, Message: "boom"}
		}
		_, attachment, err := mcp.NewAttachedSession(ctx, h.router, acpCapableInitialization(), t.TempDir(), mcp.NewSessionOptions{}, echoServer(t))
		if err == nil || attachment != nil {
			t.Fatalf("attachment = %v, err = %v", attachment, err)
		}
		serverID := declaredServer(t, (<-h.setups).params, 0)["serverId"].(string)
		_, err = h.caller.Call(ctx, serverID, "late", "tools/list", modernParams(nil), nil)
		assertRPCError(t, err, mcp.ErrorCodeServerUnavailable)
	})
	t.Run("missing capability is refused before writing", func(t *testing.T) {
		h := newAttachHost(t)
		_, attachment, err := mcp.NewAttachedSession(ctx, h.router, schema.InitializeResponse{}, t.TempDir(), mcp.NewSessionOptions{}, echoServer(t))
		if err == nil || !strings.Contains(err.Error(), "native MCP-over-ACP") || attachment != nil {
			t.Fatalf("attachment = %v, err = %v", attachment, err)
		}
		select {
		case setup := <-h.setups:
			t.Fatalf("validation wrote %s", setup.method)
		default:
		}
	})
	t.Run("invalid server", func(t *testing.T) {
		h := newAttachHost(t)
		if _, _, err := mcp.NewAttachedSession(ctx, h.router, acpCapableInitialization(), t.TempDir(), mcp.NewSessionOptions{}, mcp.NewServer("", nil)); err == nil {
			t.Fatal("invalid server attached")
		}
	})
}

func TestAttachedSetupMethodsDeclareServers(t *testing.T) {
	ctx := context.Background()
	initialization := acpCapableInitialization()
	initialization.AgentCapabilities.LoadSession = boolPtr(true)
	initialization.AgentCapabilities.SessionCapabilities = &schema.SessionCapabilities{
		Resume: &schema.SessionResumeCapabilities{},
		Fork:   &schema.SessionForkCapabilities{},
	}
	directory := t.TempDir()
	cases := map[string]func(h *attachHost) (*mcp.Attachment, error){
		schema.SessionLoadMethodName: func(h *attachHost) (*mcp.Attachment, error) {
			_, attachment, err := mcp.LoadAttachedSession(ctx, h.router, initialization, schema.LoadSessionRequest{SessionID: "s", Cwd: directory}, echoServer(t))
			return attachment, err
		},
		schema.SessionResumeMethodName: func(h *attachHost) (*mcp.Attachment, error) {
			_, attachment, err := mcp.ResumeAttachedSession(ctx, h.router, initialization, schema.ResumeSessionRequest{SessionID: "s", Cwd: directory}, echoServer(t))
			return attachment, err
		},
		schema.SessionForkMethodName: func(h *attachHost) (*mcp.Attachment, error) {
			_, attachment, err := mcp.ForkAttachedSession(ctx, h.router, initialization, schema.ForkSessionRequest{SessionID: "s", Cwd: directory}, echoServer(t))
			return attachment, err
		},
	}
	for method, attach := range cases {
		t.Run(method, func(t *testing.T) {
			h := newAttachHost(t)
			attachment, err := attach(h)
			if err != nil {
				t.Fatal(err)
			}
			setup := <-h.setups
			declared := declaredServer(t, setup.params, 0)
			if setup.method != method || declared["serverId"] != attachment.ServerIDs()[0] {
				t.Fatalf("setup %s declared %v", setup.method, declared)
			}
			if err := attachment.Close(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// probeService reports each operation's context and blocks until released.
type probeService struct {
	started chan *mcp.RequestContext
	release chan struct{}
	causes  chan error
	once    sync.Once
}

func newProbeService(t *testing.T) *probeService {
	probe := &probeService{started: make(chan *mcp.RequestContext, 4), release: make(chan struct{}), causes: make(chan error, 4)}
	t.Cleanup(probe.finish)
	return probe
}

// finish releases every blocked operation.
func (p *probeService) finish() { p.once.Do(func() { close(p.release) }) }

func (p *probeService) Execute(ctx context.Context, request mcp.Request, rc *mcp.RequestContext) (mcp.Outcome, error) {
	p.started <- rc
	select {
	case <-ctx.Done():
		p.causes <- context.Cause(ctx)
		// Owned cleanup finishes before the operation returns.
		<-p.release
		return mcp.Outcome{}, ctx.Err()
	case <-p.release:
		return mcp.ResultOutcome(map[string]any{"method": request.Method})
	}
}

func attachProbe(t *testing.T, h *attachHost) (*probeService, *mcp.Attachment, string) {
	t.Helper()
	probe := newProbeService(t)
	attachment, err := h.router.Attach(mcp.NewServer("probe", probe))
	if err != nil {
		t.Fatal(err)
	}
	return probe, attachment, attachment.ServerIDs()[0]
}

func TestAttachedOperationCancellationAndDuplicateIDs(t *testing.T) {
	h := newAttachHost(t)
	probe, _, serverID := attachProbe(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.caller.Call(ctx, serverID, "slow", "tools/call", modernParams(nil), nil)
		done <- err
	}()
	<-probe.started
	// A second caller reusing the active logical ID is refused by the router.
	var response schema.MessageMcpResponse
	err := h.agent.Call(context.Background(), schema.McpMessageMethodName, schema.MessageMcpRequest{
		ServerID: schema.McpServerAcpId(serverID), RequestID: "slow", Method: "tools/call", Params: modernParams(nil),
	}, &response)
	assertRPCError(t, err, -32602)

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	if cause := <-probe.causes; !errors.Is(cause, mcp.ErrRequestCancelled) {
		t.Fatalf("cause = %v", cause)
	}
	probe.finish()
	// The ID is reusable once the cancelled operation returned.
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := h.caller.Call(context.Background(), serverID, "slow", "tools/call", modernParams(nil), nil)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled request ID was not released: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAttachmentCloseCancelsAndJoinsActiveOperations(t *testing.T) {
	h := newAttachHost(t)
	probe, attachment, serverID := attachProbe(t, h)
	done := make(chan error, 1)
	go func() {
		_, err := h.caller.Call(context.Background(), serverID, "active", "tools/call", modernParams(nil), nil)
		done <- err
	}()
	rc := <-probe.started
	closed := make(chan error, 1)
	go func() { closed <- attachment.Close(context.Background()) }()
	if cause := <-probe.causes; !errors.Is(cause, mcp.ErrServerUnavailable) {
		t.Fatalf("cause = %v", cause)
	}
	if err := rc.Notify(context.Background(), "notifications/progress", nil); err == nil {
		t.Fatal("a removed operation sent a notification")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned before the operation finished cleanup: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	probe.finish()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	assertRPCError(t, <-done, mcp.ErrorCodeServerUnavailable)
}

func TestAttachedOperationsAreCancelledOnInputEOF(t *testing.T) {
	h := newAttachHost(t)
	probe, attachment, serverID := attachProbe(t, h)
	go func() {
		_, _ = h.caller.Call(context.Background(), serverID, "eof", "tools/call", modernParams(nil), nil)
	}()
	<-probe.started
	_ = h.agent.Close()
	select {
	case cause := <-probe.causes:
		if !errors.Is(cause, mcp.ErrConnectionClosed) {
			t.Fatalf("cause = %v", cause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("EOF did not cancel the operation")
	}
	probe.finish()
	if err := attachment.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestAttachedNotificationsAreRequestScoped(t *testing.T) {
	h := newAttachHost(t)
	var retained *mcp.RequestContext
	attachment, err := h.router.Attach(mcp.NewServer("progress", mcp.ServiceFunc(func(ctx context.Context, _ mcp.Request, rc *mcp.RequestContext) (mcp.Outcome, error) {
		retained = rc
		for i := 1; i <= 2; i++ {
			if err := rc.Notify(ctx, "notifications/progress", map[string]any{"progress": i}); err != nil {
				return mcp.Outcome{}, err
			}
		}
		return mcp.ResultOutcome(map[string]any{"done": true})
	})))
	if err != nil {
		t.Fatal(err)
	}
	var notifications []mcp.MessageNotification
	outcome, err := h.caller.Call(context.Background(), attachment.ServerIDs()[0], "progress", "tools/call", modernParams(nil), func(notification mcp.MessageNotification) error {
		notifications = append(notifications, notification)
		return nil
	})
	if err != nil || outcome.IsError() {
		t.Fatalf("outcome = %+v, %v", outcome, err)
	}
	if len(notifications) != 2 || notifications[0].RequestID != "progress" || notifications[1].Params["progress"] != float64(2) {
		t.Fatalf("notifications = %+v", notifications)
	}
	if err := retained.Notify(context.Background(), "notifications/progress", nil); err == nil {
		t.Fatal("a late notification was sent")
	}
}

func TestAttachedRequestsValidateModernMetadata(t *testing.T) {
	h := newAttachHost(t)
	attachment, err := h.router.Attach(echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	serverID := attachment.ServerIDs()[0]
	ctx := context.Background()
	cases := []struct {
		name, method string
		params       map[string]any
		code         int32
	}{
		{"no metadata", "tools/list", map[string]any{}, -32602},
		{"old version", "tools/list", map[string]any{"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2025-11-25",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		}}, -32022},
		{"capabilities not an object", "tools/list", map[string]any{"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion": "2026-07-28",
		}}, -32602},
		{"initialize", "initialize", modernParams(nil), -32601},
		{"unknown tool", "tools/call", modernParams(map[string]any{"name": "missing"}), -32602},
	}
	for i, tc := range cases {
		outcome, err := h.caller.Call(ctx, serverID, string(rune('a'+i)), tc.method, tc.params, nil)
		if err != nil {
			t.Fatalf("%s: outer error %v", tc.name, err)
		}
		if outcome.Error == nil || outcome.Error.Code != tc.code {
			t.Fatalf("%s: outcome = %+v", tc.name, outcome)
		}
	}

	outcome, err := h.caller.Call(ctx, serverID, "discover", "server/discover", modernParams(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	var discovery map[string]any
	if err := json.Unmarshal(outcome.Result, &discovery); err != nil {
		t.Fatal(err)
	}
	versions := discovery["supportedVersions"].([]any)
	info := discovery["_meta"].(map[string]any)["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if len(versions) != 1 || versions[0] != "2026-07-28" || info["name"] != "tools" || info["version"] != "0.0.0" {
		t.Fatalf("discovery = %s", outcome.Result)
	}

	outcome, err = h.caller.Call(ctx, serverID, "bad-arguments", "tools/call",
		modernParams(map[string]any{"name": "echo", "arguments": map[string]any{"wrong": 1}}), nil)
	if err != nil || outcome.IsError() {
		t.Fatalf("bad arguments = %+v, %v", outcome, err)
	}
	if !strings.Contains(string(outcome.Result), `"isError":true`) || !strings.Contains(string(outcome.Result), "message") {
		t.Fatalf("bad arguments result = %s", outcome.Result)
	}
}

func TestMessageRouterResourceLimit(t *testing.T) {
	h := newAttachHost(t)
	probe, _, serverID := attachProbe(t, h)
	h.router.SetMaxActive(1)
	go func() {
		_, _ = h.caller.Call(context.Background(), serverID, "first", "tools/call", modernParams(nil), nil)
	}()
	<-probe.started
	_, err := h.caller.Call(context.Background(), serverID, "second", "tools/call", modernParams(nil), nil)
	assertRPCError(t, err, mcp.ErrorCodeResourceLimitExceeded)
	probe.finish()
}

func TestMessageRouterNeverRebindsServerIDs(t *testing.T) {
	router := mcp.NewMessageRouter(nil)
	service := mcp.MessageServiceFunc(func(context.Context, mcp.MessageRequest) (mcp.MessageOutcome, error) {
		return mcp.MessageOutcome{}, nil
	})
	if err := router.Register("tools", service); err != nil {
		t.Fatal(err)
	}
	if err := router.UnregisterAndWait(context.Background(), "tools"); err != nil {
		t.Fatal(err)
	}
	if err := router.Register("tools", service); err == nil {
		t.Fatal("a released server ID was rebound")
	}
}

func TestHandleSessionSetupAppendsToRawParams(t *testing.T) {
	router := mcp.NewMessageRouter(nil)
	attachment, err := router.Attach(echoServer(t))
	if err != nil {
		t.Fatal(err)
	}
	var forwarded json.RawMessage
	handler := attachment.HandleSessionSetup(func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		forwarded = params
		return nil, nil
	})
	params := json.RawMessage(`{"cwd":"/w","mcpServers":[{"name":"tools","type":"http","url":"https://example.com"}],"futureField":{"kept":[1,2.50]},"_meta":{"x":true}}`)
	for _, method := range []string{"session/new", "session/load", "session/resume", "session/fork"} {
		if _, err := handler(context.Background(), method, params); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(forwarded, &got); err != nil {
			t.Fatal(err)
		}
		if string(got["futureField"]) != `{"kept":[1,2.50]}` || string(got["_meta"]) != `{"x":true}` {
			t.Fatalf("%s: unknown fields changed: %s", method, forwarded)
		}
		// v1 does not require unique names, so the colliding name is kept.
		if declared := declaredServer(t, forwarded, 1); declared["serverId"] != attachment.ServerIDs()[0] {
			t.Fatalf("%s: declaration = %v", method, declared)
		}
	}
	if _, err := handler(context.Background(), "session/prompt", params); err != nil || string(forwarded) != string(params) {
		t.Fatalf("non-setup request changed: %s, %v", forwarded, err)
	}
	if _, err := handler(context.Background(), "session/new", json.RawMessage(`[1]`)); err == nil {
		t.Fatal("malformed setup params were forwarded")
	} else {
		assertRPCError(t, err, -32602)
	}
	if err := attachment.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := handler(context.Background(), "session/new", params); err != nil || string(forwarded) != string(params) {
		t.Fatalf("closed attachment still declared: %s, %v", forwarded, err)
	}
}
