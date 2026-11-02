package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"

	"github.com/BrokkAi/acp-go/schema"
)

// ACP-specific JSON-RPC error codes.
const (
	ErrorCodeAuthRequired     = -32000
	ErrorCodeResourceNotFound = -32002
)

// RPCError is a JSON-RPC error object as it appears on the wire. Connection.Call
// returns one when the peer answers with an error response, and a Handler can
// return one (directly or wrapped) to choose the error response it sends.
//
// RPCError is lossless, like the reference SDK's raw JSON-RPC error: decoding
// and re-encoding an error object keeps its code, message, data, and every
// other member.
//
// Data distinguishes three cases. A nil or empty Data means the object had no
// "data" member, and none is encoded. The literal JSON null means the object
// carried "data": null, and it is encoded unchanged. Any other value is the
// member's raw JSON.
//
// Extra holds the members other than "code", "message", and "data", such as
// extension fields, keyed by their exact, case-sensitive name. Those three
// names belong to the typed fields: decoding never puts them in Extra, and
// encoding ignores them in Extra, so an error object never carries a member
// twice. An Extra entry with an empty value is omitted. Extra members are
// encoded after the typed fields in sorted key order.
//
// Data and Extra values must be valid JSON; otherwise encoding fails, and a
// Handler error that cannot be encoded is answered with -32603 instead.
type RPCError struct {
	Code    int
	Message string
	Data    json.RawMessage
	Extra   map[string]json.RawMessage
}

func (e *RPCError) Error() string { return fmt.Sprintf("ACP error %d: %s", e.Code, e.Message) }

// MarshalJSON encodes the error object with its typed fields first, then the
// Extra members in sorted key order.
func (e RPCError) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`{"code":`)
	b.WriteString(strconv.Itoa(e.Code))
	message, err := json.Marshal(e.Message)
	if err != nil {
		return nil, err
	}
	b.WriteString(`,"message":`)
	b.Write(message)
	if len(e.Data) > 0 {
		if !json.Valid(e.Data) {
			return nil, errors.New("acp: RPCError data is not valid JSON")
		}
		b.WriteString(`,"data":`)
		b.Write(e.Data)
	}
	keys := make([]string, 0, len(e.Extra))
	for key, value := range e.Extra {
		if !reservedErrorMember(key) && len(value) > 0 {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		value := e.Extra[key]
		if !json.Valid(value) {
			return nil, fmt.Errorf("acp: RPCError member %q is not valid JSON", key)
		}
		name, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		b.WriteByte(',')
		b.Write(name)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON decodes an error object, keeping "data": null distinct from an
// omitted "data" and collecting every other unknown member into Extra. Member
// names match exactly. A missing or null "code" or "message" decodes as the
// zero value, and JSON null leaves the receiver unchanged.
func (e *RPCError) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return err
	}
	var decoded RPCError
	for key, value := range members {
		switch key {
		case "code":
			if err := json.Unmarshal(value, &decoded.Code); err != nil {
				return fmt.Errorf("acp: RPCError code: %w", err)
			}
		case "message":
			if err := json.Unmarshal(value, &decoded.Message); err != nil {
				return fmt.Errorf("acp: RPCError message: %w", err)
			}
		case "data":
			decoded.Data = value
		default:
			if decoded.Extra == nil {
				decoded.Extra = make(map[string]json.RawMessage)
			}
			decoded.Extra[key] = value
		}
	}
	*e = decoded
	return nil
}

func reservedErrorMember(name string) bool {
	return name == "code" || name == "message" || name == "data"
}

// SchemaError converts e to the typed v1 schema error. This is the explicit
// boundary from the raw wire form to the ACP type, matching the reference
// SDK's into_acp_error: Extra members are dropped, and Data keeps its omitted,
// null, or value form. It fails when Code does not fit the schema's int32
// ErrorCode.
func (e *RPCError) SchemaError() (schema.Error, error) {
	if e.Code < math.MinInt32 || e.Code > math.MaxInt32 {
		return schema.Error{}, fmt.Errorf("acp: JSON-RPC error code %d does not fit an ACP ErrorCode", e.Code)
	}
	return schema.Error{Code: schema.ErrorCode(e.Code), Message: e.Message, Data: e.Data}, nil
}

func rpcErrorCode(err error) (int, bool) {
	var rpc *RPCError
	if errors.As(err, &rpc) {
		return rpc.Code, true
	}
	return 0, false
}

// IsAuthRequired reports whether err is, or wraps, an *RPCError with code
// -32000. Only the code is inspected; data and extension members do not affect
// the result.
func IsAuthRequired(err error) bool {
	code, ok := rpcErrorCode(err)
	return ok && code == ErrorCodeAuthRequired
}

// IsResourceNotFound reports whether err is, or wraps, an *RPCError with code
// -32002. Only the code is inspected; data and extension members do not affect
// the result.
func IsResourceNotFound(err error) bool {
	code, ok := rpcErrorCode(err)
	return ok && code == ErrorCodeResourceNotFound
}
