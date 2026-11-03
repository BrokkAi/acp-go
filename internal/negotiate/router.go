package negotiate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/jsonline"
	schema1 "github.com/BrokkAi/acp-go/schema"
)

// Supported lists the protocol implementations a router has configured.
type Supported struct {
	V1, V2 bool
}

func (s Supported) versions() []uint16 {
	var versions []uint16
	if s.V1 {
		versions = append(versions, V1)
	}
	if s.V2 {
		versions = append(versions, V2)
	}
	return versions
}

// selectVersion is the highest configured version at or below requested. Any
// request that does not select v2 keeps v1's fallback-to-latest rule, so a v1
// implementation behaves as it does without the router.
func (s Supported) selectVersion(requested uint16) (uint16, bool) {
	if s.V2 && requested >= V2 {
		return V2, true
	}
	if s.V1 {
		return V1, true
	}
	return 0, false
}

// Route is what a protocol router does with one frame read before a version
// is selected. At most one of Answer and Version is set; when neither is, the
// frame is ignored.
type Route struct {
	// Answer is a complete JSON-RPC response line, without its terminator,
	// to write before reading the next frame.
	Answer []byte
	// Version is the selected implementation's protocol version, 1 or 2.
	Version uint16
	// Frame is the initial frame to hand to the selected implementation,
	// without its terminator: the original line when it needs no rewrite,
	// otherwise the frame with params normalized for Version.
	Frame []byte
}

