package v2

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// applyWire decodes each session update from its wire form and applies it.
func applyWire(t *testing.T, projection *UpdateProjection, updates ...string) {
	t.Helper()
	for _, raw := range updates {
		var update schema.SessionUpdate
		if err := json.Unmarshal([]byte(raw), &update); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		projection.Apply(update)
	}
}

func assertWire(t *testing.T, value any, want string) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := canonicalJSON(t, string(encoded)), canonicalJSON(t, want); got != want {
		t.Fatalf("stored %s, want %s", got, want)
	}
}

func canonicalJSON(t *testing.T, doc string) string {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(doc), &value); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, doc)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestUpdateProjectionKeysEntitiesByKindAndID(t *testing.T) {
	projection := NewUpdateProjection()
	applyWire(t, projection,
		`{"sessionUpdate":"agent_thought_chunk","messageId":"m","content":{"type":"text","text":"thinking"}}`,
		`{"sessionUpdate":"user_message","messageId":"m","content":[{"type":"text","text":"question"}]}`,
		`{"sessionUpdate":"tool_call_content_chunk","toolCallId":"m","content":{"type":"content","content":{"type":"text","text":"out"}}}`,
		`{"sessionUpdate":"agent_message","messageId":"m","content":[{"type":"text","text":"answer"}]}`,
		`{"sessionUpdate":"terminal_output_chunk","terminalId":"m","data":"b2s="}`,
		`{"sessionUpdate":"compaction_summary_chunk","compactionId":"m","content":{"type":"text","text":"sum"}}`,
		// Later updates patch in place without moving their entity.
		`{"sessionUpdate":"agent_thought","messageId":"m","_meta":{"k":1}}`,
		`{"sessionUpdate":"plan_update","planId":"p","plan":{"type":"items","planId":"p","entries":[]}}`,
		`{"sessionUpdate":"future_update","messageId":"m"}`,
	)
	want := []EntityRef{
		{EntityAgentThought, "m"}, {EntityUserMessage, "m"}, {EntityToolCall, "m"},
		{EntityAgentMessage, "m"}, {EntityTerminal, "m"}, {EntityCompaction, "m"},
	}
	if got := projection.Timeline(); !reflect.DeepEqual(got, want) {
		t.Fatalf("timeline = %v", got)
	}
	thought, _ := projection.AgentThought("m")
	assertWire(t, thought, `{"messageId":"m","content":[{"type":"text","text":"thinking"}],"_meta":{"k":1}}`)
	if projection.Text() != "answer" {
		t.Fatalf("text = %q", projection.Text())
	}
}

func TestUpdateProjectionMessages(t *testing.T) {
	projection := NewUpdateProjection()
	text := func(s string) string { return `{"type":"text","text":"` + s + `"}` }
	steps := []struct{ update, want string }{
		{`{"sessionUpdate":"agent_message","messageId":"m","content":[` + text("A") + `],"_meta":{"a":1,"b":2}}`,
			`{"messageId":"m","content":[` + text("A") + `],"_meta":{"a":1,"b":2}}`},
		{`{"sessionUpdate":"agent_message_chunk","messageId":"m","content":` + text("B") + `,"_meta":{"chunk":true}}`,
			`{"messageId":"m","content":[` + text("A") + `,` + text("B") + `],"_meta":{"a":1,"b":2}}`},
		// Replacing content leaves the _meta keys the update does not mention.
		{`{"sessionUpdate":"agent_message","messageId":"m","content":[` + text("C") + `],"_meta":{"a":null}}`,
			`{"messageId":"m","content":[` + text("C") + `],"_meta":{"b":2}}`},
		{`{"sessionUpdate":"agent_message_chunk","messageId":"m","content":` + text("D") + `}`,
			`{"messageId":"m","content":[` + text("C") + `,` + text("D") + `],"_meta":{"b":2}}`},
		{`{"sessionUpdate":"agent_message","messageId":"m"}`,
			`{"messageId":"m","content":[` + text("C") + `,` + text("D") + `],"_meta":{"b":2}}`},
		{`{"sessionUpdate":"agent_message","messageId":"m","content":null,"_meta":null}`,
			`{"messageId":"m","content":null,"_meta":null}`},
		{`{"sessionUpdate":"agent_message_chunk","messageId":"m","content":` + text("E") + `}`,
			`{"messageId":"m","content":[` + text("E") + `],"_meta":null}`},
	}
	for _, step := range steps {
		applyWire(t, projection, step.update)
		message, ok := projection.AgentMessage("m")
		if !ok {
			t.Fatal("message missing")
		}
		assertWire(t, message, step.want)
	}
	if _, ok := projection.UserMessage("m"); ok {
		t.Fatal("agent message updates created a user message")
	}
}

