package unstable

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

// The subagent idle state flattens its own open stop-reason union. The v1
// prompt StopReason keeps its def, so the flattened enum is prefixed.
func TestSubagentIdleStopReason(t *testing.T) {
	var failed IdleStateUpdate
	roundTrip(t, `{"stopReason":"error","error":{"code":-32603,"message":"boom"}}`, &failed)
	if failed.StopReason == nil || *failed.StopReason != IdleStateUpdateStopReasonError || failed.Error == nil {
		t.Fatalf("error stop = %+v", failed)
	}
	var custom IdleStateUpdate
	roundTrip(t, `{"stopReason":"_vendor"}`, &custom)
	if custom.StopReason == nil || *custom.StopReason != IdleStateUpdateStopReason("_vendor") {
		t.Fatalf("custom stop = %+v", custom)
	}
	var prompt PromptResponse
	roundTrip(t, `{"stopReason":"end_turn"}`, &prompt)
	if prompt.StopReason != StopReasonEndTurn {
		t.Fatalf("prompt stop reason = %q", prompt.StopReason)
	}
}

func TestProxyChainShapes(t *testing.T) {
	// proxy/initialize reuses the initialize payloads unchanged.
	var initialize InitializeProxyRequest = InitializeRequest{ProtocolVersion: 1}
	if _, err := json.Marshal(initialize); err != nil {
		t.Fatal(err)
	}
	if Methods[ProxyInitializeMethodName].Params != Methods[InitializeMethodName].Params {
		t.Fatal("proxy/initialize params are not the initialize params")
	}

	var request ProxySuccessorRequest
	roundTrip(t, `{"method":"session/prompt","params":{"sessionId":"s","_meta":{"k":1}},"_meta":{"hop":true}}`, &request)
	if request.Method != "session/prompt" || string(request.Params) != `{"sessionId":"s","_meta":{"k":1}}` {
		t.Fatalf("successor request = %+v", request)
	}

	// The wrapped result is forwarded unchanged, whatever its shape.
	for _, wire := range []string{`{"stopReason":"end_turn","extra":[1]}`, `null`, `"text"`} {
		var result ProxySuccessorResponse
		if err := json.Unmarshal([]byte(wire), &result); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(result)
		if err != nil || string(encoded) != wire {
			t.Fatalf("successor result %s re-encoded as %s (%v)", wire, encoded, err)
		}
	}
}
