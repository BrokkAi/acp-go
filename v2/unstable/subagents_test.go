package unstable_test

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	"github.com/BrokkAi/acp-go/v2/agent"
	acpunstable "github.com/BrokkAi/acp-go/v2/unstable"
)

func observeWire(t *testing.T, tree *acpunstable.SubagentTree, notifications ...string) {
	t.Helper()
	handle := tree.Notifications()
	for _, notification := range notifications {
		if err := handle(schema.SessionUpdateMethodName, json.RawMessage(notification)); err != nil {
			t.Fatalf("observe %s: %v", notification, err)
		}
	}
}

// encoded returns value's JSON encoding with sorted object keys.
func encoded(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func canonicalDoc(t *testing.T, document string) string {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(document), &value); err != nil {
		t.Fatal(err)
	}
	return encoded(t, value)
}

func mustSubagent(t *testing.T, tree *acpunstable.SubagentTree, id schema.SessionId) acpunstable.Subagent {
	t.Helper()
	subagent, ok := tree.Subagent(id)
	if !ok {
		t.Fatalf("subagent %s is not known", id)
	}
	return subagent
}

// v2 patches the association's _meta by top-level key and otherwise keeps
// the v1 patch rules.
func TestSubagentTreeAssociationPatchesV2(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	observeWire(t, tree,
		`{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child","title":"Worker","capabilities":{"cancel":{}},"cwd":"/work","_meta":{"a":1,"b":2}}}`,
		`{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child","title":null,"capabilities":{"cancel":null},"_meta":{"a":null,"c":3}}}`,
	)
	child := mustSubagent(t, tree, "child")
	if got := encoded(t, child.Association); got != canonicalDoc(t, `{"sessionId":"child","title":null,"capabilities":{},"cwd":"/work","_meta":{"b":2,"c":3}}`) {
		t.Fatalf("association = %s", got)
	}
	if child.CanCancel() {
		t.Fatal("cancel: null still permits cancellation")
	}
	for name, notification := range map[string]string{
		"self":     `{"sessionId":"x","update":{"sessionUpdate":"subagent_update","sessionId":"x"}}`,
		"reparent": `{"sessionId":"other","update":{"sessionUpdate":"subagent_update","sessionId":"child"}}`,
		"cycle":    `{"sessionId":"child","update":{"sessionUpdate":"subagent_update","sessionId":"parent"}}`,
		"relative": `{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"d","additionalDirectories":["lib"]}}`,
	} {
		if err := tree.Notifications()(schema.SessionUpdateMethodName, json.RawMessage(notification)); err == nil {
			t.Errorf("%s association was accepted", name)
		}
	}
	if d := mustSubagent(t, tree, "d"); d.Association.AdditionalDirectories.Set {
		t.Fatal("a relative additional directory was applied")
	}
	tree.SetSessionDirectories("parent", "/project", []schema.AbsolutePath{"/lib"})
	if cwd, additional, ok := tree.Directories("child"); !ok || cwd != "/work" || len(additional) != 1 || additional[0] != "/lib" {
		t.Fatalf("directories = %q %q %v", cwd, additional, ok)
	}
}

// A v2 child reports its own state_update, mirrored on the parent's
// association; both are one logical state. The parent's background list
// names its background children.
func TestSubagentTreeMirrorsChildStateAndBackground(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	observeWire(t, tree,
		`{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child"}}`,
		`{"sessionId":"child","update":{"sessionUpdate":"state_update","state":"running"}}`,
	)
	if child := mustSubagent(t, tree, "child"); child.State == nil || child.State.Running == nil || child.Association.State.Value.Running == nil {
		t.Fatalf("child state = %+v", child)
	}
	observeWire(t, tree,
		`{"sessionId":"child","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"error","error":{"code":-32603,"message":"Internal error"}}}`,
		`{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child","state":{"state":"idle","stopReason":"error","error":{"code":-32603,"message":"Internal error"}}}}`,
	)
	state := mustSubagent(t, tree, "child").State
	if state == nil || state.Idle == nil || *state.Idle.StopReason != schema.StopReasonError || state.Idle.Error.Code != -32603 {
		t.Fatalf("error idle = %s", encoded(t, state))
	}
	observeWire(t, tree, `{"sessionId":"child","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"cancelled"}}`)
	if state := mustSubagent(t, tree, "child").State; *state.Idle.StopReason != schema.StopReasonCancelled {
		t.Fatalf("cancelled idle = %s", encoded(t, state))
	}

	if _, ok := tree.BackgroundSubagents("parent"); ok {
		t.Fatal("background reported before any state_update")
	}
	observeWire(t, tree, `{"sessionId":"parent","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn","background":[{"type":"subagent","sessionId":"child"},{"type":"terminal","terminalId":"t"}]}}`)
	if children, ok := tree.BackgroundSubagents("parent"); !ok || len(children) != 1 || children[0] != "child" {
		t.Fatalf("background subagents = %v, %v", children, ok)
	}
	observeWire(t, tree, `{"sessionId":"parent","update":{"sessionUpdate":"state_update","state":"running"}}`)
	if _, ok := tree.BackgroundSubagents("parent"); ok {
		t.Fatal("a state without background still reports the previous list")
	}
	observeWire(t, tree, `{"sessionId":"parent","update":{"sessionUpdate":"state_update","state":"running","background":[]}}`)
	if children, ok := tree.BackgroundSubagents("parent"); !ok || len(children) != 0 {
		t.Fatalf("background [] = %v, %v", children, ok)
	}
}

