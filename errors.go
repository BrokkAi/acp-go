package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// other member, so the result is semantically unchanged. Members after code,
// message, and data are re-emitted in sorted key order, and whitespace is not
// kept.
//
// Code must fit int32, the reference SDK's code type. Decoding rejects an
// object whose code or message is missing, null, or of the wrong type; see
// UnmarshalJSON.
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
// Data and Extra values must be valid JSON and Code must fit int32; otherwise
// encoding fails, and a Handler error that cannot be encoded is answered with
// -32603 instead. A Handler that returns an *RPCError received from Call
// forwards the peer's Extra members along with its code, message, and data.
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
	if !validErrorCode(e.Code) {
		return nil, fmt.Errorf("acp: JSON-RPC error code %d does not fit int32", e.Code)
	}
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
// omitted "data" and collecting every other member into Extra. It replaces the
// receiver. Member names match exactly.
//
// Like the reference SDK, it rejects anything other than a JSON object, a
// missing or null "code" or "message", a "code" that is not an integer in the
// int32 range (including 1.0, 1e3, and -0, which the reference SDK reads as
// floats),
// a non-string "message", and a repeated "code", "message", or "data". A
// repeated extension member keeps its last value.
func (e *RPCError) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return errors.New("acp: JSON-RPC error must be an object")
	}
	var decoded RPCError
	var hasCode, hasMessage, hasData bool
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("acp: JSON-RPC error member name is not a string")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
		switch key {
		case "code":
			if hasCode {
				return errors.New("acp: duplicate JSON-RPC error code")
			}
			hasCode = true
			code, err := decodeErrorCode(value)
			if err != nil {
				return err
			}
			decoded.Code = code
		case "message":
			if hasMessage {
				return errors.New("acp: duplicate JSON-RPC error message")
			}
			hasMessage = true
			if string(value) == "null" {
				return errors.New("acp: JSON-RPC error message is null")
			}
			if err := json.Unmarshal(value, &decoded.Message); err != nil {
				return fmt.Errorf("acp: JSON-RPC error message: %w", err)
			}
		case "data":
			if hasData {
				return errors.New("acp: duplicate JSON-RPC error data")
			}
			hasData = true
			decoded.Data = value
		default:
			if decoded.Extra == nil {
				decoded.Extra = make(map[string]json.RawMessage)
			}
			decoded.Extra[key] = value
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("acp: trailing data after JSON-RPC error")
	}
	if !hasCode {
		return errors.New("acp: JSON-RPC error is missing code")
	}
	if !hasMessage {
		return errors.New("acp: JSON-RPC error is missing message")
	}
	*e = decoded
	return nil
}

// decodeErrorCode accepts exactly the JSON integers that fit int32, the type
// of the reference SDK's raw error code.
func decodeErrorCode(value json.RawMessage) (int, error) {
	if len(value) == 0 || (value[0] != '-' && (value[0] < '0' || value[0] > '9')) {
		return 0, fmt.Errorf("acp: JSON-RPC error code %s is not an integer", value)
	}
	if string(value) == "-0" {
		return 0, errors.New("acp: JSON-RPC error code -0 is not an integer")
	}
	code, err := strconv.ParseInt(string(value), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("acp: JSON-RPC error code %s is not an int32 integer", value)
	}
	return int(code), nil
}

func validErrorCode(code int) bool {
	return code >= math.MinInt32 && code <= math.MaxInt32
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
	if !validErrorCode(e.Code) {
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
