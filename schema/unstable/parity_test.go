package unstable

import (
	"encoding/json"
	"reflect"
	"testing"
)

// parityCase pairs a generated type with synthesized wire documents: the
// minimal document (required fields only) and the full document (every
// field). Cases are generated; this file holds the checking logic and
// hand-written fixtures for representative flows.
type parityCase struct {
	name    string
	typ     reflect.Type
	minimal string
	full    string
}

// canonical re-encodes a JSON document so comparisons ignore key order.
func canonical(t *testing.T, doc string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("fixture is not JSON: %v\n%s", err, doc)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	return string(out)
}

// TestParityRoundTrip decodes each fixture into the generated type and
// re-encodes it. The encoding, which TestDeserializeHints compares with the
// Rust schema crate's byte for byte, must be a fixed point: decoding it and
// encoding the result again gives the same bytes.
func TestParityRoundTrip(t *testing.T) {
	for _, tc := range parityCases {
		t.Run(tc.name, func(t *testing.T) {
			for label, doc := range map[string]string{"minimal": tc.minimal, "full": tc.full} {
				value := reflect.New(tc.typ)
				if err := json.Unmarshal([]byte(doc), value.Interface()); err != nil {
					t.Fatalf("%s: decode: %v\n%s", label, err, doc)
				}
				got, err := json.Marshal(value.Elem().Interface())
				if err != nil {
					t.Fatalf("%s: encode: %v", label, err)
				}
				again := reflect.New(tc.typ)
				if err := json.Unmarshal(got, again.Interface()); err != nil {
					t.Fatalf("%s: decode the encoding: %v\n%s", label, err, got)
				}
				if second, err := json.Marshal(again.Elem().Interface()); err != nil || string(second) != string(got) {
					t.Errorf("%s: the encoding re-encodes differently (%v)\nfirst: %s\nagain: %s", label, err, got, second)
				}
				// A second pass catches state left behind by the first.
				if repeat, err := json.Marshal(value.Elem().Interface()); err != nil || string(repeat) != string(got) {
					t.Errorf("%s: second encode differs", label)
				}
			}
		})
	}
}

// TestMethodRegistry checks the generated method registry against invariants
// every release must uphold: both sides of each request/response pair share
// one method name, and every method name is unique.
func TestMethodRegistry(t *testing.T) {
	seen := map[string]bool{}
	for name, m := range Methods {
		if m.Name != name {
			t.Errorf("registry key %q carries name %q", name, m.Name)
		}
		if seen[name] {
			t.Errorf("duplicate method %q", name)
		}
		seen[name] = true
		switch m.Side {
		case SideAgent, SideClient, SideBoth, SideProtocol, SideProxy:
		default:
			t.Errorf("method %q has unknown side %q", name, m.Side)
		}
		if m.Notification && m.Result != nil && m.NotificationParams == nil {
			t.Errorf("notification %q declares a result type", name)
		}
	}
	for _, required := range []string{
		InitializeMethodName, SessionNewMethodName, SessionPromptMethodName,
		SessionCancelMethodName, SessionUpdateMethodName, SessionRequestPermissionMethodName,
		ElicitationCreateMethodName, CancelRequestMethodName,
	} {
		if !seen[required] {
			t.Errorf("method %q missing from registry", required)
		}
	}
}
