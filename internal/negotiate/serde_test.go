package negotiate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go"
	schema1 "github.com/BrokkAi/acp-go/schema"
)

// The reasons are serde's, as the reference router reports them; the router
// expectations below were confirmed against the reference SDK 5.0.0.
func TestValidateV2MatchesTheReferenceRouter(t *testing.T) {
	for params, want := range map[string]string{
		`{"protocolVersion":2}`:                                    "invalid initialize params: missing field `info`",
		`{"protocolVersion":2,"info":{"name":"c"}}`:                "invalid initialize params: missing field `version`",
		`{"protocolVersion":2,"info":{}}`:                          "invalid initialize params: missing field `name`",
		`{"protocolVersion":2,"info":"bad"}`:                       `invalid initialize params: invalid type: string "bad", expected struct Implementation`,
		`{"protocolVersion":2,"info":null}`:                        "invalid initialize params: invalid type: null, expected struct Implementation",
		`{"protocolVersion":2,"info":[]}`:                          "invalid initialize params: invalid length 0, expected struct Implementation with 4 elements",
		`{"protocolVersion":2,"info":{"name":5,"version":"1"}}`:    "invalid initialize params: invalid type: integer `5`, expected a string",
		`{"protocolVersion":2,"info":{"name":"c","version":1.5}}`:  "invalid initialize params: invalid type: floating point `1.5`, expected a string",
		`{"protocolVersion":2,"info":{"name":"c","version":{}}}`:   "invalid initialize params: invalid type: map, expected a string",
		`{"protocolVersion":2,"info":{"name":true,"version":"1"}}`: "invalid initialize params: invalid type: boolean `true`, expected a string",
	} {
		err := ValidateV2(json.RawMessage(params))
		var rpc *acp.RPCError
		data, _ := json.Marshal(want)
		if !errors.As(err, &rpc) || rpc.Code != -32602 || string(rpc.Data) != string(data) {
			t.Fatalf("%s: error = %v, want %s", params, err, data)
		}
	}
	for _, params := range []string{
		`{"protocolVersion":2,"info":{"name":"c","version":"1"},"_meta":"x"}`,
		`{"protocolVersion":2,"info":{"name":"c","version":"1","title":5,"_meta":"x"},"capabilities":{"elicitation":"bad"}}`,
		`{"protocolVersion":2,"info":{"name":"c","version":"1"},"capabilities":"bad"}`,
	} {
		if err := ValidateV2(json.RawMessage(params)); err != nil {
			t.Fatalf("%s: %v", params, err)
		}
		normalized, err := V2Params(json.RawMessage(params))
		if err != nil {
			t.Fatalf("%s: V2Params: %v", params, err)
		}
		if string(normalized) != `{"info":{"name":"c","version":"1"},"protocolVersion":2}` &&
			string(normalized) != `{"capabilities":{},"info":{"name":"c","version":"1"},"protocolVersion":2}` {
			t.Fatalf("%s: normalized = %s", params, normalized)
		}
	}
}

func TestSelectAnswersANullV2RequestID(t *testing.T) {
	route := Select([]byte(`{"jsonrpc":"2.0","id":null,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"c","version":"1"}}}`), "initialize", Supported{V1: true, V2: true})
	want := `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"ACP protocol v2 request ids must be a string or an integer, not null"}}`
	if string(route.Answer) != want {
		t.Fatalf("answer = %s", route.Answer)
	}
}

// serde matches member names exactly; encoding/json does not. A case variant
// of a known key is an unknown key to the reference SDK and must neither be
// read nor reported with Go's error text.
func TestInitializeKeysMatchExactly(t *testing.T) {
	for _, test := range []struct {
		name   string
		params string
		v1     bool
		want   string
	}{
		{"exact v2 Info", `{"protocolVersion":2,"info":{"name":"c","version":"1"},"Info":5}`, false, `{"info":{"name":"c","version":"1"},"protocolVersion":2}`},
		{"exact v2 nested", `{"protocolVersion":2,"info":{"name":"c","version":"1","Title":"t"},"capabilities":{"Auth":{"terminal":{}}}}`, false, `{"capabilities":{},"info":{"name":"c","version":"1"},"protocolVersion":2}`},
		// Rust's DefaultOnError applies at every level: a bad nested member
		// leaves the rest of its capability in place (confirmed with the
		// reference probe).
		{"exact v2 nested leniency", `{"protocolVersion":2,"info":{"name":"c","version":"1"},"capabilities":{"auth":{"terminal":"bad","_meta":{"k":1}},"elicitation":{"form":"bad","url":{}}}}`, false, `{"capabilities":{"auth":{"_meta":{"k":1}},"elicitation":{"url":{}}},"info":{"name":"c","version":"1"},"protocolVersion":2}`},
		{"newer v2 nested leniency", `{"protocolVersion":9,"info":{"name":"c","version":"1"},"capabilities":{"auth":{"terminal":{"_meta":"bad"}},"elicitation":{"form":{"_meta":3}}}}`, false, `{"capabilities":{"auth":{"terminal":{}},"elicitation":{"form":{}}},"info":{"name":"c","version":"1"},"protocolVersion":2}`},
		{"newer v2 Capabilities", `{"protocolVersion":3,"info":{"name":"c","version":"1"},"Capabilities":{"auth":{"terminal":{}}},"PROTOCOLVERSION":"x"}`, false, `{"info":{"name":"c","version":"1"},"protocolVersion":2}`},
		{"exact v1 ClientCapabilities", `{"protocolVersion":1,"ClientCapabilities":{"fs":{"readTextFile":true}}}`, true, `{"protocolVersion":1}`},
		{"exact v1 nested", `{"protocolVersion":1,"clientCapabilities":{"FS":{"readTextFile":true},"fs":{"ReadTextFile":true}}}`, true, `{"clientCapabilities":{"fs":{}},"protocolVersion":1}`},
		{"newer v1 Capabilities", `{"protocolVersion":3,"Capabilities":{"auth":{"terminal":{}}},"Info":{"name":"c","version":"1"}}`, true, `{"clientCapabilities":{"session":{"configOptions":{"boolean":{}}}},"protocolVersion":1}`},
	} {
		negotiate := V2Params
		if test.v1 {
			negotiate = V1Params
		}
		got, err := negotiate(json.RawMessage(test.params))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if test.v1 {
			// The runtime decodes the result with the generated decoder, which
			// matches names exactly; compare what it reads.
			var request schema1.InitializeRequest
			if err := json.Unmarshal(got, &request); err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			got = mustMarshal(request)
		}
		if string(got) != test.want {
			t.Errorf("%s: got %s, want %s", test.name, got, test.want)
		}
	}
	if err := ValidateV2(json.RawMessage(`{"protocolVersion":2,"Info":{"name":"c","version":"1"}}`)); err == nil ||
		!strings.Contains(string(err.(*acp.RPCError).Data), "missing field `info`") {
		t.Fatalf("Info is not info: %v", err)
	}
}