func TestSubagentTreeMessagesV2(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	observeWire(t, tree,
		`{"sessionId":"child","update":{"sessionUpdate":"session_message_chunk","messageId":"m","content":{"type":"text","text":"a"}}}`,
		`{"sessionId":"child","update":{"sessionUpdate":"session_message","messageId":"m","senderSessionId":"parent","recipientSessionId":"child","senderMessageId":"out","_meta":{"k":1}}}`,
		`{"sessionId":"child","update":{"sessionUpdate":"session_message_chunk","messageId":"m","content":{"type":"text","text":"b"},"_meta":{"chunk":true}}}`,
		`{"sessionId":"child","update":{"sessionUpdate":"session_message","messageId":"m","_meta":{"j":2}}}`,
	)
	message, ok := tree.Message("child", "m")
	if got := encoded(t, message); !ok || got != canonicalDoc(t, `{"messageId":"m","senderSessionId":"parent","recipientSessionId":"child","senderMessageId":"out","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}],"_meta":{"k":1,"j":2}}`) {
		t.Fatalf("message = %s", got)
	}
	if ref, ok := tree.LinkedMessage("child", "m"); !ok || ref != (acpunstable.MessageRef{SessionID: "parent", MessageID: "out"}) {
		t.Fatalf("linked = %+v, %v", ref, ok)
	}
	observeWire(t, tree, `{"sessionId":"child","update":{"sessionUpdate":"session_message","messageId":"m","content":[]}}`)
	if message, _ := tree.Message("child", "m"); len(message.Content.Value) != 0 {
		t.Fatal("content [] did not clear")
	}
}

func TestSubagentTreeRestrictsChildMethodsV2(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	observeWire(t, tree, `{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}}}}`)
	for _, method := range []string{
		schema.SessionPromptMethodName, schema.SessionResumeMethodName, schema.SessionCloseMethodName,
		schema.SessionDeleteMethodName, schema.SessionForkMethodName, schema.SessionSetConfigOptionMethodName,
		schema.ToolCallCancelMethodName, schema.TerminalCancelMethodName,
	} {
		if err := tree.CheckClientMethod(method, "child"); err == nil {
			t.Errorf("%s was permitted on a child", method)
		}
	}
	if err := tree.CheckClientMethod(schema.SessionCancelMethodName, "child"); err != nil {
		t.Fatal(err)
	}
}

// v2Peer is a fake v2 agent: it answers initialize itself and hands every
// other frame to the test.
func v2Peer(t *testing.T, notifications acp.Notifications) (*acpv2.Connection, *json.Encoder, chan map[string]json.RawMessage) {
	t.Helper()
	local, remote := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = remote.SetDeadline(deadline)
	connection := acpv2.Connect(local, local, nil, notifications)
	encoder := json.NewEncoder(remote)
	frames := make(chan map[string]json.RawMessage, 16)
	go func() {
		decoder := json.NewDecoder(remote)
		for {
			var frame map[string]json.RawMessage
			if decoder.Decode(&frame) != nil {
				close(frames)
				return
			}
			if string(frame["method"]) == `"initialize"` {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": frame["id"], "result": json.RawMessage(`{"protocolVersion":2,"info":{"name":"agent","version":"1"},"capabilities":{"session":{}}}`)})
				continue
			}
			frames <- frame
		}
	}()
	t.Cleanup(func() {
		_ = remote.Close()
		_ = connection.Close()
	})
	if _, err := connection.Initialize(context.Background(), acpv2.Capabilities{}); err != nil {
		t.Fatal(err)
	}
	return connection, encoder, frames
}

func nextFrame(t *testing.T, frames chan map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	select {
	case frame, ok := <-frames:
		if !ok {
			t.Fatal("connection closed")
		}
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
	}
	return nil
}

func sessionUpdate(session, body string) json.RawMessage {
	return json.RawMessage(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + session + `","update":` + body + `}}`)
}

