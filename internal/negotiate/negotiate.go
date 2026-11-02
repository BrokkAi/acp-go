// Package negotiate implements ACP protocol-version negotiation the way the
// reference Rust SDK 5.0.0 does: the v1 and draft-v2 agent runtimes negotiate
// each initialize request they receive, and the agent and proxy protocol
// routers select one configured implementation from the first initialize
// request on a connection.
//
// The rule is the same everywhere: an endpoint selects the highest version it
// supports at or below the requested one. V1 keeps its original
// fallback-to-latest rule, so a v1 endpoint selects v1 for any request. A v2
// endpoint answers a request below v2 with -32022 (Unsupported protocol
// version) and the versions it supports. A request for a newer version than
// the selected one keeps what parses under the selected version instead of
// being rejected. A missing or malformed protocolVersion is -32602.
package negotiate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/jsonline"
	"github.com/BrokkAi/acp-go/internal/serdejson"
	schema1 "github.com/BrokkAi/acp-go/schema"
	schema2 "github.com/BrokkAi/acp-go/schema/v2"
)

// UnknownImplementation is the name and version substituted for a missing or
// malformed info when a v2 endpoint selects v2 for a newer request.
const UnknownImplementation = "unknown"

// The versions this SDK implements.
const (
	V1 uint16 = 1
	V2 uint16 = 2
)

// InvalidRequest is the reference SDK's -32600 error: the generic message,
// with detail as a JSON string in data.
func InvalidRequest(detail string) *acp.RPCError {
	return errorWithData(-32600, "Invalid request", detail)
}

// InvalidInitializeDirection refuses an initialize sent the wrong way: by an
// agent, or to a client, matching the reference SDK.
func InvalidInitializeDirection() *acp.RPCError {
	return InvalidRequest("ACP clients send `initialize` requests and ACP agents respond to them")
}

// InitializationRequired refuses a request that arrives before
// initialization has completed, matching the reference SDK.
func InitializationRequired(method string) *acp.RPCError {
	return InvalidRequest(fmt.Sprintf("ACP initialization must complete before `%s` can be used", method))
}

// InvalidParams is the reference SDK's -32602 error with detail as data.
func InvalidParams(detail string) *acp.RPCError {
	return errorWithData(-32602, "Invalid params", detail)
}

// InvalidProtocolVersion answers an initialize request whose protocolVersion
// is missing, is not an integer, or does not fit uint16.
func InvalidProtocolVersion() *acp.RPCError {
	return InvalidParams("initialize.protocolVersion must be a valid ACP protocol version")
}

// InvalidInitializeParams answers initialize parameters that do not parse
// under the selected version.
func InvalidInitializeParams(detail string) *acp.RPCError {
	return InvalidParams("invalid initialize params: " + detail)
}

// UnsupportedProtocolVersion is the -32022 error for an endpoint that supports
// no version at or below requested. Its data is the v2 schema's
// UnsupportedProtocolVersionErrorData; supported is encoded as an array even
// when empty, as both members are required.
func UnsupportedProtocolVersion(requested uint16, supported []uint16) *acp.RPCError {
	versions := make([]schema2.ProtocolVersion, 0, len(supported))
	for _, version := range supported {
		versions = append(versions, schema2.ProtocolVersion(version))
	}
	data, err := json.Marshal(schema2.UnsupportedProtocolVersionErrorData{
		Requested: schema2.ProtocolVersion(requested),
		Supported: versions,
	})
	if err != nil {
		// The data holds only integers, so this cannot happen.
		panic(err)
	}
	return &acp.RPCError{
		Code:    int(schema2.ErrorCodeUnsupportedProtocolVersion),
		Message: "Unsupported protocol version",
		Data:    data,
	}
}

func errorWithData(code int, message, detail string) *acp.RPCError {
	data, _ := json.Marshal(detail)
	return &acp.RPCError{Code: code, Message: message, Data: data}
}

// RequestedVersion reads the protocolVersion member of initialize params. It
// fails with InvalidProtocolVersion when params is not an object, the member
// is missing or null, or its value is not a JSON integer that fits uint16.
func RequestedVersion(params json.RawMessage) (uint16, error) {
	object, ok := decodeObject(params)
	if !ok {
		return 0, InvalidProtocolVersion()
	}
	return requestedVersion(object)
}

func requestedVersion(params map[string]json.RawMessage) (uint16, error) {
	raw, ok := params["protocolVersion"]
	if !ok {
		return 0, InvalidProtocolVersion()
	}
	version, ok := parseUint16(raw)
	if !ok {
		return 0, InvalidProtocolVersion()
	}
	return version, nil
}

