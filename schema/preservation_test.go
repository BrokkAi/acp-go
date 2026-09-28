package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestMetaPayloadsRoundTrip pins the extensibility guardrail: the reserved
// _meta channel carries arbitrary extension data that must survive a decode and
// re-encode unchanged. (Draft v2's Nullable[Meta] is covered by the
// tracecontext package tests.)
func TestMetaPayloadsRoundTrip(t *testing.T) {
	original := AuthenticateRequest{
		MethodID: "api-key",
		Meta: Meta{
			"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			"extension": map[string]any{
				"nested": []any{"a", float64(2), true},
			},
		},
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded AuthenticateRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Meta, original.Meta) {
		t.Fatalf("_meta changed across the round trip: %#v", decoded.Meta)
	}

	reencoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(reencoded) != string(encoded) {
		t.Fatalf("re-encoded frame differs:\n%s\n%s", encoded, reencoded)
	}
}

// TestOpenWorldEnumsDecodeUnknownValues pins the second guardrail: enum string
// types carry no closed set, so a value this schema release does not name must
// still decode and re-encode instead of failing.
func TestOpenWorldEnumsDecodeUnknownValues(t *testing.T) {
	var response PromptResponse
	if err := json.Unmarshal([]byte(`{"stopReason":"future_reason"}`), &response); err != nil {
		t.Fatal(err)
	}
	if response.StopReason != StopReason("future_reason") {
		t.Fatalf("stop reason = %q", response.StopReason)
	}

	var call ToolCall
	if err := json.Unmarshal([]byte(`{"toolCallId":"t","title":"x","kind":"future_kind"}`), &call); err != nil {
		t.Fatal(err)
	}
	if call.Kind == nil || *call.Kind != ToolKind("future_kind") {
		t.Fatalf("tool kind = %v", call.Kind)
	}
	encoded, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"future_kind"`) {
		t.Fatalf("unknown tool kind was not preserved: %s", encoded)
	}
}
