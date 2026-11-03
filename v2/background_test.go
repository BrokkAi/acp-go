package v2

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

func backgroundList(work ...schema.BackgroundWork) schema.Nullable[[]schema.BackgroundWork] {
	if work == nil {
		work = []schema.BackgroundWork{}
	}
	return schema.Nullable[[]schema.BackgroundWork]{Set: true, Value: work}
}

func backgroundToolCall(id schema.ToolCallId) schema.BackgroundWork {
	return schema.BackgroundWork{ToolCall: &schema.BackgroundToolCall{ToolCallID: id}}
}

func backgroundTerminal(id schema.TerminalId) schema.BackgroundWork {
	return schema.BackgroundWork{Terminal: &schema.BackgroundTerminal{TerminalID: id}}
}

func decodeUpdate(t *testing.T, raw string) schema.SessionUpdate {
	t.Helper()
	var update schema.SessionUpdate
	if err := json.Unmarshal([]byte(raw), &update); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return update
}

func TestUpdateProjectionBackgroundSnapshots(t *testing.T) {
	projection := NewUpdateProjection()
	if _, reported := projection.Background(); reported {
		t.Fatal("background reported before any state update")
	}
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"tool_call_update","toolCallId":"watch","status":"in_progress"}`))
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"tool_call_update","toolCallId":"shell","content":[{"type":"terminal","terminalId":"server"}]}`))
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"terminal_update","terminalId":"server","command":"npm run dev"}`))
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"state_update","state":"running","background":[{"type":"tool_call","toolCallId":"watch"},{"type":"terminal","terminalId":"server"},{"type":"future","x":1}]}`))

	work, reported := projection.Background()
	if !reported || len(work) != 3 || work[0].ToolCall == nil || work[1].Terminal == nil || work[2].Other == nil {
		t.Fatalf("background = %+v, %v", work, reported)
	}
	if !projection.ToolCallInBackground("watch") || !projection.TerminalInBackground("server") {
		t.Fatal("listed entities are not in the background")
	}
	// A tool call whose live work is a listed terminal is background work.
	if !projection.ToolCallInBackground("shell") {
		t.Fatal("tool call referencing a listed terminal is not in the background")
	}
	if projection.ToolCallInBackground("other") || projection.TerminalInBackground("other") {
		t.Fatal("unlisted entities are in the background")
	}

	// An omitted list is not reported: background work becomes unknown.
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}`))
	if _, reported := projection.Background(); reported {
		t.Fatal("omitted background still reported")
	}
	if projection.ToolCallInBackground("watch") {
		t.Fatal("unknown background still lists the tool call")
	}
	// A malformed list reads as not reported too.
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"state_update","state":"idle","background":true}`))
	if _, reported := projection.Background(); reported {
		t.Fatal("malformed background reported")
	}
	// An empty list means nothing runs.
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"state_update","state":"idle","background":[]}`))
	if work, reported := projection.Background(); !reported || work == nil || len(work) != 0 {
		t.Fatalf("empty background = %#v, %v", work, reported)
	}
	// A custom state's background list is read from its payload.
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"state_update","state":"compacting","background":[{"type":"terminal","terminalId":"server"}]}`))
	if !projection.TerminalInBackground("server") {
		t.Fatal("custom state's background list was not read")
	}
	// A requires_action list replaces it.
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"state_update","state":"requires_action","background":[{"type":"tool_call","toolCallId":"watch"}]}`))
	if projection.TerminalInBackground("server") || !projection.ToolCallInBackground("watch") {
		t.Fatal("requires_action background did not replace the list")
	}
}

func TestUpdateProjectionCancelCapabilities(t *testing.T) {
	projection := NewUpdateProjection()
	if projection.CanCancelToolCall("call") || projection.CanCancelTerminal("term") {
		t.Fatal("unknown entities are cancellable")
	}
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"tool_call_update","toolCallId":"call","status":"in_progress","capabilities":{"cancel":{}}}`))
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"terminal_update","terminalId":"term","capabilities":{"cancel":{}}}`))
	if !projection.CanCancelToolCall("call") || !projection.CanCancelTerminal("term") {
		t.Fatal("advertised cancel not tracked")
	}
	// Omitted capabilities leave the stored value.
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"tool_call_update","toolCallId":"call","title":"t"}`))
	if !projection.CanCancelToolCall("call") {
		t.Fatal("omitted capabilities withdrew cancel")
	}
	// An object replaces the whole value: without cancel, it is withdrawn.
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"tool_call_update","toolCallId":"call","capabilities":{}}`))
	if projection.CanCancelToolCall("call") {
		t.Fatal("replacing capabilities kept cancel")
	}
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"tool_call_update","toolCallId":"call","capabilities":{"cancel":null}}`))
	if projection.CanCancelToolCall("call") {
		t.Fatal("null cancel is cancellable")
	}
	// null clears them.
	projection.Apply(decodeUpdate(t, `{"sessionUpdate":"terminal_update","terminalId":"term","capabilities":null}`))
	if projection.CanCancelTerminal("term") {
		t.Fatal("null capabilities kept cancel")
	}
	// A permission request's tool call patches capabilities like an update.
	projection.ApplyPermissionRequest(schema.RequestPermissionRequest{Subject: &schema.RequestPermissionSubject{
		ToolCall: &schema.ToolCallPermissionSubject{ToolCall: schema.ToolCallUpdate{
			ToolCallID:   "call",
			Capabilities: schema.Nullable[schema.ToolCallCapabilities]{Set: true, Value: schema.ToolCallCapabilities{Cancel: &schema.ToolCallCancelCapabilities{}}},
		}},
	}})
	if !projection.CanCancelToolCall("call") {
		t.Fatal("permission request capabilities not applied")
	}
}

