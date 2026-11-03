package unstable_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/agent"
	stableschema "github.com/BrokkAi/acp-go/schema"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
	acpunstable "github.com/BrokkAi/acp-go/unstable"
)

// observeWire folds session/update params, given as JSON, into tree.
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

// canonical re-encodes a JSON document in the form encoded produces.
func canonical(t *testing.T, document string) string {
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

// The RFD's association conformance cases: only sessionId is required;
// omitted fields are unchanged, null clears, and a value replaces the whole
// field, capabilities and state included.
func TestSubagentTreeAssociationPatches(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	observeWire(t, tree,
		`{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child","title":"Test investigator","description":"Investigates failures.","capabilities":{"cancel":{}},"state":{"state":"running"},"_meta":{"a":1,"b":2}}}`,
	)
	child := mustSubagent(t, tree, "child")
	if child.Parent != "parent" || child.Root != "parent" || !child.CanCancel() {
		t.Fatalf("announced child = %+v", child)
	}
	if child.State == nil || child.State.Running == nil {
		t.Fatalf("state = %+v, want running", child.State)
	}

	observeWire(t, tree,
		// Omitted fields keep their values.
		`{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child"}}`,
	)
	if child = mustSubagent(t, tree, "child"); child.Association.Title.Value != "Test investigator" || !child.CanCancel() || child.State == nil {
		t.Fatalf("an empty patch changed the association: %+v", child)
	}

	observeWire(t, tree,
		// cancel: null inside a concrete capability set disables it; a
		// concrete _meta replaces the whole v1 metadata.
		`{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child","title":null,"capabilities":{"cancel":null},"state":{"state":"idle","stopReason":"end_turn"},"_meta":{"b":3}}}`,
	)
	child = mustSubagent(t, tree, "child")
	if !child.Association.Title.Set || !child.Association.Title.Null {
		t.Fatalf("title = %+v, want cleared", child.Association.Title)
	}
	if child.Association.Description.Value != "Investigates failures." {
		t.Fatalf("description = %+v, want unchanged", child.Association.Description)
	}
	if child.CanCancel() {
		t.Fatal("cancel: null still permits cancellation")
	}
	if child.State == nil || child.State.Idle == nil || *child.State.Idle.StopReason != schema.IdleStopReasonEndTurn {
		t.Fatalf("state = %+v, want idle end_turn", child.State)
	}
	if got := encoded(t, child.Association.Meta); got != canonical(t, `{"b":3}`) {
		t.Fatalf("_meta = %s, want the v1 replacement", got)
	}

	observeWire(t, tree,
		`{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}}}}`,
		`{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":null,"state":null}}`,
	)
	child = mustSubagent(t, tree, "child")
	if child.CanCancel() || child.Capabilities != nil {
		t.Fatal("capabilities: null kept a mutation")
	}
	if child.State != nil {
		t.Fatalf("state: null left %+v; a cleared state is unconfirmed", child.State)
	}
}

func TestSubagentTreeRefusesInvalidAssociations(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	observeWire(t, tree,
		`{"sessionId":"root","update":{"sessionUpdate":"subagent_update","sessionId":"a"}}`,
		`{"sessionId":"a","update":{"sessionUpdate":"subagent_update","sessionId":"b"}}`,
	)
	handle := tree.Notifications()
	for name, notification := range map[string]string{
		"self":     `{"sessionId":"x","update":{"sessionUpdate":"subagent_update","sessionId":"x"}}`,
		"reparent": `{"sessionId":"root","update":{"sessionUpdate":"subagent_update","sessionId":"b","title":"moved"}}`,
		"cycle":    `{"sessionId":"b","update":{"sessionUpdate":"subagent_update","sessionId":"root"}}`,
	} {
		if err := handle(schema.SessionUpdateMethodName, json.RawMessage(notification)); err == nil {
			t.Errorf("%s association was accepted", name)
		}
	}
	if b := mustSubagent(t, tree, "b"); b.Parent != "a" || b.Root != "root" || b.Association.Title.Set {
		t.Fatalf("a refused update changed b: %+v", b)
	}
	if tree.IsSubagent("root") || tree.IsSubagent("x") {
		t.Fatal("a refused update registered a child")
	}
	if got := tree.Descendants("root"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("descendants = %v", got)
	}

	// A relative directory is reported, and only it is dropped.
	if err := handle(schema.SessionUpdateMethodName, json.RawMessage(`{"sessionId":"root","update":{"sessionUpdate":"subagent_update","sessionId":"c","title":"C","cwd":"work","additionalDirectories":["/ok","lib"]}}`)); err == nil {
		t.Fatal("a relative directory was not reported")
	}
	if c := mustSubagent(t, tree, "c"); c.Association.Title.Value != "C" || c.Association.Cwd.Set || c.Association.AdditionalDirectories.Set {
		t.Fatalf("association with relative directories = %s", encoded(t, c.Association))
	}
}

// Unset or cleared directories inherit the parent's effective values, each
// independently.
func TestSubagentTreeDirectoriesInherit(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	tree.SetSessionDirectories("root", "/project", []string{"/lib"})
	observeWire(t, tree,
		`{"sessionId":"root","update":{"sessionUpdate":"subagent_update","sessionId":"worker"}}`,
		`{"sessionId":"worker","update":{"sessionUpdate":"subagent_update","sessionId":"nested","cwd":"/tmp/worktree"}}`,
	)
	if cwd, additional, ok := tree.Directories("nested"); !ok || cwd != "/tmp/worktree" || len(additional) != 1 || additional[0] != "/lib" {
		t.Fatalf("nested directories = %q %q %v", cwd, additional, ok)
	}
	observeWire(t, tree,
		`{"sessionId":"root","update":{"sessionUpdate":"subagent_update","sessionId":"worker","cwd":"/worktree","additionalDirectories":[]}}`,
		`{"sessionId":"worker","update":{"sessionUpdate":"subagent_update","sessionId":"nested","cwd":null}}`,
	)
	if cwd, additional, ok := tree.Directories("nested"); !ok || cwd != "/worktree" || len(additional) != 0 {
		t.Fatalf("nested directories after the parent moved = %q %q %v", cwd, additional, ok)
	}
	if _, _, ok := tree.Directories("unknown"); ok {
		t.Fatal("an unrecorded session reported directories")
	}
}

// The RFD's message conformance cases: an upsert's content replaces the
// accumulated content, null and [] clear it, chunks append, and
// participants and references come only from upserts.
func TestSubagentTreeMessageUpsertsAndChunks(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	observeWire(t, tree,
		`{"sessionId":"child","update":{"sessionUpdate":"session_message_chunk","messageId":"m","content":{"type":"text","text":"Start by "}}}`,
		`{"sessionId":"child","update":{"sessionUpdate":"session_message","messageId":"m","senderSessionId":"parent","recipientSessionId":"child"}}`,
		`{"sessionId":"child","update":{"sessionUpdate":"session_message_chunk","messageId":"m","content":{"type":"text","text":"reproducing"},"_meta":{"chunk":true}}}`,
		`{"sessionId":"child","update":{"sessionUpdate":"session_message","messageId":"other","content":[{"type":"text","text":"x"}]}}`,
	)
	messages := tree.Messages("child")
	if len(messages) != 2 || messages[0].MessageID != "m" || messages[1].MessageID != "other" {
		t.Fatalf("messages = %+v", messages)
	}
	if got := encoded(t, messages[0]); got != canonical(t, `{"messageId":"m","senderSessionId":"parent","recipientSessionId":"child","content":[{"type":"text","text":"Start by "},{"type":"text","text":"reproducing"}]}`) {
		t.Fatalf("streamed message = %s", got)
	}

	observeWire(t, tree,
		`{"sessionId":"child","update":{"sessionUpdate":"session_message","messageId":"m","content":[{"type":"text","text":"replaced"}],"senderSessionId":null}}`,
		`{"sessionId":"child","update":{"sessionUpdate":"session_message_chunk","messageId":"m","content":{"type":"text","text":"!"}}}`,
	)
	message, _ := tree.Message("child", "m")
	if got := encoded(t, message); got != canonical(t, `{"messageId":"m","senderSessionId":null,"recipientSessionId":"child","content":[{"type":"text","text":"replaced"},{"type":"text","text":"!"}]}`) {
		t.Fatalf("replaced message = %s", got)
	}
	for _, clear := range []string{`[]`, `null`} {
		observeWire(t, tree, `{"sessionId":"child","update":{"sessionUpdate":"session_message","messageId":"m","content":`+clear+`}}`)
		if message, _ := tree.Message("child", "m"); len(message.Content.Value) != 0 {
			t.Fatalf("content %s left %+v", clear, message.Content)
		}
	}
	// A snapshot is not changed by later chunks.
	snapshot, _ := tree.Message("child", "other")
	observeWire(t, tree, `{"sessionId":"child","update":{"sessionUpdate":"session_message_chunk","messageId":"other","content":{"type":"text","text":"y"}}}`)
	if len(snapshot.Content.Value) != 1 {
		t.Fatalf("a later chunk changed a snapshot: %+v", snapshot.Content)
	}
}

func TestSubagentTreeLinksMessageViews(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	observeWire(t, tree,
		`{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child"}}`,
		`{"sessionId":"parent","update":{"sessionUpdate":"session_message","messageId":"msg_to_child_1","senderSessionId":"parent","recipientSessionId":"child","content":[{"type":"text","text":"Also check Windows."}]}}`,
	)
	if _, ok := tree.LinkedMessage("parent", "msg_to_child_1"); ok {
		t.Fatal("an outgoing view without recipientMessageId is linked")
	}
	observeWire(t, tree,
		`{"sessionId":"child","update":{"sessionUpdate":"session_message","messageId":"msg_from_parent_1","senderSessionId":"parent","recipientSessionId":"child","senderMessageId":"msg_to_child_1","content":[{"type":"text","text":"Also check Windows."}]}}`,
	)
	if ref, ok := tree.LinkedMessage("child", "msg_from_parent_1"); !ok || ref != (acpunstable.MessageRef{SessionID: "parent", MessageID: "msg_to_child_1"}) {
		t.Fatalf("incoming view links to %+v, %v", ref, ok)
	}
	observeWire(t, tree,
		`{"sessionId":"parent","update":{"sessionUpdate":"session_message","messageId":"msg_to_child_1","recipientMessageId":"msg_from_parent_1"}}`,
	)
	if ref, ok := tree.LinkedMessage("parent", "msg_to_child_1"); !ok || ref != (acpunstable.MessageRef{SessionID: "child", MessageID: "msg_from_parent_1"}) {
		t.Fatalf("outgoing view links to %+v, %v", ref, ok)
	}
	// Conflicting references leave the views unlinked.
	observeWire(t, tree,
		`{"sessionId":"parent","update":{"sessionUpdate":"session_message","messageId":"msg_to_child_1","recipientMessageId":"somewhere_else"}}`,
	)
	if _, ok := tree.LinkedMessage("child", "msg_from_parent_1"); ok {
		t.Fatal("conflicting references are linked")
	}
}

// Child error and cancelled idle snapshots, and unknown stop reasons and
// states, are kept as reported.
func TestSubagentTreeKeepsChildOutcomes(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	observeWire(t, tree,
		`{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child","state":{"state":"idle","stopReason":"error","error":{"code":-32000,"message":"Authentication required"},"usage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}}}`,
	)
	state := mustSubagent(t, tree, "child").State
	if state == nil || state.Idle == nil || *state.Idle.StopReason != schema.IdleStopReasonError || state.Idle.Error == nil || state.Idle.Error.Code != -32000 || state.Idle.Usage == nil {
		t.Fatalf("error idle = %s", encoded(t, state))
	}
	for _, wire := range []string{
		`{"state":"idle","stopReason":"cancelled"}`,
		`{"state":"idle","stopReason":"_vendor_reason","detail":1}`,
		`{"state":"unknown"}`,
		`{"state":"_vendor_state","x":1}`,
	} {
		observeWire(t, tree, `{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child","state":`+wire+`}}`)
		if got := encoded(t, mustSubagent(t, tree, "child").State); got != canonical(t, wire) {
			t.Errorf("state = %s, want %s", got, wire)
		}
	}
}

func TestSubagentTreeRestrictsChildMethods(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	observeWire(t, tree, `{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child"}}`)
	for _, method := range []string{
		schema.SessionPromptMethodName, schema.SessionLoadMethodName, schema.SessionResumeMethodName,
		schema.SessionCloseMethodName, schema.SessionDeleteMethodName, schema.SessionForkMethodName,
		schema.SessionSetModeMethodName, schema.SessionSetConfigOptionMethodName, schema.SessionSetTitleMethodName,
		schema.SessionArchiveMethodName, "_vendor/steer", schema.SessionCancelMethodName,
	} {
		if err := tree.CheckClientMethod(method, "child"); err == nil {
			t.Errorf("%s was permitted on a child without capabilities", method)
		}
		if err := tree.CheckClientMethod(method, "parent"); err != nil {
			t.Errorf("%s was refused on an ordinary session: %v", method, err)
		}
	}
	if err := tree.CheckClientMethod(schema.SessionStatusMethodName, "child"); err != nil {
		t.Errorf("session/status is read-only: %v", err)
	}
	observeWire(t, tree, `{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}}}}`)
	if err := tree.CheckClientMethod(schema.SessionCancelMethodName, "child"); err != nil {
		t.Fatalf("advertised cancel was refused: %v", err)
	}
	if err := tree.CheckClientMethod(schema.SessionPromptMethodName, "child"); err == nil {
		t.Fatal("cancel enabled session/prompt")
	}
	if err := tree.Restore(context.Background(), "child", true, func(context.Context) error { return nil }); err == nil {
		t.Fatal("a child was restored directly")
	}
}

// peer is a fake agent on the other end of a client connection: tests write
// raw frames to it and read what the client sent.
type peer struct {
	encoder *json.Encoder
	frames  chan map[string]json.RawMessage
}

func connectPeer(t *testing.T, notifications acp.Notifications) (*acp.Connection, *peer) {
	t.Helper()
	local, remote := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = remote.SetDeadline(deadline)
	connection := acp.Connect(local, local, nil, notifications)
	p := &peer{encoder: json.NewEncoder(remote), frames: make(chan map[string]json.RawMessage, 16)}
	go func() {
		decoder := json.NewDecoder(remote)
		for {
			var frame map[string]json.RawMessage
			if decoder.Decode(&frame) != nil {
				close(p.frames)
				return
			}
			p.frames <- frame
		}
	}()
	t.Cleanup(func() {
		_ = remote.Close()
		_ = connection.Close()
	})
	return connection, p
}

func (p *peer) send(t *testing.T, frame string) {
	t.Helper()
	if err := p.encoder.Encode(json.RawMessage(frame)); err != nil {
		t.Fatal(err)
	}
}

func (p *peer) next(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	select {
	case frame, ok := <-p.frames:
		if !ok {
			t.Fatal("connection closed")
		}
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("no frame")
	}
	return nil
}

func update(session, body string) string {
	return `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + session + `","update":` + body + `}}`
}

func TestSubagentTreeCancelWritesOnlyWhenPermitted(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	connection, agentSide := connectPeer(t, tree.Notifications())
	observeWire(t, tree, `{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child"}}`)
	ctx := context.Background()
	if err := tree.Cancel(ctx, connection, "child"); err == nil {
		t.Fatal("cancel without the capability was sent")
	}
	if err := tree.Cancel(ctx, connection, "parent"); err == nil {
		t.Fatal("Cancel accepted an ordinary session")
	}
	observeWire(t, tree, `{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}}}}`)
	if err := tree.Cancel(ctx, connection, "child"); err != nil {
		t.Fatal(err)
	}
	frame := agentSide.next(t)
	if string(frame["method"]) != `"session/cancel"` || string(frame["params"]) != `{"sessionId":"child"}` {
		t.Fatalf("first frame on the wire = %v", frame)
	}
	// Cancellation does not end the child or change its state; the child
	// confirms with idle/cancelled.
	if child := mustSubagent(t, tree, "child"); !child.CanCancel() {
		t.Fatal("cancelling disabled the child")
	}
}

// Closing a parent ends all descendant work: their controls and current
// state are invalidated and they are returned for request cleanup.
func TestSubagentTreeClosedCascades(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	observeWire(t, tree,
		`{"sessionId":"root","update":{"sessionUpdate":"subagent_update","sessionId":"a","capabilities":{"cancel":{}},"state":{"state":"running"}}}`,
		`{"sessionId":"a","update":{"sessionUpdate":"subagent_update","sessionId":"b","capabilities":{"cancel":{}},"state":{"state":"running"}}}`,
		`{"sessionId":"other","update":{"sessionUpdate":"subagent_update","sessionId":"c","capabilities":{"cancel":{}},"state":{"state":"running"}}}`,
		`{"sessionId":"a","update":{"sessionUpdate":"session_message","messageId":"m","content":[]}}`,
	)
	if closed := tree.Closed("root"); len(closed) != 2 || closed[0] != "a" || closed[1] != "b" {
		t.Fatalf("closed descendants = %v", closed)
	}
	for _, id := range []schema.SessionId{"a", "b"} {
		child := mustSubagent(t, tree, id)
		if child.CanCancel() || child.State != nil || child.Association.State.Value.Running == nil {
			t.Errorf("%s after close = %+v", id, child)
		}
	}
	if c := mustSubagent(t, tree, "c"); !c.CanCancel() || c.State == nil {
		t.Fatal("closing root invalidated an unrelated tree")
	}
	if len(tree.Messages("a")) != 1 {
		t.Fatal("closing erased history")
	}
	tree.Disconnected()
	if c := mustSubagent(t, tree, "c"); c.CanCancel() || c.State != nil {
		t.Fatal("connection loss kept a live state or control")
	}
}

// A replaying session/load invalidates child controls and state: snapshots
// replayed before the response are history, and only those sent after it
// are current.
func TestSubagentTreeRestoreWithReplay(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	connection, agentSide := connectPeer(t, tree.Notifications())
	observeWire(t, tree,
		`{"sessionId":"root","update":{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}},"state":{"state":"running"}}}`,
		`{"sessionId":"child","update":{"sessionUpdate":"session_message","messageId":"m","content":[{"type":"text","text":"old"}]}}`,
	)

	done := make(chan error, 1)
	checked := make(chan acpunstable.Subagent, 1)
	go func() {
		done <- tree.Restore(context.Background(), "root", true, func(ctx context.Context) error {
			return connection.Call(ctx, schema.SessionLoadMethodName, map[string]any{"sessionId": "root"}, nil)
		})
	}()
	request := agentSide.next(t)
	if child := mustSubagent(t, tree, "child"); child.CanCancel() || child.State != nil {
		t.Fatalf("restore kept live state: %+v", child)
	}
	agentSide.send(t, update("root", `{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}},"state":{"state":"running"}}`))
	agentSide.send(t, update("child", `{"sessionUpdate":"session_message","messageId":"m","content":[{"type":"text","text":"replayed"}]}`))
	agentSide.send(t, update("root", `{"sessionUpdate":"subagent_update","sessionId":"replayed_child","capabilities":{"cancel":{}}}`))
	agentSide.send(t, `{"jsonrpc":"2.0","id":`+string(request["id"])+`,"result":{}}`)
	agentSide.send(t, update("root", `{"sessionUpdate":"subagent_update","sessionId":"child","state":{"state":"idle","stopReason":"end_turn"}}`))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Wait for the post-response update to be folded.
	deadline := time.Now().Add(5 * time.Second)
	for {
		child := mustSubagent(t, tree, "child")
		if child.State != nil || time.Now().After(deadline) {
			checked <- child
			break
		}
		time.Sleep(time.Millisecond)
	}
	child := <-checked
	if child.State == nil || child.State.Idle == nil {
		t.Fatalf("post-response state = %+v, want current idle", child.State)
	}
	if child.CanCancel() {
		t.Fatal("a replayed cancel capability authorized cancellation")
	}
	if !child.Association.Capabilities.Set || child.Association.Capabilities.Value.Cancel == nil {
		t.Fatal("replayed capabilities are not kept as history")
	}
	if replayed := mustSubagent(t, tree, "replayed_child"); replayed.CanCancel() {
		t.Fatal("a child announced in replay is controllable")
	}
	if messages := tree.Messages("child"); len(messages) != 1 || messages[0].Content.Value[0].Text.Text != "replayed" {
		t.Fatalf("replayed transcript = %s", encoded(t, messages))
	}
	agentSide.send(t, update("root", `{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}}}`))
	for deadline := time.Now().Add(5 * time.Second); !mustSubagent(t, tree, "child").CanCancel(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a current capability after the response did not authorize cancellation")
		}
	}
}

