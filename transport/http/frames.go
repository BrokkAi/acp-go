package acphttp

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/BrokkAi/acp-go/internal/serdejson"
)

const (
	// HeaderConnectionID names the HTTP header carrying the ACP connection id.
	HeaderConnectionID = "Acp-Connection-Id"
	// HeaderSessionID names the HTTP header carrying a session id for
	// session-scoped methods and session-scoped event streams.
	HeaderSessionID = "Acp-Session-Id"

	// DefaultPath is the endpoint the client targets and the server serves
	// when no path is configured.
	DefaultPath = "/acp"

	eventStreamMIME = "text/event-stream"
	jsonMIME        = "application/json"
)

// frame is one JSON-RPC message as the Rust SDK's RawJsonRpcMessage holds
// it, with the members the transport routes on. ID, Params, Result, and Error
// are the serde_json encodings of those members, nil when absent; message is
// the whole message, which encode writes as the Rust SDK sends it.
type frame struct {
	kind   messageKind
	ID     json.RawMessage
	Method string
	Params json.RawMessage
	Result json.RawMessage
	Error  json.RawMessage

	message *serdejson.Value
}

// errBatch is the Rust SDK's error data for a JSON-RPC batch array, which it
// does not support.
var errBatch = errors.New("JSON-RPC batches are not supported; send each message individually")

// parseMessage reads data as one JSON-RPC message the way the Rust SDK's
// RawJsonRpcMessage deserializer does (see decodeMessage). It reports false
// for anything the Rust SDK keeps as a malformed frame instead.
func parseMessage(data []byte) (frame, bool) {
	message, kind, ok := decodeMessage(data)
	if !ok {
		return frame{}, false
	}
	return newFrame(message, kind), true
}

func newFrame(message *serdejson.Value, kind messageKind) frame {
	f := frame{kind: kind, message: message}
	if id, ok := message.Get("id"); ok {
		f.ID = serdejson.Encode(id)
	}
	if method, ok := message.Get("method"); ok {
		f.Method = method.String
	}
	if params, ok := message.Get("params"); ok {
		f.Params = serdejson.Encode(params)
	}
	if result, ok := message.Get("result"); ok {
		f.Result = serdejson.Encode(result)
	}
	if rpcError, ok := message.Get("error"); ok {
		f.Error = serdejson.Encode(rpcError)
	}
	return f
}

// encode writes the message as serde_json::to_string writes the Rust SDK's
// RawJsonRpcMessage.
func (f frame) encode() []byte { return serdejson.Encode(f.message) }

func (f frame) isRequest() bool { return f.kind == kindRequest }

func (f frame) isInitialize() bool { return f.isRequest() && f.Method == "initialize" }

func (f frame) isResponse() bool { return f.kind == kindResponse }

// hasMethod reports a request or notification, whose method may be empty.
func (f frame) hasMethod() bool { return f.kind == kindRequest || f.kind == kindNotification }

// sessionID mirrors the Rust crate's session_id_from_message: the string
// params.sessionId of a request or notification, unless the message is
// connection-scoped. Responses carry no session.
func (f frame) sessionID() (string, bool) {
	if !f.hasMethod() || f.connectionScoped() {
		return "", false
	}
	return stringMember(f.Params, "sessionId")
}

// stringMember reads a string member of a JSON object.
func stringMember(object json.RawMessage, name string) (string, bool) {
	var members map[string]json.RawMessage
	if len(object) == 0 || json.Unmarshal(object, &members) != nil || members == nil {
		return "", false
	}
	raw, ok := members[name]
	if !ok || !strings.HasPrefix(strings.TrimSpace(string(raw)), `"`) {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

// proxySuccessorMethod is the envelope a proxy chain wraps a message in.
const proxySuccessorMethod = "proxy/successor"

// connectionScoped mirrors the Rust crate's
// is_connection_scoped_protocol_message: "$/" protocol methods, and a
// $/cancel_request notification even when it is wrapped in proxy/successor
// envelopes. Such a message never takes a session header and always travels
// on the connection stream.
func (f frame) connectionScoped() bool {
	if strings.HasPrefix(f.Method, "$/") {
		return true
	}
	if f.Method != proxySuccessorMethod || f.kind != kindNotification {
		return false
	}
	method, params := f.Method, f.Params
	for method == proxySuccessorMethod {
		var envelope struct {
			Method *string         `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(params, &envelope) != nil || envelope.Method == nil {
			return false
		}
		method, params = *envelope.Method, envelope.Params
	}
	return method == "$/cancel_request"
}

// idKey identifies a JSON-RPC id for response routing. A null or missing id
// has no key.
func idKey(id json.RawMessage) (string, bool) {
	var value any
	if len(id) == 0 || json.Unmarshal(id, &value) != nil {
		return "", false
	}
	switch value := value.(type) {
	case string:
		return "s:" + value, true
	case float64:
		return "n:" + strings.TrimSpace(string(id)), true
	default:
		return "", false
	}
}

// responseKey is idKey that also matches a null id, for pairing an initialize
// request with its response.
func responseKey(id json.RawMessage) (string, bool) {
	if strings.TrimSpace(string(id)) == "null" {
		return "null", true
	}
	return idKey(id)
}

// applySessionHeader mirrors the Rust crate's apply_session_header_to_message:
// the Acp-Session-Id header becomes params.sessionId of a request or
// notification that is not connection-scoped, appended after the other params
// as serde_json's Map::insert does, and params are created when absent. A
// conflicting or non-string params.sessionId, or array params, is refused
// with the Rust crate's message.
func applySessionHeader(f frame, sessionID string) (frame, error) {
	if !f.hasMethod() || f.connectionScoped() {
		return f, nil
	}
	params, ok := f.message.Get("params")
	switch {
	case !ok:
		params = &serdejson.Value{Kind: serdejson.Object}
		f.message.Set("params", params)
	case params.Kind != serdejson.Object:
		return f, errors.New("Acp-Session-Id header requires object params")
	}
	if existing, ok := params.Get("sessionId"); ok {
		switch {
		case existing.Kind != serdejson.String:
			return f, errors.New("params.sessionId must be a string")
		case existing.String != sessionID:
			return f, errors.New("Acp-Session-Id header does not match params.sessionId")
		}
		return f, nil
	}
	params.Set("sessionId", stringValue(sessionID))
	f.Params = serdejson.Encode(params)
	return f, nil
}

// requiresSessionHeader mirrors the Rust crate's list of methods that must
// carry the session header or params.sessionId.
func requiresSessionHeader(method string) bool {
	switch method {
	case "session/prompt", "session/cancel", "session/close", "session/delete",
		"session/fork", "session/load", "session/resume",
		"session/set_config_option", "session/set_mode", "session/set_model":
		return true
	default:
		return false
	}
}
