package negotiate

import (
	"encoding/json"
	"fmt"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/castparams"
	"github.com/BrokkAi/acp-go/internal/serdejson"
)

// SelectedVersion reads the protocolVersion an initialize response selects,
// as the reference SDK's initiator does: the member must exist under exactly
// that name and be a JSON integer that fits u16, or the response fails with
// InvalidProtocolVersion.
func SelectedVersion(result json.RawMessage) (uint16, error) {
	return RequestedVersion(result)
}

// UnsupportedSelectedVersion is the reference SDK's -32600 for an initialize
// response selecting a version the endpoint does not know.
func UnsupportedSelectedVersion(selected, supported uint16) *acp.RPCError {
	return InvalidRequest(fmt.Sprintf("unsupported ACP protocol version %d; this endpoint only supports ACP protocol version %d", selected, supported))
}

// RequiredVersion is the reference SDK's -32600 for an initialize response
// selecting a known version the caller cannot use.
func RequiredVersion(required, negotiated uint16) *acp.RPCError {
	return InvalidRequest(fmt.Sprintf("required ACP protocol version %d but peer negotiated %d; use a matching implementation for the negotiated protocol version", required, negotiated))
}

// CheckV2Response checks a v2 initialize response as the reference SDK's
// InitializeResponse::from_json does: its info must parse; everything else
// defaults on error. A failure is json_cast's -32700 (Parse error) with data
// {"error": <serde reason>, "json": <response>, "phase": "deserialization"}.
func CheckV2Response(result json.RawMessage) error {
	value, err := serdejson.ParseAnyDepth(result)
	if err != nil {
		return parseError("invalid initialize response")
	}
	if detail := implementationError(value); detail != "" {
		return &acp.RPCError{Code: -32700, Message: "Parse error", Data: castparams.DataValue(detail, value)}
	}
	return nil
}

func parseError(detail string) *acp.RPCError {
	return errorWithData(-32700, "Parse error", detail)
}

// DecodeResponse decodes an initialize response into target. The generated
// decoders read it as the reference schema does: member names match exactly,
// and every DefaultOnError field, at every level, is ignored when it does not
// parse. Call CheckV2Response first for a v2 response, whose info is required;
// any other failure would be a decoder defect, which is reported as an
// internal error rather than Go's error text.
func DecodeResponse(result json.RawMessage, target any) error {
	if err := json.Unmarshal(result, target); err != nil {
		return internalError("initialize response did not decode")
	}
	return nil
}