// Without replay, snapshots during the resume are current, but controls wait
// for it to succeed; a failed attempt invalidates them again.
func TestSubagentTreeRestoreWithoutReplay(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	connection, agentSide := connectPeer(t, tree.Notifications())
	observeWire(t, tree, `{"sessionId":"root","update":{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}}}}`)

	for _, fail := range []bool{false, true} {
		done := make(chan error, 1)
		during := make(chan acpunstable.Subagent, 1)
		go func() {
			done <- tree.Restore(context.Background(), "root", false, func(ctx context.Context) error {
				err := connection.Call(ctx, schema.SessionResumeMethodName, map[string]any{"sessionId": "root"}, nil)
				during <- mustSubagent(t, tree, "child")
				return err
			})
		}()
		request := agentSide.next(t)
		agentSide.send(t, update("root", `{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}},"state":{"state":"running"}}`))
		if fail {
			agentSide.send(t, `{"jsonrpc":"2.0","id":`+string(request["id"])+`,"error":{"code":-32002,"message":"Resource not found"}}`)
		} else {
			agentSide.send(t, `{"jsonrpc":"2.0","id":`+string(request["id"])+`,"result":{}}`)
		}
		snapshot := <-during
		if snapshot.CanCancel() || snapshot.State == nil {
			t.Fatalf("during the resume: %+v; state is current but controls wait", snapshot)
		}
		err := <-done
		child := mustSubagent(t, tree, "child")
		if fail {
			if err == nil || child.CanCancel() || child.State != nil {
				t.Fatalf("failed resume: err %v, child %+v", err, child)
			}
		} else if err != nil || !child.CanCancel() || child.State == nil {
			t.Fatalf("resume: err %v, child %+v", err, child)
		}
	}
}