// A session/resume with replay treats the child state_update and
// association snapshots before its response as history.
func TestSubagentTreeResumeWithReplay(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	connection, encoder, frames := v2Peer(t, tree.Notifications())
	observeWire(t, tree, `{"sessionId":"root","update":{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}},"state":{"state":"running"}}}`)

	done := make(chan error, 1)
	go func() {
		done <- tree.Restore(context.Background(), "root", true, func(ctx context.Context) error {
			return connection.Call(ctx, schema.SessionResumeMethodName, map[string]any{"sessionId": "root", "replayFrom": map[string]any{"type": "start"}}, nil)
		})
	}()
	request := nextFrame(t, frames)
	for _, frame := range []json.RawMessage{
		sessionUpdate("root", `{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}}}`),
		sessionUpdate("child", `{"sessionUpdate":"state_update","state":"running"}`),
		json.RawMessage(`{"jsonrpc":"2.0","id":` + string(request["id"]) + `,"result":{}}`),
		sessionUpdate("child", `{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}`),
	} {
		if err := encoder.Encode(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); mustSubagent(t, tree, "child").State == nil; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the post-response state was not current")
		}
	}
	child := mustSubagent(t, tree, "child")
	if child.State.Idle == nil || child.CanCancel() {
		t.Fatalf("after resume: state %s, cancel %v", encoded(t, child.State), child.CanCancel())
	}

	if err := tree.Cancel(context.Background(), connection, "child"); err == nil {
		t.Fatal("cancel from a replayed capability was sent")
	}
	if err := encoder.Encode(sessionUpdate("root", `{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}}}`)); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); !mustSubagent(t, tree, "child").CanCancel(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a current capability did not authorize cancellation")
		}
	}
	if err := tree.Cancel(context.Background(), connection, "child"); err != nil {
		t.Fatal(err)
	}
	if frame := nextFrame(t, frames); string(frame["method"]) != `"session/cancel"` || string(frame["params"]) != `{"sessionId":"child"}` {
		t.Fatalf("cancel frame = %v", frame)
	}
	tree.Disconnected()
	if child := mustSubagent(t, tree, "child"); child.CanCancel() || child.State != nil {
		t.Fatal("connection loss kept live state")
	}
}

// subagentExtension sends the v2 subagent surface from an Extensions
// request, through the client the runtime puts on the request context.
func subagentExtension(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
	client, _ := agent.ClientFromContext(ctx)
	steps := []func() error{
		func() error {
			return acpunstable.NotifySubagentUpdate(ctx, client, "parent", schema.SubagentUpdate{
				SessionID:    "child",
				Capabilities: acpunstable.Value(schema.SubagentSessionCapabilities{Cancel: &schema.SessionCancelCapabilities{}}),
				Cwd:          acpunstable.Value[schema.AbsolutePath]("/work"),
			})
		},
		func() error {
			return acpunstable.NotifySubagentState(ctx, client, "parent", "child", acpunstable.IdleState(schema.StopReasonEndTurn))
		},
		func() error {
			return acpunstable.NotifySessionMessage(ctx, client, "parent", schema.SessionMessage{
				MessageID:          "m",
				SenderSessionID:    acpunstable.Value[schema.SessionId]("parent"),
				RecipientSessionID: acpunstable.Value[schema.SessionId]("child"),
			})
		},
		func() error {
			return acpunstable.NotifySessionMessageChunk(ctx, client, "parent", schema.SessionMessageChunk{MessageID: "m", Content: schema.ContentBlock{Text: &schema.TextContent{Text: "hi"}}})
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, &acp.RPCError{Code: -32603, Message: err.Error()}
		}
	}
	return map[string]any{}, nil
}

// Subagent updates are baseline v2: the runtime sends them with no client
// capability.
func TestRuntimeSendsSubagentUpdatesWithoutACapability(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	var states []string
	notifications := acpv2.CombineNotifications(tree.Notifications(), func(_ string, raw json.RawMessage) error {
		var notification schema.UpdateSessionNotification
		if json.Unmarshal(raw, &notification) == nil && notification.Update.StateUpdate != nil {
			states = append(states, string(notification.SessionID))
		}
		return nil
	})
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	implementation := &acptest.V2Agent{}
	runtime := agent.New(implementation)
	runtime.Extensions = subagentExtension
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), pair.B, pair.B) }()
	connection := acpv2.Connect(pair.A, pair.A, nil, notifications)
	t.Cleanup(func() {
		_ = connection.Close()
		pair.Close()
		<-done
	})
	initialize(t, connection)
	if err := connection.Call(context.Background(), "_vendor/delegate", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	child := mustSubagent(t, tree, "child")
	if !child.CanCancel() || child.State == nil || child.State.Idle == nil || child.Association.Cwd.Value != "/work" {
		t.Fatalf("child = %s", encoded(t, child))
	}
	if len(states) != 1 || states[0] != "child" {
		t.Fatalf("state_update went to %v, want the child's own stream", states)
	}
	if messages := tree.Messages("parent"); len(messages) != 1 || messages[0].Content.Value[0].Text.Text != "hi" {
		t.Fatalf("messages = %s", encoded(t, messages))
	}
}