// Select examines one frame, a valid UTF-8 line without its terminator, read
// before a version is selected. method is the initialize method: "initialize"
// for an agent, the proxy initialize method for a proxy. It follows the
// reference SDK's select_protocol_peer:
//
//   - a line that is not JSON gets a -32700 error and a JSON array a -32600
//     error, both with id null;
//   - a response, or anything shaped like one, is ignored;
//   - any other value that is not a JSON-RPC message gets a -32600 error with
//     id null;
//   - an initialize request selects the highest configured version at or
//     below the one it requests, and its params are normalized for that
//     version unless it requests exactly that version;
//   - when v1 is configured, every other message that does not select v2,
//     including a malformed initialize, a request for another method, and a
//     notification, is handed to the v1 implementation unchanged;
//   - otherwise a request is answered with its error, a -32022 error when no
//     configured version is at or below the requested one, and a
//     notification is ignored.
//
// Every answer leaves the connection uninitialized, so the peer may send
// initialize again.
func Select(line []byte, method string, supported Supported) Route {
	switch jsonline.Classify(line) {
	case jsonline.Invalid:
		return Route{Answer: jsonline.ParseError(line)}
	case jsonline.Batch:
		return Route{Answer: jsonline.BatchRejection()}
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(line, &object) != nil {
		// A JSON value that is not an object is not a message.
		return Route{Answer: jsonline.InvalidRequest()}
	}
	message, ok := parseMessage(object)
	if !ok {
		if responseOnlyShape(object) {
			return Route{}
		}
		return Route{Answer: jsonline.InvalidRequest()}
	}
	if message.response {
		return Route{}
	}
	version, params, err := selectInitialize(message, method, supported)
	if err != nil {
		if supported.V1 && !requestsV2(message, method, supported) {
			return Route{Version: V1, Frame: line}
		}
		if message.notification {
			return Route{}
		}
		if err == errNullV2RequestID {
			return Route{Answer: jsonline.NullV2RequestID()}
		}
		return Route{Answer: response(message.id, err)}
	}
	if params == nil {
		return Route{Version: version, Frame: line}
	}
	frame := map[string]json.RawMessage{
		"jsonrpc": json.RawMessage(`"2.0"`),
		"id":      message.id,
		"method":  mustMarshal(message.method),
		"params":  params,
	}
	return Route{Version: version, Frame: mustMarshal(frame)}
}

// errNullV2RequestID marks a v2-selecting request with a null id, which is
// answered with jsonline.NullV2RequestID.
var errNullV2RequestID = InvalidRequest("ACP protocol v2 request ids must be a string or an integer, not null")

type message struct {
	id           json.RawMessage
	method       string
	params       json.RawMessage
	notification bool
	response     bool
}

// parseMessage reads object as the reference SDK reads one JSON-RPC message:
// "jsonrpc" must be "2.0"; a request has a string method and an id that is a
// string, an integer, or null; a notification has a method and no id; params,
// when present and not null, is an object or an array; and a response has an
// id and exactly one of result and error, and no method.
func parseMessage(object map[string]json.RawMessage) (message, bool) {
	if string(bytes.TrimSpace(object["jsonrpc"])) != `"2.0"` {
		return message{}, false
	}
	rawMethod, hasMethod := object["method"]
	id, hasID := object["id"]
	_, hasResult := object["result"]
	_, hasError := object["error"]
	switch {
	case hasMethod && !hasResult && !hasError:
		var result message
		if !isString(rawMethod) || json.Unmarshal(rawMethod, &result.method) != nil {
			return message{}, false
		}
		params := bytes.TrimSpace(object["params"])
		if len(params) > 0 && string(params) != "null" && params[0] != '{' && params[0] != '[' {
			return message{}, false
		}
		result.params = params
		if !hasID {
			result.notification = true
			return result, true
		}
		if !validID(id) {
			return message{}, false
		}
		result.id = bytes.TrimSpace(id)
		return result, true
	case !hasMethod && hasID && hasResult != hasError:
		return message{response: true}, true
	default:
		return message{}, false
	}
}

// validID accepts the reference SDK's raw JSON-RPC ids: null, a string, or an
// integer that fits int64.
func validID(raw json.RawMessage) bool {
	text := string(bytes.TrimSpace(raw))
	switch {
	case text == "null":
		return true
	case isString(raw):
		return true
	case text == "-0":
		return false
	}
	_, err := strconv.ParseInt(text, 10, 64)
	return err == nil
}

func responseOnlyShape(object map[string]json.RawMessage) bool {
	_, hasMethod := object["method"]
	_, hasResult := object["result"]
	_, hasError := object["error"]
	return !hasMethod && (hasResult || hasError)
}

// selectInitialize selects the configured version for an initialize request
// and returns its params rewritten for that version, or nil params when the
// request is for exactly that version and is handed over unchanged.
func selectInitialize(message message, method string, supported Supported) (uint16, json.RawMessage, *acp.RPCError) {
	if message.notification {
		return 0, nil, InvalidRequest(fmt.Sprintf("first ACP message must be a `%s` request", method))
	}
	if message.method != method {
		return 0, nil, InvalidRequest(fmt.Sprintf("first ACP request must be `%s`", method))
	}
	object, ok := decodeObject(message.params)
	if !ok {
		return 0, nil, InvalidProtocolVersion()
	}
	requested, err := requestedVersion(object)
	if err != nil {
		return 0, nil, err.(*acp.RPCError)
	}
	selected, ok := supported.selectVersion(requested)
	if !ok {
		return 0, nil, UnsupportedProtocolVersion(requested, supported.versions())
	}
	if selected == V2 && string(message.id) == "null" {
		return 0, nil, errNullV2RequestID
	}
	if requested == selected {
		validate := ValidateV1
		if selected == V2 {
			validate = ValidateV2
		}
		if err := validate(message.params); err != nil {
			return 0, nil, err.(*acp.RPCError)
		}
		return selected, nil, nil
	}
	var params json.RawMessage
	var normalizeErr error
	switch {
	case selected == V2:
		params, normalizeErr = normalizeNewerV2(object)
	case requested > V1:
		params, normalizeErr = normalizeV1(object)
	default:
		// A request below v1 keeps v1's fallback-to-latest rule.
		if err := ValidateV1(message.params); err != nil {
			return 0, nil, err.(*acp.RPCError)
		}
		object["protocolVersion"] = json.RawMessage("1")
		params, normalizeErr = canonical[schema1.InitializeRequest](object)
	}
	if normalizeErr != nil {
		return 0, nil, normalizeErr.(*acp.RPCError)
	}
	return selected, params, nil
}

// requestsV2 reports whether message is an initialize request that selects a
// configured v2 implementation. Everything else is answered by a configured
// v1 implementation.
func requestsV2(message message, method string, supported Supported) bool {
	if !supported.V2 || message.notification || message.method != method {
		return false
	}
	requested, err := RequestedVersion(message.params)
	return err == nil && requested >= V2
}

func response(id json.RawMessage, err *acp.RPCError) []byte {
	return mustMarshal(struct {
		Version string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *acp.RPCError   `json:"error"`
	}{Version: "2.0", ID: id, Error: err})
}