func TestProjectionFeedsSubagentTree(t *testing.T) {
	projection := acpunstable.NewProjection()
	handle := projection.Notifications()
	for _, notification := range []string{
		`{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"child","title":"Worker"}}`,
		`{"sessionId":"child","update":{"sessionUpdate":"usage_update","used":5,"size":10}}`,
	} {
		if err := handle(schema.SessionUpdateMethodName, json.RawMessage(notification)); err != nil {
			t.Fatal(err)
		}
	}
	if child, ok := projection.Subagents().Subagent("child"); !ok || child.Association.Title.Value != "Worker" {
		t.Fatalf("projection's tree = %+v, %v", child, ok)
	}
	if usage, ok := projection.Usage("child"); !ok || usage.Used != 5 {
		t.Fatalf("child usage = %+v, %v", usage, ok)
	}
	if err := handle(schema.SessionUpdateMethodName, json.RawMessage(`{"sessionId":"x","update":{"sessionUpdate":"subagent_update","sessionId":"x"}}`)); err == nil {
		t.Fatal("a self-association did not fail Notifications")
	}
}

func TestInitializeAdvertisesSubagents(t *testing.T) {
	connection, agentSide := connectPeer(t, nil)
	result := make(chan error, 1)
	var initialization schema.InitializeResponse
	go func() {
		var err error
		initialization, err = acpunstable.Initialize(context.Background(), connection,
			schema.ClientCapabilities{Subagents: acpunstable.SubagentsClientCapabilities()},
			schema.Implementation{Name: "client", Version: "1.0.0"})
		result <- err
	}()
	request := agentSide.next(t)
	if got := string(request["params"]); !strings.Contains(got, `"clientCapabilities":{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false,"subagents":{},"auth":{"terminal":false}}`) || !strings.Contains(got, `"protocolVersion":1`) {
		t.Fatalf("initialize params = %s", got)
	}
	agentSide.send(t, `{"jsonrpc":"2.0","id":`+string(request["id"])+`,"result":{"protocolVersion":1,"agentCapabilities":{"providers":{}}}}`)
	if err := <-result; err != nil || initialization.AgentCapabilities == nil || initialization.AgentCapabilities.Providers == nil {
		t.Fatalf("initialize = %+v, %v", initialization, err)
	}

	go func() {
		_, err := acpunstable.Initialize(context.Background(), connection, schema.ClientCapabilities{}, schema.Implementation{Name: "client", Version: "1.0.0"})
		result <- err
	}()
	request = agentSide.next(t)
	agentSide.send(t, `{"jsonrpc":"2.0","id":`+string(request["id"])+`,"result":{"protocolVersion":2}}`)
	var rpcError *acp.RPCError
	if err := <-result; !errors.As(err, &rpcError) || rpcError.Code != -32600 {
		t.Fatalf("a v2 selection = %v, want the -32600 InitializeWithInfo answers", err)
	}
	if _, err := acpunstable.Initialize(context.Background(), connection, schema.ClientCapabilities{}, schema.Implementation{}); err == nil {
		t.Fatal("initialize without client info was sent")
	}
}

