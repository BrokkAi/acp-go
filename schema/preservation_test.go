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
			{Key: "traceparent", Value: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
			{Key: "extension", Value: Meta{
				{Key: "nested", Value: []any{"a", int64(2), 2.5, true, uint64(18446744073709551615), nil}},
			}},
			{Key: "a", Value: int64(-9223372036854775808)},
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

// TestOpenWorldEnumsDecodeUnknownValues pins the second guardrail: an enum
// the schema leaves open (it has a catch-all string member) decodes and
// re-encodes a value this release does not name. A closed v1 enum decodes as
// Rust's does: an unknown value fails, or reads as its #[serde(other)] member
// (ToolKind's "other").
func TestOpenWorldEnumsDecodeUnknownValues(t *testing.T) {
	var notice Notice
	if err := json.Unmarshal([]byte(`{"severity":"future_severity","title":"t","message":"m"}`), &notice); err != nil {
		t.Fatal(err)
	}
	if notice.Severity != NoticeSeverity("future_severity") {
		t.Fatalf("severity = %q", notice.Severity)
	}
	encoded, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"future_severity"`) {
		t.Fatalf("unknown severity was not preserved: %s", encoded)
	}

	var response PromptResponse
	if err := json.Unmarshal([]byte(`{"stopReason":"future_reason"}`), &response); err == nil {
		t.Fatalf("closed stop reason accepted an unknown value: %q", response.StopReason)
	}

	var call ToolCall
	if err := json.Unmarshal([]byte(`{"toolCallId":"t","title":"x","kind":"future_kind"}`), &call); err != nil {
		t.Fatal(err)
	}
	if call.Kind == nil || *call.Kind != ToolKindOther {
		t.Fatalf("tool kind = %v, want other", call.Kind)
	}
}
