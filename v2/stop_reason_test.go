package v2

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// observeRaw feeds raw session/update payloads for one session through the
// tracker's notification adapter, as a connection would.
func observeRaw(t *testing.T, tracker *SessionTracker, updates ...string) {
	t.Helper()
	notifications := tracker.TrackerNotifications(nil)
	for _, update := range updates {
		raw := json.RawMessage(`{"sessionId":"session","update":` + update + `}`)
		if err := notifications(schema.SessionUpdateMethodName, raw); err != nil {
			t.Fatalf("%s: %v", update, err)
		}
	}
}

const (
	runningRaw = `{"sessionUpdate":"state_update","state":"running"}`
	chunkRaw   = `{"sessionUpdate":"agent_message_chunk","messageId":"m","content":{"type":"text","text":"partial output"}}`
)

func completedWork(t *testing.T, tracker *SessionTracker) WorkResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := tracker.Work("session").Wait(ctx)
	if err != nil {
		t.Fatalf("active work did not complete: %v", err)
	}
	return result
}

func TestActiveWorkKeepsKnownStopReasons(t *testing.T) {
	for _, reason := range []schema.StopReason{
		schema.StopReasonEndTurn,
		schema.StopReasonMaxTokens,
		schema.StopReasonMaxTurnRequests,
		schema.StopReasonRefusal,
		schema.StopReasonCancelled,
	} {
		t.Run(string(reason), func(t *testing.T) {
			tracker := NewSessionTracker()
			observeRaw(t, tracker, runningRaw, chunkRaw,
				`{"sessionUpdate":"state_update","state":"idle","stopReason":"`+string(reason)+`","error":{"code":1,"message":"ignored"}}`)
			result := completedWork(t, tracker)
			if result.StopReason == nil || *result.StopReason != reason {
				t.Fatalf("stop reason = %v", result.StopReason)
			}
			if result.Text != "partial output" || result.Error != nil || result.StopReasonFields != nil || result.Err() != nil {
				t.Fatalf("result = %+v, Err = %v", result, result.Err())
			}
		})
	}
}

