package unstable

import (
	"encoding/json"
	"testing"
)

// authStatusDecodeCases were captured from agent-client-protocol-schema 1.15.1
// (feature unstable_auth_status): the input, and Rust's re-encoding of the
// decoded value, or "" where Rust refuses the message.
var authStatusDecodeCases = []struct{ wire, want string }{
	{`{"state":"authenticated"}`, `{"state":"authenticated"}`},
	{`{"state":"_vendor"}`, `{"state":"_vendor"}`},
	{`{"state":"future"}`, `{"state":"future"}`},
	{`{"state":5}`, ""},
	{`{"state":null}`, ""},
	{`{}`, ""},
	{`{"state":"unknown","accounts":null}`, `{"state":"unknown"}`},
	{`{"state":"unknown","accounts":"x"}`, `{"state":"unknown"}`},
	{`{"state":"unknown","accounts":[1,{"label":"a"},{"name":"b"},{"label":null},{"label":"c","active":"yes","name":5,"email":[],"organization":{},"_meta":7},{"label":5}]}`, `{"state":"unknown","accounts":[{"label":"a"},{"label":"c"}]}`},
	{`{"state":"unknown","accounts":[null,{"label":"a"}]}`, `{"state":"unknown","accounts":[{"label":"a"}]}`},
	{`{"state":"unknown","_meta":5}`, `{"state":"unknown"}`},
	{`{"state":"unknown","accounts":[{"label":"a","extra":1}]}`, `{"state":"unknown","accounts":[{"label":"a"}]}`},
	{`{"state":"unknown","accounts":[{"label":"a","active":null,"name":null}]}`, `{"state":"unknown","accounts":[{"label":"a"}]}`},
	{`{"state":"authenticated","accounts":[{"label":"Key","active":true,"name":"octocat","email":"user@example.com","organization":"Example","_meta":{"k":"v"}},{"label":"Vertex AI"}]}`, `{"state":"authenticated","accounts":[{"label":"Key","active":true,"name":"octocat","email":"user@example.com","organization":"Example","_meta":{"k":"v"}},{"label":"Vertex AI"}]}`},
}

// The auth-status RFD requires a receiver to ignore a malformed accounts
// value, or skip a malformed entry, while state stays required and strict.
func TestAuthStatusDecodesAsRust(t *testing.T) {
	for _, c := range authStatusDecodeCases {
		check := func(name string, value any) {
			err := json.Unmarshal([]byte(c.wire), value)
			if c.want == "" {
				if err == nil {
					t.Errorf("%s %s decoded, want an error", name, c.wire)
				}
				return
			}
			if err != nil {
				t.Errorf("%s %s: %v", name, c.wire, err)
				return
			}
			encoded, _ := json.Marshal(value)
			if canonical(t, string(encoded)) != canonical(t, c.want) {
				t.Errorf("%s %s re-encoded as %s, want %s", name, c.wire, encoded, c.want)
			}
		}
		check("response", new(AuthStatusResponse))
		check("notification", new(AuthStatusUpdateNotification))
	}
	for _, wire := range []string{`null`, `{}`, `{"_meta":5}`} {
		var request AuthStatusRequest
		if err := json.Unmarshal([]byte(wire), &request); err != nil || request.Meta != nil {
			t.Errorf("request %s = %+v, %v", wire, request, err)
		}
	}
	var request AuthStatusRequest
	if json.Unmarshal([]byte(`5`), &request) == nil {
		t.Error("request 5 decoded")
	}
}