func TestActiveWorkIdleWithBackgroundCompletes(t *testing.T) {
	work := BeginActiveWork()
	if _, reported := work.Background(); reported {
		t.Fatal("background reported before the work became active")
	}
	work.Apply(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{
		Background: backgroundList(backgroundTerminal("old")),
	}}})
	if list, reported := work.Background(); !reported || len(list) != 1 || list[0].Terminal.TerminalID != "old" {
		t.Fatalf("running background = %+v, %v", list, reported)
	}
	work.Apply(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}})
	if _, reported := work.Background(); reported {
		t.Fatal("omitted background still reported")
	}
	reason := schema.StopReasonEndTurn
	work.Apply(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{Idle: &schema.IdleStateUpdate{
		StopReason: &reason,
		Background: backgroundList(backgroundTerminal("server"), backgroundToolCall("watch")),
	}}})
	select {
	case <-work.Done():
	default:
		t.Fatal("idle with background work did not complete the foreground work")
	}
	result := work.Result()
	if !result.BackgroundReported || len(result.Background) != 2 || result.Background[0].Terminal.TerminalID != "server" ||
		result.Background[1].ToolCall.ToolCallID != "watch" || result.StopReason == nil || *result.StopReason != reason {
		t.Fatalf("result = %+v", result)
	}
	// A repeated idle snapshot with a changed list is not a new completion.
	work.Apply(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{Idle: &schema.IdleStateUpdate{Background: backgroundList()}}})
	if again := work.Result(); len(again.Background) != 2 {
		t.Fatalf("repeated idle changed the result: %+v", again)
	}

	unreported := BeginActiveWork()
	unreported.Apply(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}})
	unreported.Apply(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{Idle: &schema.IdleStateUpdate{}}})
	if result := unreported.Result(); result.BackgroundReported || result.Background != nil {
		t.Fatalf("unreported background result = %+v", result)
	}
}

// cancelPeer is an Agent stub that records the entity cancellations it
// receives, answers them, and then confirms them with final updates.
type cancelPeer struct {
	mu       sync.Mutex
	server   *acp.Connection
	requests []string
}

func (p *cancelPeer) handle(_ context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case schema.InitializeMethodName:
		return sessionInitialization(), nil
	case schema.ToolCallCancelMethodName, schema.TerminalCancelMethodName:
		p.mu.Lock()
		p.requests = append(p.requests, method+" "+string(raw))
		p.mu.Unlock()
		var confirm string
		if method == schema.ToolCallCancelMethodName {
			var request schema.CancelToolCallRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				return nil, err
			}
			confirm = `{"sessionUpdate":"tool_call_update","toolCallId":"` + string(request.ToolCallID) + `","status":"cancelled","capabilities":null}`
		} else {
			var request schema.CancelTerminalRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				return nil, err
			}
			confirm = `{"sessionUpdate":"terminal_update","terminalId":"` + string(request.TerminalID) + `","exitStatus":{"exitCode":null,"signal":"SIGTERM"},"capabilities":null}`
		}
		go func() {
			_ = p.server.Notify(context.Background(), schema.SessionUpdateMethodName,
				json.RawMessage(`{"sessionId":"s","update":`+confirm+`}`))
		}()
		return map[string]any{}, nil
	}
	return nil, &acp.RPCError{Code: -32601, Message: "Method not found"}
}