// subagentAgent sends the subagent surface from its prompt turn.
type subagentAgent struct {
	errs chan error
}

func (subagentAgent) Initialize(context.Context, agent.Client, stableschema.InitializeRequest) (stableschema.InitializeResponse, error) {
	return stableschema.InitializeResponse{ProtocolVersion: acp.Version}, nil
}

func (subagentAgent) NewSession(context.Context, agent.Client, stableschema.NewSessionRequest) (stableschema.NewSessionResponse, error) {
	return stableschema.NewSessionResponse{SessionID: "parent"}, nil
}

func (a subagentAgent) Prompt(ctx context.Context, client agent.Client, request stableschema.PromptRequest, _ agent.SessionUpdater) (stableschema.PromptResponse, error) {
	parent := schema.SessionId(request.SessionID)
	a.errs <- acpunstable.NotifySubagentUpdate(ctx, client, parent, schema.SubagentUpdate{
		SessionID:    "child",
		Title:        acpunstable.Value("Worker"),
		Capabilities: acpunstable.Value(schema.SubagentSessionCapabilities{Cancel: &schema.SessionCancelCapabilities{}}),
		Cwd:          acpunstable.Value("/work"),
	})
	a.errs <- acpunstable.NotifySubagentState(ctx, client, parent, "child", acpunstable.ErrorState(&schema.Error{Code: -32000, Message: "Authentication required"}))
	a.errs <- acpunstable.NotifySessionMessage(ctx, client, parent, schema.SessionMessage{
		MessageID:          "m",
		SenderSessionID:    acpunstable.Value[schema.SessionId]("parent"),
		RecipientSessionID: acpunstable.Value[schema.SessionId]("child"),
	})
	a.errs <- acpunstable.NotifySessionMessageChunk(ctx, client, parent, schema.SessionMessageChunk{MessageID: "m", Content: schema.ContentBlock{Text: &schema.TextContent{Text: "hi"}}})
	// Hand-built updates are gated too.
	a.errs <- client.Notify(ctx, schema.SessionUpdateMethodName, json.RawMessage(`{"sessionId":"parent","update":{"sessionUpdate":"subagent_update","sessionId":"raw"}}`))
	a.errs <- client.Notify(ctx, schema.SessionUpdateMethodName, map[string]any{"sessionId": "parent", "update": map[string]any{"sessionUpdate": "session_message_chunk"}})
	return stableschema.PromptResponse{StopReason: stableschema.StopReasonEndTurn}, nil
}