func TestUpdateProjectionToolCalls(t *testing.T) {
	projection := NewUpdateProjection()
	content := func(s string) string {
		return `{"type":"content","content":{"type":"text","text":"` + s + `"}}`
	}
	applyWire(t, projection,
		// A chunk for an unseen ID creates the tool call with its defaults.
		`{"sessionUpdate":"tool_call_content_chunk","toolCallId":"call","content":`+content("early")+`}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"call","title":"Build","kind":"execute","status":"in_progress",
		  "capabilities":{"cancel":{}},"_meta":{"zed.dev/a":1,"acme.dev/b":{"x":1}}}`,
		// A writer that only knows its own key does not erase the others.
		`{"sessionUpdate":"tool_call_update","toolCallId":"call","_meta":{"proxy.dev/c":true}}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"call","status":"completed","capabilities":null,
		  "content":[`+content("replaced")+`],"_meta":{"zed.dev/a":null,"acme.dev/b":{"y":2}}}`,
		`{"sessionUpdate":"tool_call_content_chunk","toolCallId":"call","content":`+content("appended")+`,"_meta":{"chunk":1}}`,
	)
	call, ok := projection.ToolCall("call")
	if !ok {
		t.Fatal("tool call missing")
	}
	assertWire(t, call, `{"toolCallId":"call","title":"Build","kind":"execute","status":"completed","capabilities":null,
		"content":[`+content("replaced")+`,`+content("appended")+`],"_meta":{"acme.dev/b":{"y":2},"proxy.dev/c":true}}`)

	applyWire(t, projection, `{"sessionUpdate":"tool_call_update","toolCallId":"call","content":[]}`)
	call, _ = projection.ToolCall("call")
	if !call.Content.Set || call.Content.Null || len(call.Content.Value) != 0 {
		t.Fatalf("content [] did not clear: %+v", call.Content)
	}
}

// The toolCall of a permission request's tool_call subject applies to the
// stored tool call like a tool_call_update, in receive order relative to
// session/update notifications, before the permission handler runs. The
// handler blocks until the notification that follows the request on the wire
// has been observed, so applying the subject on the handler path would apply
// it after that notification. The handler can answer before the read loop
// reaches the final agent_message_chunk, so the test waits for that chunk to be
// observed before checking the timeline.
func TestTrackerRequestsApplyPermissionSubjectInReceiveOrder(t *testing.T) {
	tracker := NewSessionTracker()
	notified := make(chan struct{})
	chunked := make(chan struct{})
	var once, chunkOnce sync.Once
	var atHandler schema.ToolCallUpdate
	host := permissionFunc(func(ctx context.Context, request schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
		select {
		case <-notified:
		case <-ctx.Done():
			return schema.RequestPermissionResponse{}, ctx.Err()
		}
		atHandler, _ = tracker.Projection(request.SessionID).ToolCall("call")
		return CancelPermission(), nil
	})
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	client := Connect(local, local, HandlePermissions(nil, host),
		tracker.TrackerNotifications(func(method string, raw json.RawMessage) error {
			if method == schema.SessionUpdateMethodName {
				once.Do(func() { close(notified) })
				if bytes.Contains(raw, []byte(`"agent_message_chunk"`)) {
					chunkOnce.Do(func() { close(chunked) })
				}
			}
			return nil
		}),
		acp.WithRequestObserver(tracker.TrackerRequests(nil)),
	)
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })

	frames := `{"jsonrpc":"2.0","id":1,"method":"session/request_permission","params":{"sessionId":"s","title":"Allow?","options":[],` +
		`"subject":{"type":"tool_call","toolCall":{"toolCallId":"call","status":"pending","_meta":{"stale":1,"keep":1}}}}}` + "\n" +
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"call","_meta":{"stale":null}}}}` + "\n" +
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","messageId":"m","content":{"type":"text","text":"x"}}}}` + "\n"
	go func() { _, _ = peer.Write([]byte(frames)) }()
	response, err := bufio.NewReader(peer).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(response, []byte(`"cancelled"`)) {
		t.Fatalf("permission response = %s", response)
	}
	const want = `{"toolCallId":"call","status":"pending","_meta":{"keep":1}}`
	assertWire(t, atHandler, want)
	call, _ := tracker.Projection("s").ToolCall("call")
	assertWire(t, call, want)
	select {
	case <-chunked:
	case <-time.After(time.Until(deadline)):
		t.Fatal("agent_message_chunk not observed")
	}
	if got := tracker.Projection("s").Timeline(); !reflect.DeepEqual(got, []EntityRef{{EntityToolCall, "call"}, {EntityAgentMessage, "m"}}) {
		t.Fatalf("timeline = %v", got)
	}
}