func (p *cancelPeer) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.requests...)
}

func notifyRaw(t *testing.T, server *acp.Connection, update string) {
	t.Helper()
	if err := server.Notify(context.Background(), schema.SessionUpdateMethodName, json.RawMessage(`{"sessionId":"s","update":`+update+`}`)); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSessionHandleCancelsToolCallsAndTerminals(t *testing.T) {
	tracker := NewSessionTracker()
	peer := &cancelPeer{}
	client, server := pipeClientWithServerNotifications(t, peer.handle, tracker.TrackerNotifications(nil), nil)
	peer.server = server
	ctx := context.Background()
	initialization, err := client.Initialize(ctx, Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := NewSessionHandle(client, initialization, Session{SessionID: "s"}, SessionHandleOptions{Tracker: tracker})
	if err != nil {
		t.Fatal(err)
	}
	projection := tracker.Projection("s")

	// Nothing is written for an unknown or non-cancellable entity.
	var notCancellable *NotCancellableError
	if err := handle.CancelToolCall(ctx, "call"); !errors.As(err, &notCancellable) || notCancellable.Known ||
		notCancellable.Entity != (EntityRef{Kind: EntityToolCall, ID: "call"}) || notCancellable.SessionID != "s" {
		t.Fatalf("unknown tool call = %v", err)
	}
	notifyRaw(t, server, `{"sessionUpdate":"tool_call_update","toolCallId":"call","status":"in_progress"}`)
	notifyRaw(t, server, `{"sessionUpdate":"terminal_update","terminalId":"term","command":"sleep 100"}`)
	waitFor(t, "entities", func() bool {
		_, call := projection.ToolCall("call")
		_, term := projection.Terminal("term")
		return call && term
	})
	if err := handle.CancelToolCall(ctx, "call"); !errors.As(err, &notCancellable) || !notCancellable.Known {
		t.Fatalf("non-cancellable tool call = %v", err)
	}
	if err := handle.CancelTerminal(ctx, "term"); !errors.As(err, &notCancellable) || !notCancellable.Known ||
		notCancellable.Entity.Kind != EntityTerminal {
		t.Fatalf("non-cancellable terminal = %v", err)
	}
	if seen := peer.seen(); len(seen) != 0 {
		t.Fatalf("refused cancellations reached the wire: %v", seen)
	}

	// Once advertised, each round-trips, and the final update confirms it.
	notifyRaw(t, server, `{"sessionUpdate":"tool_call_update","toolCallId":"call","capabilities":{"cancel":{}}}`)
	notifyRaw(t, server, `{"sessionUpdate":"terminal_update","terminalId":"term","capabilities":{"cancel":{}}}`)
	waitFor(t, "capabilities", func() bool {
		return projection.CanCancelToolCall("call") && projection.CanCancelTerminal("term")
	})
	if err := handle.CancelToolCall(ctx, "call"); err != nil {
		t.Fatal(err)
	}
	if err := handle.CancelTerminal(ctx, "term"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`tool_call/cancel {"sessionId":"s","toolCallId":"call"}`,
		`terminal/cancel {"sessionId":"s","terminalId":"term"}`,
	}
	if seen := peer.seen(); len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("wire = %v", seen)
	}
	waitFor(t, "confirmation", func() bool {
		call, _ := projection.ToolCall("call")
		term, _ := projection.Terminal("term")
		return call.Status.Value == schema.ToolCallStatusCancelled && term.Terminal.ExitStatus.Set
	})
	// The confirmation cleared cancel, so a repeat is refused locally.
	if err := handle.CancelToolCall(ctx, "call"); !errors.As(err, &notCancellable) {
		t.Fatalf("repeat after final status = %v", err)
	}

	// The handle needs a tracker to know the capabilities.
	bare, err := NewSessionHandle(client, initialization, Session{SessionID: "s"}, SessionHandleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := bare.CancelToolCall(ctx, "call"); err == nil {
		t.Fatal("CancelToolCall without a tracker succeeded")
	}
}

// TestCancelActiveWorkLeavesPermissionsToTheAgent sends only session/cancel:
// no pending permission request is cancelled locally, whatever its subject.
// The Agent cancels the foreground work's request with $/cancel_request,
// which CancellablePermissions answers with the cancelled outcome, and the
// request for listed background work stays open until it is answered.
func TestCancelActiveWorkLeavesPermissionsToTheAgent(t *testing.T) {
	tracker := NewSessionTracker()
	entered := make(chan string, 2)
	dismissed := make(chan string, 2)
	release := make(chan struct{})
	permissions := NewCancellablePermissions(permissionFunc(func(ctx context.Context, request schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
		id := string(request.Subject.ToolCall.ToolCall.ToolCallID)
		entered <- id
		select {
		case <-ctx.Done():
			dismissed <- id
			return schema.RequestPermissionResponse{}, ctx.Err()
		case <-release:
			return schema.RequestPermissionResponse{Outcome: schema.RequestPermissionOutcome{
				Selected: &schema.SelectedPermissionOutcome{OptionID: "allow"},
			}}, nil
		}
	}))
	cancels := make(chan struct{}, 1)
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	server := acp.Connect(peer, peer, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == schema.InitializeMethodName {
			return sessionInitialization(), nil
		}
		return nil, &acp.RPCError{Code: -32601, Message: "Method not found"}
	}, func(method string, _ json.RawMessage) error {
		if method == schema.SessionCancelMethodName {
			cancels <- struct{}{}
		}
		return nil
	})
	client := Connect(local, local, HandlePermissions(nil, permissions), tracker.TrackerNotifications(nil),
		acp.WithRequestObserver(tracker.TrackerRequests(nil)))
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		_ = peer.Close()
	})
	ctx := context.Background()
	initialization, err := client.Initialize(ctx, Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := NewSessionHandle(client, initialization, Session{SessionID: "s"}, SessionHandleOptions{Tracker: tracker})
	if err != nil {
		t.Fatal(err)
	}
	notifyRaw(t, server, `{"sessionUpdate":"state_update","state":"running","background":[{"type":"tool_call","toolCallId":"watch"}]}`)

	foregroundCtx, cancelForeground := context.WithCancel(context.Background())
	defer cancelForeground()
	results := make(chan string, 2)
	for id, callCtx := range map[string]context.Context{"watch": context.Background(), "edit": foregroundCtx} {
		go func() {
			var response schema.RequestPermissionResponse
			err := server.Call(callCtx, schema.SessionRequestPermissionMethodName, json.RawMessage(
				`{"sessionId":"s","title":"t","options":[{"optionId":"allow","name":"Allow","kind":"allow_once"}],"subject":{"type":"tool_call","toolCall":{"toolCallId":"`+id+`"}}}`), &response)
			switch {
			case err != nil:
				results <- id + " abandoned"
			case response.Outcome.Cancelled != nil:
				results <- id + " cancelled"
			default:
				results <- id + " selected"
			}
		}()
	}
	for range 2 {
		<-entered
	}
	if err := handle.CancelActiveWork(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancels:
	case <-time.After(2 * time.Second):
		t.Fatal("session/cancel was not sent")
	}
	select {
	case id := <-dismissed:
		t.Fatalf("CancelActiveWork cancelled the %q request locally", id)
	case <-time.After(50 * time.Millisecond):
	}
	// The Agent stops waiting for the foreground request: its Call sends
	// $/cancel_request, and the host's context ends.
	cancelForeground()
	if got := <-dismissed; got != "edit" {
		t.Fatalf("dismissed %q", got)
	}
	if got := <-results; got != "edit abandoned" {
		t.Fatalf("foreground result = %q", got)
	}
	select {
	case got := <-results:
		t.Fatalf("background request resolved: %q", got)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if got := <-results; got != "watch selected" {
		t.Fatalf("background result = %q", got)
	}
	if count := permissions.CancelPermissionRequests("s"); count != 0 {
		t.Fatalf("answered requests still pending: %d", count)
	}
}
