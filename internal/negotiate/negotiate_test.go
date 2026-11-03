package negotiate

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/BrokkAi/acp-go"
)

func TestRequestedVersionAcceptsOnlyUint16Integers(t *testing.T) {
	for params, want := range map[string]uint16{
		`{"protocolVersion":0}`:     0,
		`{"protocolVersion":2}`:     2,
		`{"protocolVersion":65535}`: 65535,
	} {
		got, err := RequestedVersion(json.RawMessage(params))
		if err != nil || got != want {
			t.Fatalf("%s: got %d, %v", params, got, err)
		}
	}
	for _, params := range []string{
		`{}`, `[]`, `null`, `"x"`, `{"protocolVersion":null}`, `{"protocolVersion":"2"}`,
		`{"protocolVersion":-0}`, `{"protocolVersion":-1}`, `{"protocolVersion":2.0}`,
		`{"protocolVersion":2e0}`, `{"protocolVersion":65536}`, `{"protocolVersion":true}`,
	} {
		_, err := RequestedVersion(json.RawMessage(params))
		var rpc *acp.RPCError
		if !errors.As(err, &rpc) || rpc.Code != -32602 || string(rpc.Data) != `"initialize.protocolVersion must be a valid ACP protocol version"` {
			t.Fatalf("%s: error = %v", params, err)
		}
	}
}

func TestSelectValidatesMessageShape(t *testing.T) {
	supported := Supported{V2: true}
	invalid := `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request"}}`
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1.5,"method":"initialize","params":{"protocolVersion":2}}`,
		`{"jsonrpc":"2.0","id":-0,"method":"initialize","params":{"protocolVersion":2}}`,
		`{"jsonrpc":"2.0","id":{},"method":"initialize","params":{"protocolVersion":2}}`,
		`{"jsonrpc":"2.0","id":1,"method":7}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":"x"}`,
		`{"id":1,"method":"initialize","params":{"protocolVersion":2}}`,
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","result":{}}`,
		`null`,
	} {
		route := Select([]byte(line), "initialize", supported)
		if string(route.Answer) != invalid {
			t.Fatalf("%s: answer = %s", line, route.Answer)
		}
	}
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1,"result":null}`,
		`{"jsonrpc":"2.0","id":1,"result":{},"error":{}}`,
		`{"error":{}}`,
	} {
		if route := Select([]byte(line), "initialize", supported); route.Answer != nil || route.Version != 0 {
			t.Fatalf("%s: route = %+v", line, route)
		}
	}
}
