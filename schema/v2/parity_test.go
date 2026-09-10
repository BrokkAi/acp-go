package v2

import (
	"encoding/json"
	"reflect"
	"testing"
)

type parityCase struct {
	name    string
	typ     reflect.Type
	minimal string
	full    string
}

func canonical(t *testing.T, doc string) string {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(doc), &value); err != nil {
		t.Fatalf("fixture is not JSON: %v\n%s", err, doc)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("remarshal: %v", err)
	}
	return string(encoded)
}

func TestParityRoundTrip(t *testing.T) {
	for _, tc := range parityCases {
		t.Run(tc.name, func(t *testing.T) {
			for label, doc := range map[string]string{"minimal": tc.minimal, "full": tc.full} {
				want := canonical(t, doc)
				value := reflect.New(tc.typ)
				if err := json.Unmarshal([]byte(doc), value.Interface()); err != nil {
					t.Fatalf("%s: decode: %v\n%s", label, err, doc)
				}
				got, err := json.Marshal(value.Elem().Interface())
				if err != nil {
					t.Fatalf("%s: encode: %v", label, err)
				}
				if canonical(t, string(got)) != want {
					t.Errorf("%s: round trip mismatch\n got: %s\nwant: %s", label, got, want)
				}
				again, err := json.Marshal(value.Elem().Interface())
				if err != nil || canonical(t, string(again)) != want {
					t.Errorf("%s: second encode differs", label)
				}
			}
		})
	}
}

func TestMethodRegistry(t *testing.T) {
	seen := map[string]bool{}
	for name, method := range Methods {
		if method.Name != name {
			t.Errorf("registry key %q carries name %q", name, method.Name)
		}
		if seen[name] {
			t.Errorf("duplicate method %q", name)
		}
		seen[name] = true
		switch method.Side {
		case SideAgent, SideClient, SideProtocol:
		default:
			t.Errorf("method %q has unknown side %q", name, method.Side)
		}
		if method.Notification && method.Result != nil {
			t.Errorf("notification %q declares a result type", name)
		}
	}
	for _, required := range []string{
		InitializeMethodName, AuthLoginMethodName, AuthLogoutMethodName,
		SessionNewMethodName, SessionPromptMethodName, SessionCancelMethodName,
		SessionCloseMethodName, SessionUpdateMethodName,
		SessionRequestPermissionMethodName, ElicitationCreateMethodName,
		ElicitationCompleteMethodName, CancelRequestMethodName,
	} {
		if !seen[required] {
			t.Errorf("method %q missing from registry", required)
		}
	}
}
