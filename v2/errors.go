package v2

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/negotiate"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// UnsupportedProtocolVersionError builds the -32022 (Unsupported protocol
// version) error an endpoint returns when it supports no protocol version at
// or below requested, like the reference SDK's
// v2::schema::Error::unsupported_protocol_version. Its data is the generated
// UnsupportedProtocolVersionErrorData; supported is encoded as an array even
// when empty. The agent runtimes and protocol routers answer with exactly this
// error; a custom endpoint can return it from a Handler.
func UnsupportedProtocolVersionError(requested schema.ProtocolVersion, supported []schema.ProtocolVersion) *acp.RPCError {
	versions := make([]uint16, 0, len(supported))
	for _, version := range supported {
		versions = append(versions, uint16(version))
	}
	return negotiate.UnsupportedProtocolVersion(uint16(requested), versions)
}

// SessionNotFoundError builds the -32002 (Resource not found) error for a
// session-scoped request naming a session that does not exist or is not
// active, with SessionNotFoundErrorData, like the reference SDK's
// v2::schema::Error::session_not_found.
func SessionNotFoundError(sessionID schema.SessionId) *acp.RPCError {
	data, err := json.Marshal(schema.SessionNotFoundErrorData{SessionID: sessionID})
	if err != nil {
		// A session ID is a string, so this cannot happen.
		panic(err)
	}
	return &acp.RPCError{Code: int(schema.ErrorCodeResourceNotFound), Message: "Resource not found", Data: data}
}

// SchemaError converts e to the typed draft-v2 schema error, the explicit
// boundary from the raw wire form, like acp.RPCError.SchemaError for v1 and the
// reference SDK's into_acp_error. Extra members are dropped. Data decodes as
// ErrorData: an omitted or null data is nil, as the v2 schema treats them as
// equivalent; data shaped like UnsupportedProtocolVersionErrorData or
// SessionNotFoundErrorData decodes to that variant whatever the code; and any
// other value is kept verbatim in ErrorData.Other. It fails when Code does not
// fit the schema's int32 ErrorCode, or when Data does not decode as ErrorData,
// such as data that is not valid JSON.
func SchemaError(e *acp.RPCError) (schema.Error, error) {
	if e.Code < math.MinInt32 || e.Code > math.MaxInt32 {
		return schema.Error{}, fmt.Errorf("acp: JSON-RPC error code %d does not fit an ACP ErrorCode", e.Code)
	}
	result := schema.Error{Code: schema.ErrorCode(e.Code), Message: e.Message}
	if len(e.Data) > 0 && string(e.Data) != "null" {
		var data schema.ErrorData
		if err := json.Unmarshal(e.Data, &data); err != nil {
			return schema.Error{}, fmt.Errorf("acp: JSON-RPC error data: %w", err)
		}
		result.Data = &data
	}
	return result, nil
}

// UnsupportedProtocolVersionData decodes the data of a -32022 error: it
// reports ok when err is, or wraps, an *acp.RPCError with code -32022 whose
// data is an UnsupportedProtocolVersionErrorData, with its required requested
// and supported members. Any other code or data shape reports false; use
// SchemaError to inspect it.
func UnsupportedProtocolVersionData(err error) (schema.UnsupportedProtocolVersionErrorData, bool) {
	data, ok := errorData(err, acp.ErrorCodeUnsupportedProtocolVersion)
	if !ok || data.UnsupportedProtocolVersion == nil {
		return schema.UnsupportedProtocolVersionErrorData{}, false
	}
	return *data.UnsupportedProtocolVersion, true
}

// SessionNotFoundData decodes the data of a -32002 error for an unknown
// session: it reports ok when err is, or wraps, an *acp.RPCError with code
// -32002 whose data is a SessionNotFoundErrorData, with its required sessionId.
// A -32002 error without that data, such as one for another kind of resource,
// reports false.
func SessionNotFoundData(err error) (schema.SessionNotFoundErrorData, bool) {
	data, ok := errorData(err, acp.ErrorCodeResourceNotFound)
	if !ok || data.SessionNotFound == nil {
		return schema.SessionNotFoundErrorData{}, false
	}
	return *data.SessionNotFound, true
}

func errorData(err error, code int) (schema.ErrorData, bool) {
	var rpc *acp.RPCError
	if !errors.As(err, &rpc) || rpc.Code != code {
		return schema.ErrorData{}, false
	}
	converted, convertErr := SchemaError(rpc)
	if convertErr != nil || converted.Data == nil {
		return schema.ErrorData{}, false
	}
	return *converted.Data, true
}
