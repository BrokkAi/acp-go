// Package castparams decodes JSON-RPC params the way the reference Rust SDK
// 5.0.0 hands them to a typed handler, and builds the data of the -32602 it
// answers when they do not deserialize.
//
// The reference SDK's JSON-RPC layer reads absent params as null for every
// message (params_from_transport), so a message without params decodes, and
// fails, exactly as one with "params": null. Its typed handlers then
// deserialize the params with json_cast_params, which answers a failure with
// -32602 "Invalid params" and the data
// {"error": <serde reason>, "json": <params>, "phase": "deserialization"}.
//
// The package imports nothing that imports the root acp package, so that
// the root package can use it too; internal/rpcparams wraps it in the
// acp.RPCError the dispatch layers answer.
package castparams

import (
	"bytes"
	"encoding/json"

	"github.com/BrokkAi/acp-go/internal/jsonline"
	"github.com/BrokkAi/acp-go/internal/serdejson"
)

// Code and Message are the reference SDK's invalid_params error.
const (
	Code    = -32602
	Message = "Invalid params"
)

var null = json.RawMessage("null")

// Params returns raw, or null when the params are absent.
func Params(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return null
	}
	return raw
}

// Unmarshal decodes raw, the params of a message, into v, reading absent
// params as null.
func Unmarshal(raw json.RawMessage, v any) error {
	return json.Unmarshal(Params(raw), v)
}

// Data is the data json_cast_params sends when raw, the params of a message,
// do not deserialize: err's text, the params as serde_json holds them (null
// when absent, compact, with a repeated member keeping its first position
// and last value, and numbers as serde_json writes them), and the phase.
func Data(err error, raw json.RawMessage) json.RawMessage {
	params := Params(raw)
	if value, parseErr := serdejson.Parse(params); parseErr == nil {
		return DataValue(err.Error(), value)
	}
	// Params the transport accepted always parse; keep anything else as
	// received rather than lose it.
	return data(err.Error(), params)
}

// DataValue is Data for params already held as a serde_json value.
func DataValue(detail string, params *serdejson.Value) json.RawMessage {
	return data(detail, serdejson.Encode(params))
}

func data(detail string, params []byte) json.RawMessage {
	var b bytes.Buffer
	b.WriteString(`{"error":`)
	b.Write(jsonline.Quote([]byte(detail)))
	b.WriteString(`,"json":`)
	b.Write(params)
	b.WriteString(`,"phase":"deserialization"}`)
	return b.Bytes()
}