func TestUpdateProjectionTerminals(t *testing.T) {
	projection := NewUpdateProjection()
	output := func() string {
		terminal, ok := projection.Terminal("term")
		if !ok {
			t.Fatal("terminal missing")
		}
		return string(terminal.Output)
	}
	applyWire(t, projection,
		`{"sessionUpdate":"terminal_output_chunk","terminalId":"term","data":"ZWFybHkg"}`,
		`{"sessionUpdate":"terminal_update","terminalId":"term","command":"cargo test","cwd":"/work",
		  "size":{"cols":120,"rows":40},"capabilities":{"cancel":{}},"_meta":{"a":1,"b":2}}`,
	)
	if got := output(); got != "early " {
		t.Fatalf("output = %q", got)
	}
	applyWire(t, projection,
		`{"sessionUpdate":"terminal_update","terminalId":"term","output":{"data":"c25hcHNob3Qg","_meta":{"snapshot":1}}}`,
		`{"sessionUpdate":"terminal_output_chunk","terminalId":"term","data":"Y2h1bms="}`,
		`{"sessionUpdate":"terminal_output_chunk","terminalId":"term","data":"not base64!"}`,
	)
	if got := output(); got != "snapshot chunk" {
		t.Fatalf("output = %q", got)
	}
	// The latest snapshot is kept as received, with its _meta.
	snapshot, _ := projection.Terminal("term")
	assertWire(t, snapshot.Terminal.Output, `{"data":"c25hcHNob3Qg","_meta":{"snapshot":1}}`)
	// An invalid snapshot keeps the stored bytes; the other fields apply.
	applyWire(t, projection, `{"sessionUpdate":"terminal_update","terminalId":"term","output":{"data":"!!"},
		"size":null,"capabilities":null,"exitStatus":{"exitCode":0},"_meta":{"a":null,"c":3}}`)
	if got := output(); got != "snapshot chunk" {
		t.Fatalf("output after invalid snapshot = %q", got)
	}
	terminal, _ := projection.Terminal("term")
	assertWire(t, terminal.Terminal, `{"terminalId":"term","command":"cargo test","cwd":"/work","size":null,
		"capabilities":null,"exitStatus":{"exitCode":0},"output":{"data":"!!"},"_meta":{"b":2,"c":3}}`)
	applyWire(t, projection, `{"sessionUpdate":"terminal_update","terminalId":"term","output":null}`)
	if got := output(); got != "" {
		t.Fatalf("output after null = %q", got)
	}
	// An invalid chunk for an unseen terminal still creates it and fixes its
	// timeline position.
	applyWire(t, projection,
		`{"sessionUpdate":"terminal_output_chunk","terminalId":"bad","data":"%%"}`,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m","content":{"type":"text","text":"x"}}`,
		`{"sessionUpdate":"terminal_output_chunk","terminalId":"bad","data":"b2s="}`,
	)
	bad, ok := projection.Terminal("bad")
	if !ok || string(bad.Output) != "ok" {
		t.Fatalf("terminal created by an invalid chunk = %+v, %v", bad, ok)
	}
	want := []EntityRef{{EntityTerminal, "term"}, {EntityTerminal, "bad"}, {EntityAgentMessage, "m"}}
	if got := projection.Timeline(); !reflect.DeepEqual(got, want) {
		t.Fatalf("timeline = %v", got)
	}
}

