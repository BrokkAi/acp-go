package negotiate

import (
	"encoding/json"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/serdejson"
)

// successorMethod is the reference SDK 5.0.0's proxy successor method, which
// its outgoing initialize check matches literally.
const successorMethod = "proxy/successor"

// SuccessorEnclosesInitialize reports whether a request already wraps an
// initialize in proxy/successor, as the reference SDK's
// successor_encloses_initialize does: it follows nested proxy/successor
// params to the innermost method and reports whether that is initialize or
// proxy/initialize.
func SuccessorEnclosesInitialize(method string, params json.RawMessage) bool {
	wrapped := false
	for method == successorMethod {
		wrapped = true
		object, ok := decodeObject(params)
		if !ok {
			return false
		}
		var inner string
		if !isString(object["method"]) || json.Unmarshal(object["method"], &inner) != nil {
			return false
		}
		method = inner
		if method == successorMethod {
			next, ok := object["params"]
			if !ok {
				return false
			}
			params = next
		}
	}
	return wrapped && (method == "initialize" || method == "proxy/initialize")
}

// InvalidPrewrappedInitialize refuses an outgoing initialize that is already
// wrapped in proxy/successor, matching the reference SDK.
func InvalidPrewrappedInitialize() *acp.RPCError {
	return InvalidRequest("initialize requests must be sent as a logical `initialize` message; `proxy/successor` wrapping is applied by connection routing")
}

// OutgoingV1Params prepares the params of an initialize a v1 endpoint sends,
// as the reference SDK's outgoing_message does: it sets protocolVersion to 1
// in place. Params that are not an object are -32602 (InvalidProtocolVersion).
func OutgoingV1Params(params json.RawMessage) (json.RawMessage, error) {
	value, err := serdejson.ParseAnyDepth(params)
	if err != nil || value.Kind != serdejson.Object {
		return nil, InvalidProtocolVersion()
	}
	value.Set("protocolVersion", &serdejson.Value{Kind: serdejson.Integer, Integer: "1"})
	return serdejson.Encode(value), nil
}
