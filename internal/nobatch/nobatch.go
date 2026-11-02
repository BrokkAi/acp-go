// Package nobatch recognizes JSON-RPC batch arrays and holds the one error
// that answers them.
//
// acp-go does not support JSON-RPC batching on any protocol version, matching
// the ACP v2 transports rule and the reference Rust SDK 5.0.0. A JSON array,
// including an empty one, is one invalid request: it gets a single -32600
// error with "id": null, and none of its entries run.
package nobatch

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

const (
	// Code is the JSON-RPC Invalid Request error code.
	Code = -32600
	// Message is the reference SDK's message for Code.
	Message = "Invalid request"
	// Reason is the reference SDK's error data for a rejected batch.
	Reason = "JSON-RPC batches are not supported; send each message individually"
)

// rejection is the exact wire response, without a trailing newline, that the
// reference SDK sends for a batch array.
const rejection = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"JSON-RPC batches are not supported; send each message individually"}}`

// Rejection returns a fresh copy of the JSON-RPC error response that answers a
// batch array, without a trailing newline.
func Rejection() []byte { return []byte(rejection) }

// IsArray reports whether frame, ignoring surrounding whitespace, is one
// well-formed JSON array. Input that starts with '[' but is not valid JSON is
// malformed JSON, not a batch, and is left to the caller's malformed-frame
// handling.
func IsArray(frame []byte) bool {
	trimmed := bytes.TrimSpace(frame)
	return len(trimmed) > 0 && trimmed[0] == '[' && utf8.Valid(trimmed) && json.Valid(trimmed)
}
