package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

const initializeParams = `{"protocolVersion":2,"info":{"name":"c","version":"1"}}`

// TestRuntimeAnswersUnhandledEntityCancelWithInvalidParams pins the
// reference SDK's answer to tool_call/cancel and terminal/cancel on a v2
// agent without handlers (agent-client-protocol 5.0.0,
// v2_agent_answers_unhandled_entity_cancel_with_invalid_params): -32600
// before initialization, then -32602 whose data holds the request's ID when
// it is a string, while other unknown methods keep -32601.
func TestRuntimeAnswersUnhandledEntityCancelWithInvalidParams(t *testing.T) {
	w, r := serveWire(t, func(in io.ReadCloser, out io.WriteCloser) error {
		return New(&echoAgent{}).Serve(context.Background(), in, out)
	})
	before := exchange(t, w, r, 0, schema.ToolCallCancelMethodName, `{"sessionId":"s","toolCallId":"c"}`)
	if !strings.HasPrefix(before, `{"jsonrpc":"2.0","id":0,"error":{"code":-32600,`) {
		t.Fatalf("before initialization = %s", before)
	}
	if got := exchange(t, w, r, 1, schema.InitializeMethodName, initializeParams); !strings.Contains(got, `"result"`) {
		t.Fatalf("initialize answer = %s", got)
	}
	const (
		toolCall = `"code":-32602,"message":"the tool call is unknown or not cancellable"`
		terminal = `"code":-32602,"message":"the terminal is unknown or not cancellable"`
	)
	for i, tc := range []struct {
		method, params, want string
	}{
		{schema.ToolCallCancelMethodName, `{"sessionId":"s","toolCallId":"c"}`, `{` + toolCall + `,"data":{"toolCallId":"c"}}`},
		{schema.TerminalCancelMethodName, `{"sessionId":"s","terminalId":"t"}`, `{` + terminal + `,"data":{"terminalId":"t"}}`},
		// The request is not decoded: a missing session or the other
		// entity's ID does not matter, and a non-string ID gives no data.
		{schema.ToolCallCancelMethodName, `{"toolCallId":"only"}`, `{` + toolCall + `,"data":{"toolCallId":"only"}}`},
		{schema.ToolCallCancelMethodName, `{"sessionId":"s","terminalId":"t"}`, `{` + toolCall + `}`},
		{schema.TerminalCancelMethodName, `{"sessionId":"s","terminalId":7}`, `{` + terminal + `}`},
		{schema.ToolCallCancelMethodName, `[1]`, `{` + toolCall + `}`},
		{schema.ToolCallCancelMethodName, `null`, `{` + toolCall + `}`},
		{schema.TerminalCancelMethodName, ``, `{` + terminal + `}`},
		// serde_json keeps the last value of a repeated member.
		{schema.ToolCallCancelMethodName, `{"toolCallId":"a","toolCallId":"b\n\u00e9"}`, `{` + toolCall + `,"data":{"toolCallId":"b\né"}}`},
	} {
		id := i + 2
		want := `{"jsonrpc":"2.0","id":` + itoa(id) + `,"error":` + tc.want + `}`
		if got := exchange(t, w, r, id, tc.method, tc.params); got != want {
			t.Fatalf("%s %s:\n got %s\nwant %s", tc.method, tc.params, got, want)
		}
	}
	requireMethodsNotFound(t, w, r, []string{"_vendor/unknown"}, `{}`)
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

type cancellingAgent struct {
	*echoAgent
	toolCalls chan schema.CancelToolCallRequest
	terminals chan schema.CancelTerminalRequest
}

func (a cancellingAgent) CancelToolCall(_ context.Context, _ Client, request schema.CancelToolCallRequest) (schema.CancelToolCallResponse, error) {
	if request.ToolCallID != "call" {
		return schema.CancelToolCallResponse{}, ToolCallNotCancellableError(request.ToolCallID)
	}
	a.toolCalls <- request
	return schema.CancelToolCallResponse{}, nil
}

func (a cancellingAgent) CancelTerminal(_ context.Context, _ Client, request schema.CancelTerminalRequest) (schema.CancelTerminalResponse, error) {
	if request.TerminalID != "term" {
		return schema.CancelTerminalResponse{}, TerminalNotCancellableError(request.TerminalID)
	}
	a.terminals <- request
	return schema.CancelTerminalResponse{}, nil
}

// TestRuntimeDispatchesEntityCancel serves both methods through the Agent's
// handlers: decoded params reach them, malformed params answer
// json_cast_params' -32602, and the exported errors match the runtime's own.
func TestRuntimeDispatchesEntityCancel(t *testing.T) {
	implementation := cancellingAgent{
		echoAgent: &echoAgent{},
		toolCalls: make(chan schema.CancelToolCallRequest, 1),
		terminals: make(chan schema.CancelTerminalRequest, 1),
	}
	w, r := serveWire(t, func(in io.ReadCloser, out io.WriteCloser) error {
		return New(implementation).Serve(context.Background(), in, out)
	})
	if got := exchange(t, w, r, 0, schema.InitializeMethodName, initializeParams); !strings.Contains(got, `"result"`) {
		t.Fatalf("initialize answer = %s", got)
	}
	if got := exchange(t, w, r, 1, schema.ToolCallCancelMethodName, `{"sessionId":"s","toolCallId":"call"}`); got != `{"jsonrpc":"2.0","id":1,"result":{}}` {
		t.Fatalf("tool_call/cancel = %s", got)
	}
	if got := <-implementation.toolCalls; got.SessionID != "s" || got.ToolCallID != "call" {
		t.Fatalf("tool call request = %+v", got)
	}
	if got := exchange(t, w, r, 2, schema.TerminalCancelMethodName, `{"sessionId":"s","terminalId":"term"}`); got != `{"jsonrpc":"2.0","id":2,"result":{}}` {
		t.Fatalf("terminal/cancel = %s", got)
	}
	if got := <-implementation.terminals; got.SessionID != "s" || got.TerminalID != "term" {
		t.Fatalf("terminal request = %+v", got)
	}
	if got, want := exchange(t, w, r, 3, schema.ToolCallCancelMethodName, `{"sessionId":"s","toolCallId":"done"}`),
		`{"jsonrpc":"2.0","id":3,"error":{"code":-32602,"message":"the tool call is unknown or not cancellable","data":{"toolCallId":"done"}}}`; got != want {
		t.Fatalf("not cancellable:\n got %s\nwant %s", got, want)
	}
	if got, want := exchange(t, w, r, 4, schema.TerminalCancelMethodName, `{"sessionId":"s","terminalId":"gone"}`),
		`{"jsonrpc":"2.0","id":4,"error":{"code":-32602,"message":"the terminal is unknown or not cancellable","data":{"terminalId":"gone"}}}`; got != want {
		t.Fatalf("not cancellable:\n got %s\nwant %s", got, want)
	}
	for i, tc := range []paramsCase{
		{schema.ToolCallCancelMethodName, "", "invalid type: null, expected struct CancelToolCallRequest", `null`},
		{schema.ToolCallCancelMethodName, `{"sessionId":"s"}`, "missing field `toolCallId`", `{"sessionId":"s"}`},
		{schema.TerminalCancelMethodName, `null`, "invalid type: null, expected struct CancelTerminalRequest", `null`},
		{schema.TerminalCancelMethodName, `{"sessionId":"s","terminalId":5}`, "invalid type: integer `5`, expected a string", `{"sessionId":"s","terminalId":5}`},
	} {
		requireAnswer(t, exchange(t, w, r, 10+i, tc.method, tc.params), 10+i, tc)
	}
}

// TestRuntimeOverrideServesEntityCancel lets an override answer in place of
// the default -32602, as a catch-all handler does in the reference SDK.
func TestRuntimeOverrideServesEntityCancel(t *testing.T) {
	runtime := New(&echoAgent{})
	runtime.Overrides = map[string]acp.Handler{
		schema.TerminalCancelMethodName: func(context.Context, string, json.RawMessage) (any, error) {
			return map[string]bool{"caughtAll": true}, nil
		},
	}
	w, r := serveWire(t, func(in io.ReadCloser, out io.WriteCloser) error {
		return runtime.Serve(context.Background(), in, out)
	})
	if got := exchange(t, w, r, 0, schema.InitializeMethodName, initializeParams); !strings.Contains(got, `"result"`) {
		t.Fatalf("initialize answer = %s", got)
	}
	if got := exchange(t, w, r, 1, schema.TerminalCancelMethodName, `{"sessionId":"s","terminalId":"t"}`); got != `{"jsonrpc":"2.0","id":1,"result":{"caughtAll":true}}` {
		t.Fatalf("terminal/cancel = %s", got)
	}
}

// permissionAgent sends one foreground permission request through
// ForegroundPermissions and one background request directly when prompted,
// and cancels the foreground ones on session/cancel.
type permissionAgent struct {
	*echoAgent
	permissions ForegroundPermissions
	foreground  chan permissionResult
	background  chan permissionResult
}

type permissionResult struct {
	response schema.RequestPermissionResponse
	err      error
}

func permissionRequest(title string) schema.RequestPermissionRequest {
	return schema.RequestPermissionRequest{
		SessionID: "v2-session",
		Title:     title,
		Options:   []schema.PermissionOption{{OptionID: "allow", Name: "Allow", Kind: schema.PermissionOptionKindAllowOnce}},
	}
}

func (a *permissionAgent) Prompt(_ context.Context, client Client, _ schema.PromptRequest, _ SessionUpdater) (schema.PromptResponse, error) {
	go func() {
		response, err := client.RequestPermission(context.Background(), permissionRequest("background"))
		a.background <- permissionResult{response, err}
	}()
	go func() {
		response, err := a.permissions.RequestPermission(context.Background(), client, permissionRequest("foreground"))
		a.foreground <- permissionResult{response, err}
	}()
	return schema.PromptResponse{MessageID: "user"}, nil
}

func (a *permissionAgent) CancelSession(notification schema.CancelSessionNotification) error {
	a.permissions.CancelSession(notification.SessionID)
	return nil
}

// TestForegroundPermissionsCancelOnlyForegroundRequests cancels the
// foreground request with $/cancel_request on session/cancel and reports it
// as not approved, while the background request stays open and is answered
// normally.
func TestForegroundPermissionsCancelOnlyForegroundRequests(t *testing.T) {
	implementation := &permissionAgent{
		echoAgent:  &echoAgent{},
		foreground: make(chan permissionResult, 1),
		background: make(chan permissionResult, 1),
	}
	entered := make(chan string, 2)
	release := make(chan struct{})
	dismissed := make(chan string, 2)
	host := acpv2.HandlePermissions(nil, permissionFunc(func(ctx context.Context, request schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
		entered <- request.Title
		select {
		case <-ctx.Done():
			dismissed <- request.Title
			return schema.RequestPermissionResponse{}, ctx.Err()
		case <-release:
			return schema.RequestPermissionResponse{Outcome: schema.RequestPermissionOutcome{
				Selected: &schema.SelectedPermissionOutcome{OptionID: "allow"},
			}}, nil
		}
	}))

	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	go func() { _ = New(implementation).Serve(context.Background(), peer, peer) }()
	client := acpv2.Connect(local, local, host, nil)
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })

	ctx := context.Background()
	initialization, err := client.Initialize(ctx, acpv2.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSessionWithOptions(ctx, initialization, hostRoot+"/repo", acpv2.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Prompt(ctx, initialization, session, "go"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		<-entered
	}
	if err := client.CancelSession(ctx, session.SessionID); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-implementation.foreground:
		if result.err != nil || result.response.Outcome.Cancelled == nil {
			t.Fatalf("foreground permission = %+v, %v", result.response, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("foreground permission was not cancelled")
	}
	select {
	case title := <-dismissed:
		if title != "foreground" {
			t.Fatalf("dismissed %q", title)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("$/cancel_request did not dismiss the foreground prompt")
	}
	select {
	case result := <-implementation.background:
		t.Fatalf("background permission resolved on session/cancel: %+v, %v", result.response, result.err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	result := <-implementation.background
	if result.err != nil || result.response.Outcome.Selected == nil {
		t.Fatalf("background permission = %+v, %v", result.response, result.err)
	}
	if count := implementation.permissions.CancelSession(session.SessionID); count != 0 {
		t.Fatalf("finished requests are still pending: %d", count)
	}
}

// TestForegroundPermissionsKeepCallerCancellation returns the caller's own
// context error rather than a cancelled outcome.
func TestForegroundPermissionsKeepCallerCancellation(t *testing.T) {
	var permissions ForegroundPermissions
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := permissions.RequestPermission(ctx, blockingClient{}, permissionRequest("x"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

type blockingClient struct{ Client }

func (blockingClient) RequestPermission(ctx context.Context, _ schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
	<-ctx.Done()
	return schema.RequestPermissionResponse{}, ctx.Err()
}

type permissionFunc func(context.Context, schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error)

func (f permissionFunc) RequestPermission(ctx context.Context, request schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
	return f(ctx, request)
}
