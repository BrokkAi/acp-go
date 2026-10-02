package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/mcp"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

type messageFixture struct {
	client       *mcp.MessageClient
	router       *mcp.MessageRouter
	clientConn   *acp.Connection
	providerConn *acp.Connection
}

// newMessageFixture wires a consumer connection and a provider connection over
// two in-memory pipes, mirroring a proxy that declares an acp MCP server.
func newMessageFixture(t *testing.T) *messageFixture {
	t.Helper()
	c2pLocal, c2pPeer := net.Pipe()
	p2cLocal, p2cPeer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	for _, conn := range []net.Conn{c2pLocal, c2pPeer, p2cLocal, p2cPeer} {
		_ = conn.SetDeadline(deadline)
	}

	var client *mcp.MessageClient
	clientConn := acp.Connect(p2cPeer, c2pLocal, nil, func(method string, raw json.RawMessage) error {
		if client == nil {
			return nil
		}
		return client.Notifications(nil)(method, raw)
	})
	client = mcp.NewMessageClient(clientConn)

	var handler acp.Handler
	providerConn := acp.Connect(c2pPeer, p2cLocal, func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if handler == nil {
			return nil, &acp.RPCError{Code: -32601}
		}
		return handler(ctx, method, raw)
	}, nil)
	router := mcp.NewMessageRouter(providerConn)
	handler = router.Handle(nil)

	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = providerConn.Close()
		_ = c2pLocal.Close()
		_ = c2pPeer.Close()
		_ = p2cLocal.Close()
		_ = p2cPeer.Close()
	})
	return &messageFixture{client: client, router: router, clientConn: clientConn, providerConn: providerConn}
}

func (f *messageFixture) rawCall(ctx context.Context, t *testing.T, serverID, requestID, method string) (schema.MessageMcpResponse, error) {
	t.Helper()
	var response schema.MessageMcpResponse
	err := f.clientConn.Call(ctx, schema.McpMessageMethodName, schema.MessageMcpRequest{
		ServerID:  schema.McpServerAcpId(serverID),
		RequestID: schema.McpRequestId(requestID),
		Method:    method,
	}, &response)
	return response, err
}