func TestUpdateProjectionCompactionsSessionInfoAndUsage(t *testing.T) {
	projection := NewUpdateProjection()
	text := func(s string) string { return `{"type":"text","text":"` + s + `"}` }
	applyWire(t, projection,
		`{"sessionUpdate":"compaction_summary_chunk","compactionId":"c","content":`+text("early")+`}`,
	)
	compaction, _ := projection.Compaction("c")
	if compaction.Status != schema.CompactionStatusInProgress {
		t.Fatalf("first-seen status = %q", compaction.Status)
	}
	applyWire(t, projection,
		`{"sessionUpdate":"compaction_update","compactionId":"c","status":"failed","error":"boom","_meta":{"a":1}}`,
		`{"sessionUpdate":"compaction_update","compactionId":"c","status":"completed","summary":[`+text("S")+`],"error":null}`,
		`{"sessionUpdate":"compaction_summary_chunk","compactionId":"c","content":`+text("T")+`}`,
	)
	compaction, _ = projection.Compaction("c")
	assertWire(t, compaction, `{"compactionId":"c","status":"completed","summary":[`+text("S")+`,`+text("T")+`],"error":null,"_meta":{"a":1}}`)
	applyWire(t, projection, `{"sessionUpdate":"compaction_update","compactionId":"c","status":"_custom","summary":[]}`)
	compaction, _ = projection.Compaction("c")
	assertWire(t, compaction, `{"compactionId":"c","status":"_custom","summary":[],"error":null,"_meta":{"a":1}}`)

	if _, ok := projection.SessionInfo(); ok {
		t.Fatal("session info before any update")
	}
	applyWire(t, projection,
		`{"sessionUpdate":"session_info_update","title":"First","updatedAt":"2026-01-01T00:00:00Z","_meta":{"a":1,"b":2}}`,
		`{"sessionUpdate":"session_info_update","title":null,"_meta":{"b":null}}`,
	)
	info, _ := projection.SessionInfo()
	assertWire(t, info, `{"title":null,"updatedAt":"2026-01-01T00:00:00Z","_meta":{"a":1}}`)

	applyWire(t, projection,
		`{"sessionUpdate":"usage_update","used":1,"size":10,"cost":{"amount":0.5,"currency":"USD"},"_meta":{"a":1}}`,
		`{"sessionUpdate":"usage_update","used":2,"size":10}`,
	)
	usage, _ := projection.Usage()
	assertWire(t, usage, `{"used":2,"size":10,"cost":{"amount":0.5,"currency":"USD"}}`)
	applyWire(t, projection, `{"sessionUpdate":"usage_update","used":3,"size":10,"cost":null}`)
	usage, _ = projection.Usage()
	assertWire(t, usage, `{"used":3,"size":10,"cost":null}`)
}