// parseUint16 accepts exactly the JSON integers serde reads as a u16: no sign,
// fraction, exponent, or -0.
func parseUint16(raw json.RawMessage) (uint16, bool) {
	text := string(bytes.TrimSpace(raw))
	if text == "" || text[0] < '0' || text[0] > '9' {
		return 0, false
	}
	value, err := strconv.ParseUint(text, 10, 16)
	if err != nil {
		return 0, false
	}
	return uint16(value), true
}

// V1Params negotiates initialize params for a v1 endpoint, which keeps v1's
// fallback-to-latest rule: every valid request selects v1. A request for a
// newer version is normalized to v1's shape, keeping only what parses under
// v1. The result carries protocolVersion 1; the caller still decodes it as a
// v1 InitializeRequest.
func V1Params(params json.RawMessage) (json.RawMessage, error) {
	object, ok := decodeObject(params)
	if !ok {
		return nil, InvalidProtocolVersion()
	}
	requested, err := requestedVersion(object)
	if err != nil {
		return nil, err
	}
	if requested > V1 {
		return normalizeV1(object)
	}
	object["protocolVersion"] = json.RawMessage("1")
	return json.Marshal(object)
}

// OutgoingV2Params prepares the params of an initialize a v2 endpoint sends,
// as the reference SDK's outgoing_message does: it sets protocolVersion to 2
// in place, keeping every other member and its order, and validates the
// result as a v2 InitializeRequest. Params that are not an object are -32602
// (InvalidProtocolVersion); params whose info does not parse are -32602 with
// the reference SDK's deserialization data. The params are a value the caller
// built, so serde_json's parse recursion limit does not apply.
func OutgoingV2Params(params json.RawMessage) (json.RawMessage, error) {
	value, err := serdejson.ParseAnyDepth(params)
	if err != nil || value.Kind != serdejson.Object {
		return nil, InvalidProtocolVersion()
	}
	value.Set("protocolVersion", &serdejson.Value{Kind: serdejson.Integer, Integer: "2"})
	if detail := implementationError(value); detail != "" {
		return nil, deserializationError(detail, value)
	}
	return serdejson.Encode(value), nil
}

// V2Params negotiates initialize params for a v2 endpoint, like the reference
// SDK's v2 responder. A request below v2 gets -32022 listing v2 as the only
// supported version. A request for a newer version keeps what parses under
// v2. A request for exactly v2 must carry an info that parses, and is read as
// leniently as the reference schema reads it: a capability, a _meta, or an
// info title or _meta that does not parse is ignored. An exact v2 request
// whose info does not parse is -32602 with the reference SDK's
// deserialization data. The result carries protocolVersion 2.
func V2Params(params json.RawMessage) (json.RawMessage, error) {
	object, ok := decodeObject(params)
	if !ok {
		return nil, InvalidProtocolVersion()
	}
	requested, err := requestedVersion(object)
	if err != nil {
		return nil, err
	}
	switch {
	case requested < V2:
		return nil, UnsupportedProtocolVersion(requested, []uint16{V2})
	case requested > V2:
		return normalizeNewerV2(object)
	}
	parsed, _ := serdejson.Parse(params)
	if detail := implementationError(parsed); detail != "" {
		return nil, deserializationError(detail, parsed)
	}
	return normalizeExactV2(object)
}

// ValidateV2 checks params, an object that requests exactly v2, as the
// reference agent router does before handing the frame to a v2
// implementation: the info must parse, and everything else is read
// leniently. A failure is -32602 with the serde reason, such as
// "invalid initialize params: missing field `info`".
func ValidateV2(params json.RawMessage) error {
	parsed, err := serdejson.Parse(params)
	if err != nil || parsed.Kind != serdejson.Object {
		return InvalidProtocolVersion()
	}
	if detail := implementationError(parsed); detail != "" {
		return InvalidInitializeParams(detail)
	}
	return nil
}

// deserializationError is the reference SDK's -32602 for params that do not
// deserialize as the typed request (json_cast_params): data carries the serde
// reason, the params as the reference SDK holds them, and the phase.
func deserializationError(detail string, params *serdejson.Value) *acp.RPCError {
	var data bytes.Buffer
	data.WriteString(`{"error":`)
	data.Write(jsonline.Quote([]byte(detail)))
	data.WriteString(`,"json":`)
	data.Write(serdejson.Encode(params))
	data.WriteString(`,"phase":"deserialization"}`)
	return &acp.RPCError{Code: -32602, Message: "Invalid params", Data: data.Bytes()}
}

