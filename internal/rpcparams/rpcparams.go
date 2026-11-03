// Package rpcparams is the request dispatch every acp-go dispatch layer that
// imports the root acp package shares: the agent runtimes, the client hosts,
// and the unstable dispatch. It reads params as the reference Rust SDK 5.0.0
// does, answers a failure with the reference SDK's json_cast_params -32602,
// built by internal/castparams, and answers a request no handler serves with
// the reference SDK's -32601.
package rpcparams

import (
	"encoding/json"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/castparams"
	"github.com/BrokkAi/acp-go/internal/serdejson"
)

// Decode decodes raw, the params of a request, as a T. Absent params decode
// as null. Params that do not decode answer InvalidParams.
func Decode[T any](raw json.RawMessage) (T, error) {
	var request T
	if err := castparams.Unmarshal(raw, &request); err != nil {
		return request, InvalidParams(err, raw)
	}
	return request, nil
}

// InvalidParams is the reference SDK's -32602 for params that do not
// deserialize (json_cast_params): data carries err's text, the received
// params as serde_json holds them (null when absent), and the phase.
func InvalidParams(err error, raw json.RawMessage) *acp.RPCError {
	return &acp.RPCError{Code: castparams.Code, Message: castparams.Message, Data: castparams.Data(err, raw)}
}

// InvalidParamsValue is InvalidParams for params already held as a
// serde_json value, failing with detail.
func InvalidParamsValue(detail string, params *serdejson.Value) *acp.RPCError {
	return &acp.RPCError{Code: castparams.Code, Message: castparams.Message, Data: castparams.DataValue(detail, params)}
}

// MethodNotFound is the reference SDK's answer to a request no handler
// serves (reject_unhandled): -32601 "Method not found", with the method name
// as string data. It answers every role and protocol version alike,
// extension methods included; an unhandled notification is ignored instead.
func MethodNotFound(method string) *acp.RPCError {
	data, _ := json.Marshal(method)
	return &acp.RPCError{Code: -32601, Message: "Method not found", Data: data}
}
