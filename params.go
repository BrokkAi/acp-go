package acp

import (
	"bytes"
	"encoding/json"
)

// checkWireParams refuses outgoing params that JSON-RPC does not allow, as
// the reference SDK does before writing a request or notification: params
// must be an object or an array, or null for none. Anything else is -32602
// and nothing is sent.
func checkWireParams(params json.RawMessage) error {
	trimmed := bytes.TrimSpace(params)
	if len(trimmed) == 0 || string(trimmed) == "null" || trimmed[0] == '{' || trimmed[0] == '[' {
		return nil
	}
	data, _ := json.Marshal("JSON-RPC params must be an object or array")
	return &RPCError{Code: -32602, Message: "Invalid params", Data: data}
}

// wireParams is the params member of an outgoing frame: null params are
// omitted, as the reference SDK serializes a message without params.
func wireParams(params json.RawMessage) json.RawMessage {
	if string(bytes.TrimSpace(params)) == "null" {
		return nil
	}
	return params
}