func TestActiveWorkReportsErrorStopReason(t *testing.T) {
	tracker := NewSessionTracker()
	observeRaw(t, tracker, runningRaw, chunkRaw,
		`{"sessionUpdate":"state_update","state":"idle","stopReason":"error","error":{"code":-32000,"message":"Authentication required","data":{"hint":"login"}}}`)
	result := completedWork(t, tracker)
	if result.StopReason == nil || *result.StopReason != schema.StopReasonError {
		t.Fatalf("stop reason = %v", result.StopReason)
	}
	if result.Text != "partial output" {
		t.Fatalf("projected text = %q", result.Text)
	}
	var failed *WorkError
	if err := result.Err(); !errors.As(err, &failed) {
		t.Fatalf("Err = %v, want *WorkError", err)
	}
	if failed.Failure == nil || failed.Failure != result.Error ||
		failed.Failure.Code != -32000 || failed.Failure.Message != "Authentication required" {
		t.Fatalf("failure = %+v", failed.Failure)
	}
	data, err := json.Marshal(failed.Failure.Data)
	if err != nil || string(data) != `{"hint":"login"}` {
		t.Fatalf("failure data = %s, %v", data, err)
	}
	if got := failed.Error(); got != "agent reported that foreground work failed: ACP error -32000: Authentication required" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestActiveWorkReportsErrorStopReasonWithoutDetails(t *testing.T) {
	for name, idle := range map[string]string{
		"omitted":   `"stopReason":"error"`,
		"null":      `"stopReason":"error","error":null`,
		"malformed": `"stopReason":"error","error":"boom"`,
	} {
		t.Run(name, func(t *testing.T) {
			tracker := NewSessionTracker()
			observeRaw(t, tracker, runningRaw, `{"sessionUpdate":"state_update","state":"idle",`+idle+`}`)
			result := completedWork(t, tracker)
			var failed *WorkError
			if !errors.As(result.Err(), &failed) || failed.Failure != nil || result.Error != nil {
				t.Fatalf("result = %+v, Err = %v", result, result.Err())
			}
			if got := failed.Error(); got != "agent reported that foreground work failed" {
				t.Fatalf("Error() = %q", got)
			}
		})
	}
}

// An idle update ends foreground work whatever its stop reason: the v2 prompt
// lifecycle requires clients to recognize idle even when the reason is
// omitted, null, malformed, or unknown, and background work keeps running
// independently of it.
func TestActiveWorkCompletesOnAnyIdle(t *testing.T) {
	for name, test := range map[string]struct {
		idle   string
		reason *schema.StopReason
		fields map[string]json.RawMessage
	}{
		"omitted":   {idle: ``},
		"null":      {idle: `,"stopReason":null`},
		"malformed": {idle: `,"stopReason":["end_turn"]`},
		"future":    {idle: `,"stopReason":"future_reason"`, reason: new(schema.StopReason("future_reason"))},
		"custom": {
			idle:   `,"stopReason":"_paused","resumeAt":"tomorrow","error":{"code":1,"message":"kept"}`,
			reason: new(schema.StopReason("_paused")),
			fields: map[string]json.RawMessage{
				"resumeAt": json.RawMessage(`"tomorrow"`),
				"error":    json.RawMessage(`{"code":1,"message":"kept"}`),
			},
		},
		"background": {
			idle:   `,"stopReason":"end_turn","background":[{"type":"terminal","terminalId":"t"},{"type":"future_work"}]`,
			reason: new(schema.StopReasonEndTurn),
		},
	} {
		t.Run(name, func(t *testing.T) {
			tracker := NewSessionTracker()
			observeRaw(t, tracker, runningRaw, chunkRaw, `{"sessionUpdate":"state_update","state":"idle"`+test.idle+`}`)
			result := completedWork(t, tracker)
			if !reflect.DeepEqual(result.StopReason, test.reason) {
				t.Fatalf("stop reason = %v, want %v", result.StopReason, test.reason)
			}
			if !reflect.DeepEqual(result.StopReasonFields, test.fields) {
				t.Fatalf("stop reason fields = %v, want %v", result.StopReasonFields, test.fields)
			}
			if result.Text != "partial output" || result.Error != nil || result.Err() != nil {
				t.Fatalf("result = %+v, Err = %v", result, result.Err())
			}
		})
	}
}

// Like the reference one-shot client, any state other than idle shows that
// the foreground work started, including requires_action and unknown states.
func TestActiveWorkStartsOnAnyActiveState(t *testing.T) {
	for name, state := range map[string]string{
		"requires_action": `{"sessionUpdate":"state_update","state":"requires_action"}`,
		"unknown state":   `{"sessionUpdate":"state_update","state":"future_state"}`,
	} {
		t.Run(name, func(t *testing.T) {
			tracker := NewSessionTracker()
			observeRaw(t, tracker, state, chunkRaw, `{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}`)
			if result := completedWork(t, tracker); result.Text != "partial output" {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

// Repeated idle snapshots, which may carry the last stop reason for context,
// are not new completions: the result stays the one from the idle update that
// ended the work, and later output does not join it.
func TestActiveWorkIgnoresRepeatedIdle(t *testing.T) {
	tracker := NewSessionTracker()
	observeRaw(t, tracker, runningRaw, chunkRaw, `{"sessionUpdate":"state_update","state":"idle"}`)
	first := completedWork(t, tracker)
	observeRaw(t, tracker,
		`{"sessionUpdate":"state_update","state":"idle","stopReason":"cancelled","background":[]}`,
		`{"sessionUpdate":"agent_message_chunk","messageId":"later","content":{"type":"text","text":" later"}}`,
		runningRaw,
		`{"sessionUpdate":"state_update","state":"idle","stopReason":"error","error":{"code":-32603,"message":"late"}}`,
	)
	if again := tracker.Work("session").Result(); !reflect.DeepEqual(again, first) {
		t.Fatalf("result after repeated idle = %+v, want %+v", again, first)
	}
	if first.StopReason != nil || first.Text != "partial output" {
		t.Fatalf("first result = %+v", first)
	}
}

// A resume snapshot or replay without a preceding active state never
// completes the work, even when it carries an old stop reason.
func TestActiveWorkIgnoresIdleSnapshotBeforeActivity(t *testing.T) {
	tracker := NewSessionTracker()
	observeRaw(t, tracker, `{"sessionUpdate":"state_update","state":"idle","stopReason":"cancelled"}`)
	select {
	case <-tracker.Work("session").Done():
		t.Fatal("idle snapshot completed work that never started")
	default:
	}
}

func TestIdleStateUpdateConstructors(t *testing.T) {
	failure := &schema.Error{Code: -32000, Message: "Authentication required"}
	for name, test := range map[string]struct {
		update schema.SessionUpdate
		want   string
	}{
		"end turn":      {NewIdleStateUpdate(schema.StopReasonEndTurn), `{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}`},
		"no reason":     {NewIdleStateUpdate(""), `{"sessionUpdate":"state_update","state":"idle"}`},
		"custom":        {NewIdleStateUpdate("_paused"), `{"sessionUpdate":"state_update","state":"idle","stopReason":"_paused"}`},
		"error":         {NewErrorIdleStateUpdate(failure), `{"sessionUpdate":"state_update","state":"idle","stopReason":"error","error":{"code":-32000,"message":"Authentication required"}}`},
		"error no data": {NewErrorIdleStateUpdate(nil), `{"sessionUpdate":"state_update","state":"idle","stopReason":"error"}`},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(test.update)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(test.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("encoded %s, want %s", encoded, test.want)
			}
			var decoded schema.SessionUpdate
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, test.update) {
				t.Fatalf("decoded %+v, want %+v", decoded, test.update)
			}
		})
	}
}

// cancelBarrierHandle returns a session handle whose agent answers
// session/cancel with the given idle update after reporting running.
func cancelBarrierHandle(t *testing.T, idle schema.SessionUpdate) *SessionHandle {
	t.Helper()
	tracker := NewSessionTracker()
	var server *acp.Connection
	notify := func(update schema.SessionUpdate) error {
		return server.Notify(context.Background(), schema.SessionUpdateMethodName, Update{SessionID: "cancel-session", Update: update})
	}
	client, server := pipeClientWithServerNotifications(t,
		func(_ context.Context, method string, _ json.RawMessage) (any, error) {
			switch method {
			case schema.InitializeMethodName:
				return sessionInitialization(), nil
			case schema.SessionNewMethodName:
				return schema.NewSessionResponse{SessionID: "cancel-session"}, nil
			case schema.SessionPromptMethodName:
				if err := notify(schema.SessionUpdate{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}}); err != nil {
					return nil, err
				}
				return schema.PromptResponse{MessageID: "user-message"}, nil
			default:
				return nil, &acp.RPCError{Code: -32601}
			}
		},
		tracker.TrackerNotifications(nil),
		func(method string, _ json.RawMessage) error {
			if method != schema.SessionCancelMethodName {
				return nil
			}
			return notify(idle)
		},
	)
	ctx := context.Background()
	initialization, err := client.InitializeWithInfo(ctx, Capabilities{}, ClientInfo{Name: "cancel-client", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSessionWithOptions(ctx, initialization, hostRoot+"/repo", NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := NewSessionHandle(client, initialization, session, SessionHandleOptions{Tracker: tracker})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Prompt(ctx, "long work"); err != nil {
		t.Fatal(err)
	}
	return handle
}

// Only cancelled confirms the cancellation barrier (agent-client-protocol
// #2523, #2576): an agent must report cancelled, not error, even when
// aborting raised errors, and any other reason means the work ended for
// another cause.
func TestCancelActiveWorkAndWaitRequiresCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := cancelBarrierHandle(t, NewIdleStateUpdate(schema.StopReasonCancelled)).CancelActiveWorkAndWait(ctx)
	if err != nil || result.StopReason == nil || *result.StopReason != schema.StopReasonCancelled {
		t.Fatalf("cancelled: result %+v, err %v", result, err)
	}

	for name, test := range map[string]struct {
		idle    schema.SessionUpdate
		message string
		failed  bool
	}{
		"error": {
			idle:    NewErrorIdleStateUpdate(&schema.Error{Code: -32603, Message: "abort failed"}),
			message: `active work became idle without the cancelled stop reason (stop reason "error"): agent reported that foreground work failed: ACP error -32603: abort failed`,
			failed:  true,
		},
		"end turn": {
			idle:    NewIdleStateUpdate(schema.StopReasonEndTurn),
			message: `active work became idle without the cancelled stop reason (stop reason "end_turn")`,
		},
		"unknown": {
			idle:    NewIdleStateUpdate("_stopped"),
			message: `active work became idle without the cancelled stop reason (stop reason "_stopped")`,
		},
		"none": {
			idle:    NewIdleStateUpdate(""),
			message: `active work became idle without the cancelled stop reason (no stop reason)`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := cancelBarrierHandle(t, test.idle).CancelActiveWorkAndWait(ctx)
			if !errors.Is(err, ErrWorkNotCancelled) || err.Error() != test.message {
				t.Fatalf("err = %v", err)
			}
			var failed *WorkError
			if errors.As(err, &failed) != test.failed {
				t.Fatalf("errors.As WorkError = %v, want %v", !test.failed, test.failed)
			}
			if !reflect.DeepEqual(result.StopReason, test.idle.StateUpdate.Idle.StopReason) {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}
