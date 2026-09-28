package tracecontext

import (
	"encoding/hex"
	"strings"
)

// Root-level _meta keys reserved for W3C Trace Context by the ACP
// extensibility conventions.
const (
	TraceparentKey = "traceparent"
	TracestateKey  = "tracestate"
	BaggageKey     = "baggage"
)

// Context is the W3C Trace Context attached to a message's _meta channel.
// Traceparent holds the fixed-grammar "traceparent" value; Tracestate and
// Baggage hold the opaque "tracestate" and "baggage" values. Empty fields are
// omitted when writing and reported as empty when reading.
type Context struct {
	Traceparent string
	Tracestate  string
	Baggage     string
}

// Empty reports whether the context carries no values.
func (tc Context) Empty() bool {
	return tc.Traceparent == "" && tc.Tracestate == "" && tc.Baggage == ""
}

// FromMeta extracts the reserved W3C trace-context keys from a generated _meta
// map such as schema.Meta or v2.Meta.
//
// It reports whether any of the three keys held a non-empty string. Non-string
// values are ignored rather than removed, so a peer that stores unrelated data
// at a reserved key is left untouched. Decoding a v2 Nullable[Meta] field first
// (for example request.Meta.Value) remains the caller's responsibility.
func FromMeta(meta map[string]any) (Context, bool) {
	tc := Context{
		Traceparent: metaString(meta, TraceparentKey),
		Tracestate:  metaString(meta, TracestateKey),
		Baggage:     metaString(meta, BaggageKey),
	}
	return tc, !tc.Empty()
}

// IntoMeta writes the non-empty fields of tc to the reserved keys of meta and
// returns the result. A nil map is allocated; otherwise meta is updated in
// place so a generated Meta keeps its other keys. Fields left empty in tc do
// not change meta, and an empty tc returns meta unchanged.
func IntoMeta(meta map[string]any, tc Context) map[string]any {
	if tc.Empty() {
		return meta
	}
	if meta == nil {
		meta = make(map[string]any, 3)
	}
	if tc.Traceparent != "" {
		meta[TraceparentKey] = tc.Traceparent
	}
	if tc.Tracestate != "" {
		meta[TracestateKey] = tc.Tracestate
	}
	if tc.Baggage != "" {
		meta[BaggageKey] = tc.Baggage
	}
	return meta
}

func metaString(meta map[string]any, key string) string {
	value, ok := meta[key].(string)
	if !ok {
		return ""
	}
	return value
}

// ValidTraceparent reports whether value is a well-formed W3C traceparent:
// "version-trace-id-span-id-flags" in lowercase hex, with a non-reserved
// version, non-zero trace and span IDs, and no trailing fields for version 00.
// Higher versions may append fields that this helper accepts and ignores,
// matching the W3C forward-compatibility rule.
func ValidTraceparent(value string) bool {
	if len(value) < 55 || value[2] != '-' || value[35] != '-' || value[52] != '-' {
		return false
	}
	version, traceID, spanID, flags := value[0:2], value[3:35], value[36:52], value[53:55]
	if !isLowerHex(version) || version == "ff" {
		return false
	}
	if !isLowerHex(traceID) || traceID == strings.Repeat("0", 32) {
		return false
	}
	if !isLowerHex(spanID) || spanID == strings.Repeat("0", 16) {
		return false
	}
	if !isLowerHex(flags) {
		return false
	}
	return version != "00" || len(value) == 55
}

func isLowerHex(value string) bool {
	if value == "" || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