func startSubagentAgent(t *testing.T, notifications acp.Notifications) (*acp.Connection, chan error) {
	t.Helper()
	agentSide, clientSide := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = agentSide.SetDeadline(deadline)
	_ = clientSide.SetDeadline(deadline)
	errs := make(chan error, 8)
	runtime := agent.New(subagentAgent{errs: errs})
	go func() { _ = runtime.Serve(context.Background(), agentSide, agentSide) }()
	connection := acp.Connect(clientSide, clientSide, nil, notifications)
	t.Cleanup(func() {
		_ = connection.Close()
		_ = clientSide.Close()
	})
	return connection, errs
}

// In v1 the agent runtime refuses every subagent update, before writing,
// unless the client advertised clientCapabilities.subagents.
func TestAgentRuntimeGatesSubagentUpdatesOnTheClientCapability(t *testing.T) {
	ctx := context.Background()
	for _, advertise := range []bool{false, true} {
		projection := acpunstable.NewProjection()
		connection, errs := startSubagentAgent(t, projection.Notifications())
		capabilities := schema.ClientCapabilities{}
		if advertise {
			capabilities.Subagents = acpunstable.SubagentsClientCapabilities()
		}
		if _, err := acpunstable.Initialize(ctx, connection, capabilities, schema.Implementation{Name: "client", Version: "1.0.0"}); err != nil {
			t.Fatal(err)
		}
		session, err := connection.NewSession(ctx, "/work")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Prompt(ctx, session, "go"); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 6; i++ {
			err := <-errs
			var rpcError *acp.RPCError
			switch {
			case advertise && err != nil:
				t.Errorf("update %d with the capability: %v", i, err)
			case !advertise && (!errors.As(err, &rpcError) || rpcError.Code != -32601):
				t.Errorf("update %d without the capability = %v, want -32601", i, err)
			}
		}
		tree := projection.Subagents()
		if advertise {
			child := mustSubagent(t, tree, "child")
			if !child.CanCancel() || child.State == nil || child.State.Idle == nil || child.State.Idle.Error.Code != -32000 {
				t.Fatalf("child = %+v", child)
			}
			if messages := tree.Messages("parent"); len(messages) != 1 || messages[0].Content.Value[0].Text.Text != "hi" {
				t.Fatalf("messages = %s", encoded(t, messages))
			}
			if !tree.IsSubagent("raw") {
				t.Fatal("a hand-built update was not delivered")
			}
		} else if tree.IsSubagent("child") || tree.IsSubagent("raw") || len(tree.Messages("parent")) != 0 {
			t.Fatal("a refused update reached the client")
		}
	}
}

