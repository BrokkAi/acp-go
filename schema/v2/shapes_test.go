package v2

import (
	"encoding/json"
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
