package acp

import (
	"bytes"
	"encoding/json"

	"github.com/BrokkAi/acp-go/internal/serdejson"
	"github.com/BrokkAi/acp-go/schema"
)

// proxySuccessorMethod is the proxy-chains RFD's successor envelope method.
const proxySuccessorMethod = "proxy/successor"

// cancellation is how the reference SDK reads a notification before
// dispatching it.
type cancellation int

const (
	// notCancellation is any other notification, dispatched as usual.
	notCancellation cancellation = iota
	// cancelsRequest cancels the inbound request with the returned id, and
	// the notification is still dispatched.
	cancelsRequest
	// malformedCancellation is a $/cancel_request whose params do not parse.
	// It cancels nothing and is not dispatched.
	malformedCancellation
)

// peelSuccessor peels any proxy/successor envelopes off a notification, as
// the reference SDK's peel_successor_envelopes does, returning the innermost
// method and params. It stops at an envelope that is not an object or whose
// method is not a string, leaving that envelope to the handler.
func peelSuccessor(method string, params json.RawMessage) (string, json.RawMessage) {
	for method == proxySuccessorMethod {
		var envelope map[string]json.RawMessage
		if json.Unmarshal(params, &envelope) != nil || envelope == nil {
			break
		}
		raw := bytes.TrimSpace(envelope["method"])
		var inner string
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &inner) != nil {
			break
		}
		method, params = inner, envelope["params"]
	}
	return method, params
}

// IsCancelRequestNotification reports whether a notification is a
// $/cancel_request, plain or wrapped in any number of proxy/successor
// envelopes, as the reference SDK's is_cancel_request_notification does.
// A $/cancel_request is connection-scoped: its requestId means nothing on any
// other connection, so code that forwards notifications to another
// connection drops one instead of relaying it. The connection has already
// cancelled the request it names.
func IsCancelRequestNotification(method string, params json.RawMessage) bool {
	method, _ = peelSuccessor(method, params)
	return method == schema.CancelRequestMethodName
}

// cancelRequestID reports the request a notification cancels, as the
// reference SDK reads it: a $/cancel_request, plain or wrapped in any number
// of proxy/successor envelopes, cancels its requestId. An envelope whose
// method is not a string is left to the notification handler and cancels
// nothing.
func cancelRequestID(method string, params json.RawMessage) (json.RawMessage, cancellation) {
	method, params = peelSuccessor(method, params)
	if method != schema.CancelRequestMethodName {
		return nil, notCancellation
	}
	if len(params) == 0 {
		params = json.RawMessage("null")
	}
	var typed schema.CancelRequestNotification
	if json.Unmarshal(params, &typed) != nil {
		return nil, malformedCancellation
	}
	// The typed decoder accepted the params, as the object form or serde's
	// positional form ([requestId, _meta]); take the id from the same place.
	var id json.RawMessage
	if trimmed := bytes.TrimSpace(params); trimmed[0] == '[' {
		var items []json.RawMessage
		if json.Unmarshal(trimmed, &items) != nil || len(items) == 0 {
			return nil, malformedCancellation
		}
		id = items[0]
	} else {
		var request struct {
			ID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(trimmed, &request) != nil {
			return nil, malformedCancellation
		}
		id = request.ID
	}
	return canonicalID(id), cancelsRequest
}

// canonicalID is a JSON-RPC id as the reference SDK compares and writes it:
// decoded and re-serialized, so "\u0062" and "b" are the same id. An id that
// does not parse is kept as it is.
func canonicalID(id json.RawMessage) json.RawMessage {
	value, err := serdejson.ParseAnyDepth(id)
	if err != nil {
		return id
	}
	return serdejson.Encode(value)
}