// Replaying the original updates in order reproduces the live state,
// including removals, through the same code path.
func TestUpdateProjectionReplayReproducesLiveState(t *testing.T) {
	updates := []string{
		`{"sessionUpdate":"user_message","messageId":"u","content":[{"type":"text","text":"go"}],"_meta":{"a":1}}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"t","title":"Run","_meta":{"a":1,"b":2}}`,
		`{"sessionUpdate":"terminal_update","terminalId":"x","command":"make","output":{"data":"aGk="},"_meta":{"k":true}}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"t","status":"completed","_meta":{"a":null}}`,
		`{"sessionUpdate":"terminal_output_chunk","terminalId":"x","data":"IQ=="}`,
		`{"sessionUpdate":"agent_message_chunk","messageId":"a","content":{"type":"text","text":"done"}}`,
		`{"sessionUpdate":"session_info_update","title":"T","_meta":{"s":1}}`,
		`{"sessionUpdate":"terminal_update","terminalId":"x","_meta":null}`,
	}
	snapshot := func(projection *UpdateProjection) []any {
		user, _ := projection.UserMessage("u")
		call, _ := projection.ToolCall("t")
		terminal, _ := projection.Terminal("x")
		agent, _ := projection.AgentMessage("a")
		info, _ := projection.SessionInfo()
		return []any{projection.Timeline(), user, call, terminal, agent, info}
	}
	live := NewUpdateProjection()
	applyWire(t, live, updates...)
	replay := NewUpdateProjection()
	applyWire(t, replay, updates...)
	if !reflect.DeepEqual(snapshot(live), snapshot(replay)) {
		t.Fatalf("replay %+v differs from live %+v", snapshot(replay), snapshot(live))
	}
	call, _ := replay.ToolCall("t")
	assertWire(t, call, `{"toolCallId":"t","title":"Run","status":"completed","_meta":{"b":2}}`)
	terminal, _ := replay.Terminal("x")
	if string(terminal.Output) != "hi!" || !terminal.Terminal.Meta.Null {
		t.Fatalf("terminal = %+v", terminal)
	}
}

// The projection never writes into an update's backing arrays or into a
// value it has returned.
func TestUpdateProjectionDoesNotAlias(t *testing.T) {
	projection := NewUpdateProjection()
	content := make([]schema.ContentBlock, 1, 4)
	content[0] = textBlock("A")
	backing := content[:4]
	projection.Apply(schema.SessionUpdate{AgentMessage: &schema.AgentMessage{
		MessageID: "m",
		Content:   schema.Nullable[[]schema.ContentBlock]{Set: true, Value: content},
	}})
	before, _ := projection.AgentMessage("m")
	projection.Apply(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{MessageID: "m", Content: textBlock("B")}})
	if backing[1].Text != nil {
		t.Fatal("chunk was written into the update's backing array")
	}
	if len(before.Content.Value) != 1 || cap(before.Content.Value) != 1 {
		t.Fatalf("returned content changed: %+v", before.Content.Value)
	}
	if projection.Text() != "AB" {
		t.Fatalf("text = %q", projection.Text())
	}
}

// The session projection spans every update for the session, including
// those before Running and replay, and survives BeginWork.
func TestSessionTrackerProjectionSurvivesBeginWork(t *testing.T) {
	tracker := NewSessionTracker()
	notifications := tracker.TrackerNotifications(nil)
	send := func(update string) {
		raw := json.RawMessage(`{"sessionId":"s","update":` + update + `}`)
		if err := notifications(schema.SessionUpdateMethodName, raw); err != nil {
			t.Fatal(err)
		}
	}
	send(`{"sessionUpdate":"tool_call_update","toolCallId":"t","title":"Before","_meta":{"a":1}}`)
	tracker.BeginWork("s")
	send(`{"sessionUpdate":"state_update","state":"running"}`)
	send(`{"sessionUpdate":"tool_call_update","toolCallId":"t","status":"completed","_meta":{"b":2}}`)
	call, ok := tracker.Projection("s").ToolCall("t")
	if !ok {
		t.Fatal("tool call missing")
	}
	assertWire(t, call, `{"toolCallId":"t","title":"Before","status":"completed","_meta":{"a":1,"b":2}}`)
	// The work projection keeps only the Agent text of the current turn.
	send(`{"sessionUpdate":"agent_message_chunk","messageId":"m","content":{"type":"text","text":"hi"}}`)
	work := tracker.Work("s").projection
	if _, ok := work.ToolCall("t"); ok || work.Text() != "hi" {
		t.Fatalf("work projection stored %v, text %q", work.Timeline(), work.Text())
	}

	tracker.Forget("s")
	if _, ok := tracker.Projection("s").ToolCall("t"); ok {
		t.Fatal("Forget kept the session projection")
	}
}
