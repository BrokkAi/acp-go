package negotiate

import (
	"encoding/json"
	"testing"
)

func TestSuccessorEnclosesInitializeMatchesTheReferenceSDK(t *testing.T) {
	for _, test := range []struct {
		method string
		params string
		want   bool
	}{
		{"proxy/successor", `{"method":"initialize","params":{}}`, true},
		{"proxy/successor", `{"method":"proxy/initialize"}`, true},
		{"proxy/successor", `{"method":"proxy/successor","params":{"method":"initialize"}}`, true},
		{"proxy/successor", `{"method":"proxy/successor"}`, false},
		{"proxy/successor", `{"method":"session/new"}`, false},
		{"proxy/successor", `{"method":5}`, false},
		{"proxy/successor", `[]`, false},
		{"initialize", `{}`, false},
		{"_proxy/successor", `{"method":"initialize"}`, false},
	} {
		if got := SuccessorEnclosesInitialize(test.method, json.RawMessage(test.params)); got != test.want {
			t.Errorf("%s %s: %t, want %t", test.method, test.params, got, test.want)
		}
	}
	if got := string(InvalidPrewrappedInitialize().Data); got != `"initialize requests must be sent as a logical `+"`initialize`"+` message; `+"`proxy/successor`"+` wrapping is applied by connection routing"` {
		t.Fatalf("data = %s", got)
	}
}

func TestOutgoingV1ParamsSetsTheVersionInPlace(t *testing.T) {
	got, err := OutgoingV1Params(json.RawMessage(`{"clientInfo":{"name":"<"},"protocolVersion":7}`))
	if err != nil || string(got) != `{"clientInfo":{"name":"<"},"protocolVersion":1}` {
		t.Fatalf("got %s, %v", got, err)
	}
	if _, err := OutgoingV1Params(json.RawMessage(`[]`)); err == nil {
		t.Fatal("non-object params accepted")
	}
}