// normalizeExactV2 canonicalizes an exact v2 request whose info parses. The
// generated decoder reads it as the reference schema does: member names match
// exactly, and each field the schema marks DefaultOnError, at every level,
// is ignored when it does not parse.
func normalizeExactV2(params map[string]json.RawMessage) (json.RawMessage, error) {
	return canonical[schema2.InitializeRequest](params)
}

// ValidateV1 checks that params is a valid v1 InitializeRequest.
func ValidateV1(params json.RawMessage) error {
	var request schema1.InitializeRequest
	if err := json.Unmarshal(params, &request); err != nil {
		return InvalidInitializeParams(err.Error())
	}
	return nil
}

// normalizeNewerV2 canonicalizes a newer client's initialize params as a v2
// request, like the reference SDK's normalize_newer_v2_initialize_request.
// The endpoint selected v2 for a request written for a version it does not
// know, so it ignores what it cannot parse instead of rejecting the request:
// a missing or malformed info becomes UnknownImplementation for both name and
// version, and the generated decoder ignores every other member that does not
// parse, as the reference schema's DefaultOnError fields do.
func normalizeNewerV2(params map[string]json.RawMessage) (json.RawMessage, error) {
	if !parses[schema2.Implementation](params["info"]) {
		params["info"] = mustMarshal(map[string]string{"name": UnknownImplementation, "version": UnknownImplementation})
	}
	params["protocolVersion"] = json.RawMessage("2")
	return canonical[schema2.InitializeRequest](params)
}

// normalizeV1 canonicalizes a newer client's initialize params as a v1
// request, like the reference SDK's normalize_v1_initialize_request. Only
// the shared surface is carried across: identity, terminal authentication,
// elicitation, and _meta. Unknown newer fields never enable v1's file-system
// or terminal-execution capabilities. What does not parse under v1 is dropped:
// a missing or malformed info leaves clientInfo unset.
func normalizeV1(params map[string]json.RawMessage) (json.RawMessage, error) {
	capabilities := map[string]json.RawMessage{}
	if source, ok := decodeObject(params["capabilities"]); ok {
		for _, key := range []string{"auth", "elicitation", "_meta"} {
			if value, ok := source[key]; ok {
				capabilities[key] = value
			}
		}
	}
	if auth, ok := decodeObject(capabilities["auth"]); ok {
		_, terminal := decodeObject(auth["terminal"])
		auth["terminal"] = json.RawMessage(strconv.FormatBool(terminal))
		capabilities["auth"] = mustMarshal(auth)
	}
	capabilities["session"] = json.RawMessage(`{"configOptions":{"boolean":{}}}`)
	target := map[string]json.RawMessage{
		"protocolVersion":    json.RawMessage("1"),
		"clientCapabilities": mustMarshal(capabilities),
	}
	if info, ok := params["info"]; ok && parses[schema1.Implementation](info) {
		target["clientInfo"] = info
	}
	if meta, ok := params["_meta"]; ok {
		target["_meta"] = meta
	}
	return canonical[schema1.InitializeRequest](target)
}

// parses reports whether raw is present and decodes as T, as the reference
// schema's deserializer judges it.
func parses[T any](raw json.RawMessage) bool {
	if raw == nil {
		return false
	}
	var value T
	return json.Unmarshal(raw, &value) == nil
}

// canonical decodes params as T and re-encodes it. The generated decoder
// drops what T does not define and what its DefaultOnError fields cannot
// parse, as the reference schema does.
func canonical[T any](params map[string]json.RawMessage) (json.RawMessage, error) {
	var request T
	if err := json.Unmarshal(mustMarshal(params), &request); err != nil {
		// The params were normalized to what parses, so this is an internal
		// failure, never a Go error text on the wire.
		return nil, internalError("initialize params did not normalize")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, internalError("initialize params did not serialize to an object")
	}
	return encoded, nil
}

// internalError is the reference SDK's -32603 with detail as data.
func internalError(detail string) *acp.RPCError {
	return errorWithData(-32603, "Internal error", detail)
}

func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(trimmed, &object) != nil {
		return nil, false
	}
	return object, true
}

func isString(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '"'
}

func mustMarshal(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("negotiate: encode %T: %v", value, err))
	}
	return encoded
}
