package acphttp

import (
	"bytes"
	"strconv"

	"github.com/BrokkAi/acp-go/internal/serdejson"
)

// messageKind is the variant of the Rust SDK's RawJsonRpcMessage.
type messageKind int

const (
	kindRequest messageKind = iota + 1
	kindNotification
	kindResponse
)

// decodeMessage reads data as one JSON-RPC message exactly as the Rust SDK's
// TransportFrame::parse_json does, and returns the message as
// RawJsonRpcMessage holds it, ready for serde_json::to_string: members in the
// order jsonrpc, id, method, params for a request or notification and
// jsonrpc, id, result or error for a response; other envelope members
// dropped; a null params dropped; and an error object with code, message,
// data, then its other members. It reports false for anything the Rust SDK
// keeps as a malformed frame: JSON serde_json rejects, a value that is not an
// object, a "jsonrpc" other than "2.0", a method that is not a string, params
// that are not null, an object, or an array, an id that is not null, a
// string, or an integer that fits int64, a request or notification that also
// carries result or error, and a response without exactly one of result and
// an error object whose code fits int32 and whose message is a string.
func decodeMessage(data []byte) (*serdejson.Value, messageKind, bool) {
	value, err := serdejson.Parse(data)
	if err != nil || value.Kind != serdejson.Object {
		return nil, 0, false
	}
	version, _ := value.Get("jsonrpc")
	id, hasID := value.Get("id")
	method, hasMethod := value.Get("method")
	result, hasResult := value.Get("result")
	rawError, hasError := value.Get("error")
	if !validVersion(version) || (hasID && !validRequestID(id)) {
		return nil, 0, false
	}
	message := &serdejson.Value{Kind: serdejson.Object}
	message.Set("jsonrpc", stringValue("2.0"))
	if hasID {
		message.Set("id", id)
	}
	switch {
	case hasMethod && !hasResult && !hasError:
		if method.Kind != serdejson.String {
			return nil, 0, false
		}
		message.Set("method", method)
		if params, ok := value.Get("params"); ok {
			switch params.Kind {
			case serdejson.Null:
			case serdejson.Object, serdejson.Array:
				message.Set("params", params)
			default:
				return nil, 0, false
			}
		}
		if hasID {
			return message, kindRequest, true
		}
		return message, kindNotification, true
	case !hasMethod && hasID && hasResult != hasError:
		if hasResult {
			message.Set("result", result)
			return message, kindResponse, true
		}
		canonical, ok := decodeError(rawError)
		if !ok {
			return nil, 0, false
		}
		message.Set("error", canonical)
		return message, kindResponse, true
	default:
		return nil, 0, false
	}
}

// validVersion reports whether a "jsonrpc" member deserializes as the Rust
// SDK's JsonRpcVersion, a serde enum with the one unit variant "2.0": the
// string "2.0", or the externally tagged form {"2.0": null}.
func validVersion(version *serdejson.Value) bool {
	switch {
	case version == nil:
		return false
	case version.Kind == serdejson.String:
		return version.String == "2.0"
	case version.Kind == serdejson.Object:
		return len(version.Members) == 1 && version.Members[0].Key == "2.0" &&
			version.Members[0].Value.Kind == serdejson.Null
	default:
		return false
	}
}

// validRequestID reports whether id is a RawJsonRpcId: null, a string, or an
// integer that fits int64. A number with a fraction or an exponent, and -0,
// are floats there, so they are not ids.
func validRequestID(id *serdejson.Value) bool {
	switch id.Kind {
	case serdejson.Null, serdejson.String:
		return true
	case serdejson.Integer:
		_, err := strconv.ParseInt(id.Integer, 10, 64)
		return err == nil
	default:
		return false
	}
}

// decodeError reads an error object as the Rust SDK's RawJsonRpcError and
// returns it as that struct serializes: code, message, data when present
// (null included), then every other member in order.
func decodeError(value *serdejson.Value) (*serdejson.Value, bool) {
	if value.Kind != serdejson.Object {
		return nil, false
	}
	code, ok := value.Get("code")
	if !ok || code.Kind != serdejson.Integer {
		return nil, false
	}
	if _, err := strconv.ParseInt(code.Integer, 10, 32); err != nil {
		return nil, false
	}
	message, ok := value.Get("message")
	if !ok || message.Kind != serdejson.String {
		return nil, false
	}
	canonical := &serdejson.Value{Kind: serdejson.Object}
	canonical.Set("code", code)
	canonical.Set("message", message)
	if data, ok := value.Get("data"); ok {
		canonical.Set("data", data)
	}
	for _, member := range value.Members {
		switch member.Key {
		case "code", "message", "data":
		default:
			canonical.Set(member.Key, member.Value)
		}
	}
	return canonical, true
}

func stringValue(s string) *serdejson.Value {
	return &serdejson.Value{Kind: serdejson.String, String: s}
}

// lineValue makes a frame that is not a JSON-RPC message one line, as the
// Rust SDK's line writer does with a malformed frame (malformed_line_value):
// a frame without line breaks is kept as is, JSON that serde_json reads is
// re-encoded as serde_json::to_string writes it, and anything else becomes a
// JSON string of the frame.
func lineValue(raw []byte) []byte {
	if !bytes.ContainsAny(raw, "\r\n") {
		return raw
	}
	if value, err := serdejson.Parse(raw); err == nil {
		return serdejson.Encode(value)
	}
	return serdejson.Encode(stringValue(string(raw)))
}
