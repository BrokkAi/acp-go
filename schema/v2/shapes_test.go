package v2

import (
	"encoding/json"
	"reflect"
	"testing"
)

func roundTrip(t *testing.T, wire string, value any) {
	t.Helper()
	if err := json.Unmarshal([]byte(wire), value); err != nil {
		t.Fatalf("decode %s: %v", wire, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if canonical(t, string(encoded)) != canonical(t, wire) {
		t.Fatalf("round trip mismatch\n got: %s\nwant: %s", encoded, wire)
	}
}

// The schema-v2.0.0-alpha.13 idle state flattens an open stop-reason union
// into the update: known reasons keep the StopReason constants, the error
// reason carries its JSON-RPC error beside it, and custom or future reasons
// decode unchanged.
func TestIdleStopReasonUnion(t *testing.T) {
	var ended StateUpdate
	roundTrip(t, `{"state":"idle","stopReason":"end_turn"}`, &ended)
	if ended.Idle == nil || ended.Idle.StopReason == nil || *ended.Idle.StopReason != StopReasonEndTurn {
		t.Fatalf("end_turn = %+v", ended.Idle)
	}

	var failed IdleStateUpdate
	roundTrip(t, `{"stopReason":"error","error":{"code":-32603,"message":"boom","data":{"sessionId":"s"}}}`, &failed)
	if failed.StopReason == nil || *failed.StopReason != StopReasonError {
		t.Fatalf("stop reason = %v", failed.StopReason)
	}
	if failed.Error == nil || failed.Error.Code != ErrorCodeInternalError || failed.Error.Data == nil ||
		failed.Error.Data.SessionNotFound == nil || failed.Error.Data.SessionNotFound.SessionID != "s" {
		t.Fatalf("error details = %+v", failed.Error)
	}

	var custom IdleStateUpdate
	roundTrip(t, `{"stopReason":"_vendor_paused"}`, &custom)
	if custom.StopReason == nil || *custom.StopReason != StopReason("_vendor_paused") {
		t.Fatalf("custom stop reason = %v", custom.StopReason)
	}

	for _, wire := range []string{`{}`, `{"stopReason":null}`} {
		var none IdleStateUpdate
		if err := json.Unmarshal([]byte(wire), &none); err != nil {
			t.Fatal(err)
		}
		if none.StopReason != nil {
			t.Fatalf("%s: stop reason = %v, want not reported", wire, *none.StopReason)
		}
	}
}

// rustFixtures are decode/re-encode pairs produced by the Rust
// agent-client-protocol-schema 1.15.1 crate (serde_json from_str then
// to_string). Go must reach the same JSON value.
var rustFixtures = []struct {
	name    string
	typ     any
	in, out string
}{
	{"idle stop reason object", IdleStateUpdate{}, `{"stopReason":{"x":1}}`, `{}`},
	{"idle stop reason null", IdleStateUpdate{}, `{"stopReason":null}`, `{}`},
	{"idle error not an object", IdleStateUpdate{}, `{"stopReason":"error","error":"boom"}`, `{"stopReason":"error"}`},
	{"idle error bad code", IdleStateUpdate{}, `{"stopReason":"error","error":{"code":"x","message":"m"}}`, `{"stopReason":"error"}`},
	{"idle error on end_turn", IdleStateUpdate{}, `{"stopReason":"end_turn","error":{"code":1,"message":"m"}}`, `{"stopReason":"end_turn"}`},
	{"idle error without reason", IdleStateUpdate{}, `{"error":{"code":1,"message":"m"}}`, `{}`},
	{"idle custom extra", IdleStateUpdate{}, `{"stopReason":"_x","detail":1}`, `{"stopReason":"_x","detail":1}`},
	{"idle custom with error", IdleStateUpdate{}, `{"stopReason":"_x","error":{"code":1,"message":"m"},"detail":{"a":[1]}}`, `{"stopReason":"_x","detail":{"a":[1]},"error":{"code":1,"message":"m"}}`},
	{"idle known reason extra", IdleStateUpdate{}, `{"stopReason":"end_turn","detail":1}`, `{"stopReason":"end_turn"}`},
	{"idle error with data", IdleStateUpdate{}, `{"stopReason":"error","error":{"code":-32603,"message":"boom","data":{"sessionId":"s","extra":1}}}`, `{"stopReason":"error","error":{"code":-32603,"message":"boom","data":{"sessionId":"s","extra":1}}}`},
	{"idle background string", IdleStateUpdate{}, `{"background":"x"}`, `{}`},
	{"idle background bad item", IdleStateUpdate{}, `{"background":[{"type":"terminal","terminalId":5}]}`, `{}`},
	{"idle meta number", IdleStateUpdate{}, `{"_meta":5}`, `{}`},
	{"idle bad meta kept background", IdleStateUpdate{}, `{"_meta":"x","background":[],"stopReason":"cancelled"}`, `{"stopReason":"cancelled","background":[]}`},
	{"idle bad reason kept background", IdleStateUpdate{}, `{"stopReason":{"n":7},"background":[{"type":"terminal","terminalId":"t"}]}`, `{"background":[{"type":"terminal","terminalId":"t"}]}`},
	{"notification custom reason", UpdateSessionNotification{}, `{"sessionId":"s","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"_x","detail":1}}`, `{"sessionId":"s","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"_x","detail":1}}`},
	{"notification malformed background", UpdateSessionNotification{}, `{"sessionId":"s","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn","background":"x","_meta":5}}`, `{"sessionId":"s","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}}`},
	{"running malformed background", RunningStateUpdate{}, `{"background":{"x":1},"_meta":[1]}`, `{}`},
	{"requires action malformed background", RequiresActionStateUpdate{}, `{"background":7}`, `{}`},
	{"error data session extra", Error{}, `{"code":-32002,"message":"m","data":{"sessionId":"s","extra":1}}`, `{"code":-32002,"message":"m","data":{"sessionId":"s","extra":1}}`},
	{"error data version extra", Error{}, `{"code":-32022,"message":"m","data":{"requested":3,"supported":[1,2],"note":"x"}}`, `{"code":-32022,"message":"m","data":{"requested":3,"supported":[1,2],"note":"x"}}`},
	{"error data string", Error{}, `{"code":-32603,"message":"m","data":"details"}`, `{"code":-32603,"message":"m","data":"details"}`},
	// Not mirrored: Rust reads an integer stopReason (e.g. 5) as "error"
	// through serde's flatten buffering. Go reads every non-string value as
	// not reported, which is what the schema's default-on-error asks for.
}

func TestRustDecodeParity(t *testing.T) {
	for _, fixture := range rustFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			value := reflect.New(reflect.TypeOf(fixture.typ))
			if err := json.Unmarshal([]byte(fixture.in), value.Interface()); err != nil {
				t.Fatalf("decode: %v", err)
			}
			encoded, err := json.Marshal(value.Elem().Interface())
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if canonical(t, string(encoded)) != canonical(t, fixture.out) {
				t.Fatalf("got %s, Rust %s", encoded, fixture.out)
			}
		})
	}
}