func TestNotifySubagentUpdateValidatesBeforeWriting(t *testing.T) {
	ctx := context.Background()
	client := &paramsClient{}
	for name, call := range map[string]func() error{
		"blank": func() error { return acpunstable.NotifySubagentUpdate(ctx, client, "p", schema.SubagentUpdate{}) },
		"relative": func() error {
			return acpunstable.NotifySubagentUpdate(ctx, client, "p", schema.SubagentUpdate{SessionID: "c", Cwd: acpunstable.Value("rel")})
		},
		"dirs": func() error {
			return acpunstable.NotifySubagentUpdate(ctx, client, "p", schema.SubagentUpdate{SessionID: "c", AdditionalDirectories: acpunstable.Value([]string{"rel"})})
		},
		"blank message": func() error {
			return acpunstable.NotifySessionMessage(ctx, client, "a", schema.SessionMessage{})
		},
		"chunk id": func() error {
			return acpunstable.NotifySessionMessageChunk(ctx, client, "a", schema.SessionMessageChunk{Content: schema.ContentBlock{Text: &schema.TextContent{}}})
		},
	} {
		if err := call(); err == nil {
			t.Errorf("%s was sent", name)
		}
	}
	if len(client.sent) != 0 {
		t.Fatalf("invalid updates reached Notify: %v", client.sent)
	}
	if err := acpunstable.NotifySubagentUpdate(ctx, client, "p", schema.SubagentUpdate{SessionID: "c", Cwd: acpunstable.Null[string]()}); err != nil {
		t.Fatal(err)
	}
	if got := encoded(t, client.sent[0]); got != canonical(t, `{"sessionId":"p","update":{"sessionUpdate":"subagent_update","sessionId":"c","cwd":null}}`) {
		t.Fatalf("sent %s", got)
	}
	// As in the reference SDK, the senders do not check what the RFD leaves
	// to the agent: a self-association and inconsistent participants go out.
	if err := acpunstable.NotifySubagentUpdate(ctx, client, "p", schema.SubagentUpdate{SessionID: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := acpunstable.NotifySessionMessage(ctx, client, "x", schema.SessionMessage{MessageID: "m", SenderSessionID: acpunstable.Value[schema.SessionId]("a"), RecipientSessionID: acpunstable.Value[schema.SessionId]("a")}); err != nil {
		t.Fatal(err)
	}
	if len(client.sent) != 3 {
		t.Fatalf("sent %d notifications, want 3", len(client.sent))
	}
}

type paramsClient struct {
	agent.Client
	sent []any
}

func (c *paramsClient) Notify(_ context.Context, _ string, params any) error {
	c.sent = append(c.sent, params)
	return nil
}