func TestMessageCallCarriesRequestAndReturnsResult(t *testing.T) {
	fixture := newMessageFixture(t)
	requests := make(chan mcp.MessageRequest, 1)
	if err := fixture.router.Register("tools", mcp.MessageServiceFunc(func(_ context.Context, request mcp.MessageRequest) (mcp.MessageOutcome, error) {
		requests <- request
		return mcp.MessageOutcome{Result: json.RawMessage(`{"ok":true}`)}, nil
	})); err != nil {
		t.Fatal(err)
	}

	outcome, err := fixture.client.Call(context.Background(), "tools", "req-1", "tools/call", map[string]any{"name": "echo"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.IsError() {
		t.Fatalf("outcome = %+v, want a result", outcome)
	}
	if string(outcome.Result) != `{"ok":true}` {
		t.Fatalf("result = %s", outcome.Result)
	}
	request := <-requests
	if request.ServerID != "tools" || request.RequestID != "req-1" || request.Method != "tools/call" {
		t.Fatalf("request = %+v", request)
	}
	if request.Params["name"] != "echo" {
		t.Fatalf("params = %v", request.Params)
	}
}

func TestMessageCallKeepsInnerErrorsInsideTheOutcome(t *testing.T) {
	fixture := newMessageFixture(t)
	if err := fixture.router.Register("tools", mcp.MessageServiceFunc(func(context.Context, mcp.MessageRequest) (mcp.MessageOutcome, error) {
		return mcp.MessageOutcome{Error: &schema.McpError{
			Code:    -32602,
			Message: "Unknown tool",
			Data:    json.RawMessage(`{"name":"echo"}`),
		}}, nil
	})); err != nil {
		t.Fatal(err)
	}

	outcome, err := fixture.client.Call(context.Background(), "tools", "req-error", "tools/call", nil, nil)
	if err != nil {
		t.Fatalf("inner MCP error surfaced as an ACP error: %v", err)
	}
	if !outcome.IsError() {
		t.Fatalf("outcome = %+v, want an inner MCP error", outcome)
	}
	if outcome.Error.Code != -32602 || outcome.Error.Message != "Unknown tool" || string(outcome.Error.Data) != `{"name":"echo"}` {
		t.Fatalf("error = %+v", outcome.Error)
	}
}

func TestMessageNotificationsAreOrderedAndRequestScoped(t *testing.T) {
	fixture := newMessageFixture(t)
	var afterCall func(string, map[string]any) error
	if err := fixture.router.Register("tools", mcp.MessageServiceFunc(func(_ context.Context, request mcp.MessageRequest) (mcp.MessageOutcome, error) {
		afterCall = request.Notify
		if err := request.Notify("notifications/progress", map[string]any{"progress": 1}); err != nil {
			return mcp.MessageOutcome{}, err
		}
		if err := request.Notify("notifications/progress", map[string]any{"progress": 2}); err != nil {
			return mcp.MessageOutcome{}, err
		}
		return mcp.MessageOutcome{Result: json.RawMessage(`null`)}, nil
	})); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var notifications []mcp.MessageNotification
	outcome, err := fixture.client.Call(context.Background(), "tools", "req-notify", "tools/call", nil, func(notification mcp.MessageNotification) error {
		mu.Lock()
		defer mu.Unlock()
		notifications = append(notifications, notification)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(outcome.Result) != "null" {
		t.Fatalf("result = %s", outcome.Result)
	}
	mu.Lock()
	got := append([]mcp.MessageNotification(nil), notifications...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("notifications = %+v", got)
	}
	if got[0].RequestID != "req-notify" || got[0].Method != "notifications/progress" || got[1].Params["progress"] != float64(2) {
		t.Fatalf("notifications = %+v", got)
	}

	// A notification sent after completion is dropped by the consumer, and the
	// provider-side sender refuses to emit it.
	late := schema.MessageMcpNotification{
		ServerID:  schema.McpServerAcpId("tools"),
		RequestID: schema.McpRequestId("req-notify"),
		Method:    "notifications/progress",
		Params:    map[string]any{"progress": 3},
	}
	if err := fixture.providerConn.Notify(context.Background(), schema.McpMessageMethodName, late); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.rawCall(context.Background(), t, "tools", "req-notify-2", "tools/list"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	stillTwo := len(notifications)
	mu.Unlock()
	if stillTwo != 2 {
		t.Fatalf("late notification was delivered: %+v", notifications)
	}
	if err := afterCall("notifications/progress", map[string]any{"progress": 4}); err == nil {
		t.Fatal("notify after completion was accepted")
	}
}

func TestMessageRouterRejectsMalformedUnknownAndDuplicateRequests(t *testing.T) {
	fixture := newMessageFixture(t)
	ctx := context.Background()

	var response schema.MessageMcpResponse
	if err := fixture.clientConn.Call(ctx, schema.McpMessageMethodName, map[string]any{"serverId": "tools"}, &response); err == nil {
		t.Fatal("malformed envelope was accepted")
	} else {
		assertRPCError(t, err, -32602)
	}
	if _, err := fixture.rawCall(ctx, t, "missing", "req-1", "tools/list"); err == nil {
		t.Fatal("unknown server was accepted")
	} else {
		assertRPCError(t, err, -33001)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	if err := fixture.router.Register("tools", mcp.MessageServiceFunc(func(context.Context, mcp.MessageRequest) (mcp.MessageOutcome, error) {
		close(started)
		<-release
		return mcp.MessageOutcome{Result: json.RawMessage(`"first"`)}, nil
	})); err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() {
		_, err := fixture.rawCall(ctx, t, "tools", "dup", "tools/call")
		first <- err
	}()
	<-started
	if _, err := fixture.rawCall(ctx, t, "tools", "dup", "tools/call"); err == nil {
		t.Fatal("duplicate active request ID was accepted")
	} else {
		assertRPCError(t, err, -32602)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

func TestMessageRouterCancelsActiveRequestsAndReleasesIDs(t *testing.T) {
	fixture := newMessageFixture(t)
	started := make(chan struct{})
	canceled := make(chan struct{})
	var attempts int32
	if err := fixture.router.Register("tools", mcp.MessageServiceFunc(func(ctx context.Context, request mcp.MessageRequest) (mcp.MessageOutcome, error) {
		if request.RequestID == "slow" && atomic.AddInt32(&attempts, 1) == 1 {
			close(started)
			<-ctx.Done()
			close(canceled)
			return mcp.MessageOutcome{}, ctx.Err()
		}
		return mcp.MessageOutcome{Result: json.RawMessage(`"ok"`)}, nil
	})); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := fixture.client.Call(ctx, "tools", "slow", "tools/call", nil, nil)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("service did not observe cancellation")
	}

	deadline := time.Now().Add(time.Second)
	for {
		outcome, err := fixture.client.Call(context.Background(), "tools", "slow", "tools/call", nil, nil)
		if err == nil {
			if string(outcome.Result) != `"ok"` {
				t.Fatalf("result = %s", outcome.Result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled request ID was not released: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMessageRouterUnregisterCancelsAndRejectsRequests(t *testing.T) {
	fixture := newMessageFixture(t)
	started := make(chan struct{})
	canceled := make(chan struct{})
	if err := fixture.router.Register("tools", mcp.MessageServiceFunc(func(ctx context.Context, _ mcp.MessageRequest) (mcp.MessageOutcome, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return mcp.MessageOutcome{}, ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := fixture.client.Call(context.Background(), "tools", "unregister", "tools/call", nil, nil)
		done <- err
	}()
	<-started
	fixture.router.Unregister("tools")
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("unregister did not cancel the active request")
	}
	if err := <-done; err == nil {
		t.Fatal("cancelled call succeeded")
	}
	if _, err := fixture.rawCall(context.Background(), t, "tools", "after", "tools/call"); err == nil {
		t.Fatal("unregistered server was accepted")
	} else {
		assertRPCError(t, err, -33001)
	}
}

func TestMessageRouterDelegatesOtherMethods(t *testing.T) {
	router := mcp.NewMessageRouter(nil)
	next := func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		return "next:" + method, nil
	}
	value, err := router.Handle(next)(context.Background(), "session/prompt", nil)
	if err != nil || value != "next:session/prompt" {
		t.Fatalf("delegated value = %#v, %v", value, err)
	}
	if _, err := router.Handle(nil)(context.Background(), "session/prompt", nil); err == nil {
		t.Fatal("missing next answered an unrelated method")
	} else {
		assertRPCError(t, err, -32601)
	}
}

func assertRPCError(t *testing.T, err error, code int) {
	t.Helper()
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want *acp.RPCError", err)
	}
	if rpcErr.Code != code {
		t.Fatalf("error code = %d, want %d (%v)", rpcErr.Code, code, err)
	}
}