func TestCustomStopReasonKeepsOnlyItsOwnFields(t *testing.T) {
	var notification UpdateSessionNotification
	wire := `{"sessionId":"s","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"_x","detail":1}}`
	if err := json.Unmarshal([]byte(wire), &notification); err != nil {
		t.Fatal(err)
	}
	idle := notification.Update.StateUpdate.Idle
	if len(idle.StopReasonFields) != 1 || string(idle.StopReasonFields["detail"]) != "1" {
		t.Fatalf("custom fields = %v", idle.StopReasonFields)
	}
	// Switching to a known reason drops the custom fields, as Rust does.
	reason := StopReasonEndTurn
	idle.StopReason = &reason
	encoded, err := json.Marshal(idle)
	if err != nil || canonical(t, string(encoded)) != `{"stopReason":"end_turn"}` {
		t.Fatalf("encoded %s (%v)", encoded, err)
	}
}

func TestErrorDataReencodesChangedVariant(t *testing.T) {
	wire := `{"sessionId":"s","extra":1}` // key order is kept
	var data ErrorData
	if err := json.Unmarshal([]byte(wire), &data); err != nil {
		t.Fatal(err)
	}
	if encoded, err := json.Marshal(data); err != nil || string(encoded) != wire {
		t.Fatalf("unchanged data re-encoded as %s (%v), want the wire bytes", encoded, err)
	}
	data.SessionNotFound.SessionID = "t"
	if encoded, err := json.Marshal(data); err != nil || canonical(t, string(encoded)) != `{"sessionId":"t"}` {
		t.Fatalf("changed data re-encoded as %s (%v)", encoded, err)
	}
}

func TestErrorDataShapes(t *testing.T) {
	if ErrorCodeUnsupportedProtocolVersion != -32022 {
		t.Fatalf("unsupported protocol version code = %d", ErrorCodeUnsupportedProtocolVersion)
	}

	var unsupported Error
	roundTrip(t, `{"code":-32022,"message":"Unsupported protocol version","data":{"requested":3,"supported":[1,2]}}`, &unsupported)
	if data := unsupported.Data; data == nil || data.UnsupportedProtocolVersion == nil ||
		data.UnsupportedProtocolVersion.Requested != 3 || len(data.UnsupportedProtocolVersion.Supported) != 2 {
		t.Fatalf("unsupported version data = %+v", unsupported.Data)
	}

	// Values that match no defined shape, including primitives and a
	// mistyped defined shape, are kept verbatim.
	for _, data := range []string{`"details"`, `42`, `[1,2]`, `{"custom":true}`, `{"sessionId":7}`} {
		var other Error
		roundTrip(t, `{"code":-32603,"message":"x","data":`+data+`}`, &other)
		if other.Data == nil || other.Data.Other == nil || string(other.Data.Other.Raw) != data {
			t.Fatalf("data %s decoded as %+v", data, other.Data)
		}
	}

	var bare Error
	roundTrip(t, `{"code":-32603,"message":"x"}`, &bare)
	if bare.Data != nil {
		t.Fatalf("absent data decoded as %+v", bare.Data)
	}
}

func TestAvailableCommandAndBackgroundWorkUnions(t *testing.T) {
	var command AvailableCommand
	roundTrip(t, `{"type":"command","name":"plan","description":"Make a plan"}`, &command)
	if command.Command == nil || command.Command.Name != "plan" || command.Kind != AvailableCommandKindCommand {
		t.Fatalf("command = %+v", command)
	}
	var future AvailableCommand
	roundTrip(t, `{"type":"_skill","name":"x","description":"y","extra":1}`, &future)
	if future.Other == nil || future.Kind != AvailableCommandKind("_skill") {
		t.Fatalf("future command = %+v", future)
	}

	var update RunningStateUpdate
	roundTrip(t, `{"background":[{"type":"terminal","terminalId":"t"},{"type":"_watch","id":"w"}]}`, &update)
	work := update.Background.Value
	if len(work) != 2 || work[0].Terminal == nil || work[0].Terminal.TerminalID != "t" || work[1].Other == nil {
		t.Fatalf("background = %+v", update.Background)
	}
}
