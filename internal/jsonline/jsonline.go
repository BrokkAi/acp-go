// Package jsonline classifies one JSON-RPC wire value the way the reference
// Rust SDK 5.0.0 does before dispatch, and builds the single error that answers
// a value that is not a JSON-RPC message.
//
// A line that is not valid JSON is a parse error: it gets one -32700 error with
// "id": null and the line as data. A JSON array is a batch, which acp-go does
// not support on any protocol version, matching the ACP v2 transports rule: it
// gets one -32600 error with "id": null, and none of its entries run. Either
// way the connection keeps reading. The package also holds the exact -32600
// errors for a valid JSON value that is not a JSON-RPC message and for a
// protocol v2 request with a null id.
//
// Validity follows serde_json, which the reference SDK parses with: besides the
// JSON grammar, a string may not hold a lone UTF-16 surrogate escape, a number
// may not overflow float64, and arrays and objects may nest at most 127 deep.
package jsonline

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"unicode/utf8"
)

// Kind is how the reference SDK classifies one wire value.
type Kind int

const (
	// Message is a JSON value that is not an array. Whether it is a valid
	// JSON-RPC message is the caller's concern.
	Message Kind = iota
	// Invalid is not valid JSON: a -32700 parse error.
	Invalid
	// Batch is a JSON array, including an empty one: a -32600 invalid request.
	Batch
)

// maxDepth is serde_json's recursion limit: its remaining depth starts at 128
// and parsing fails when entering an array or object brings it to zero.
const maxDepth = 127

// batchRejection is the exact wire response, without a trailing newline, that
// the reference SDK sends for a batch array (util::batch_not_supported).
const batchRejection = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"JSON-RPC batches are not supported; send each message individually"}}`

// Classify reports how the reference SDK treats line, a valid UTF-8 wire value
// without its line terminator. Surrounding JSON whitespace is allowed.
func Classify(line []byte) Kind {
	if !Valid(line) {
		return Invalid
	}
	if trimmed := bytes.TrimLeft(line, " \t\r\n"); trimmed[0] == '[' {
		return Batch
	}
	return Message
}

// Valid reports whether data is one JSON value that serde_json accepts.
func Valid(data []byte) bool {
	return valid(data, maxDepth)
}

// ValidAnyDepth is Valid without serde_json's recursion limit, for a value
// the reference SDK builds in memory rather than parses, such as one
// serde_json::to_value produces.
func ValidAnyDepth(data []byte) bool {
	return valid(data, math.MaxInt)
}

func valid(data []byte, maxDepth int) bool {
	if !utf8.Valid(data) || !json.Valid(data) {
		return false
	}
	depth := 0
	for i := 0; i < len(data); i++ {
		switch c := data[i]; {
		case c == '[' || c == '{':
			depth++
			if depth > maxDepth {
				return false
			}
		case c == ']' || c == '}':
			depth--
		case c == '"':
			end, ok := scanString(data, i+1)
			if !ok {
				return false
			}
			i = end
		case c == '-' || (c >= '0' && c <= '9'):
			end := i + 1
			for end < len(data) && isNumberByte(data[end]) {
				end++
			}
			if !numberInRange(data[i:end]) {
				return false
			}
			i = end - 1
		}
	}
	return true
}

// pow10 holds the correctly rounded powers of ten serde_json's POW10 table
// holds, 1e0 through 1e308.
var pow10 = func() [309]float64 {
	var table [309]float64
	for i := range table {
		table[i], _ = strconv.ParseFloat("1e"+strconv.Itoa(i), 64)
	}
	return table
}()

// numberInRange reports whether serde_json, built without its
// float_roundtrip feature as the reference SDK is, accepts the number token,
// which encoding/json has already checked against the JSON grammar. It mirrors
// serde_json's parse_integer, parse_decimal, parse_exponent, and
// f64_from_parts: integers that fit u64 or i64 always pass, and any other
// value fails only when its approximate float64 evaluation overflows. That
// evaluation is not correctly rounded, so serde_json rejects some values
// just below math.MaxFloat64, such as 1.7976931348623158e308, and so does
// this.
func numberInRange(token []byte) bool {
	_, ok := ParseFloat(token)
	return ok
}

// ParseFloat evaluates a JSON number token, already checked against the JSON
// grammar, as serde_json built without float_roundtrip does when it reads the
// number as an f64, and reports whether serde_json accepts it. The value is
// not always the correctly rounded one, exactly as in serde_json. An integer
// that fits u64 or i64 is returned as its float64 conversion; serde_json keeps
// those as integers.
func ParseFloat(token []byte) (float64, bool) {
	negative := token[0] == '-'
	signed := func(f float64) float64 {
		if negative {
			return -f
		}
		return f
	}
	i := 0
	if negative {
		i++
	}
	isDigit := func() bool { return i < len(token) && token[i] >= '0' && token[i] <= '9' }
	// overflows is serde_json's overflow! macro for u64 accumulation.
	overflows := func(significand uint64, digit uint64) bool {
		return significand >= math.MaxUint64/10 && (significand > math.MaxUint64/10 || digit > math.MaxUint64%10)
	}
	var significand uint64
	var exponent int64
	if token[i] == '0' {
		i++
	} else {
		for isDigit() {
			digit := uint64(token[i] - '0')
			if overflows(significand, digit) {
				// parse_long_integer: each remaining digit scales by ten.
				for isDigit() {
					i++
					exponent++
				}
				break
			}
			significand = significand*10 + digit
			i++
		}
	}
	if i == len(token) && exponent == 0 {
		// A u64 or i64 integer (or -0) is always in range.
		return signed(float64(significand)), true
	}
	if i < len(token) && token[i] == '.' {
		i++
		for isDigit() {
			digit := uint64(token[i] - '0')
			if overflows(significand, digit) {
				// parse_decimal_overflow ignores every further digit.
				for isDigit() {
					i++
				}
				break
			}
			significand = significand*10 + digit
			exponent--
			i++
		}
	}
	if i < len(token) && (token[i] == 'e' || token[i] == 'E') {
		i++
		positive := true
		if token[i] == '+' || token[i] == '-' {
			positive = token[i] == '+'
			i++
		}
		var exp int64
		for isDigit() {
			digit := int64(token[i] - '0')
			if exp >= math.MaxInt32/10 && (exp > math.MaxInt32/10 || digit > math.MaxInt32%10) {
				// parse_exponent_overflow: an error instead of infinity,
				// and zero for a tiny or zero value.
				return signed(0), significand == 0 || !positive
			}
			exp = exp*10 + digit
			i++
		}
		if positive {
			exponent = min(exponent+exp, math.MaxInt32)
		} else {
			exponent = max(exponent-exp, math.MinInt32)
		}
	}
	f, ok := fromParts(significand, exponent)
	return signed(f), ok
}

// fromParts is serde_json's f64_from_parts without float_roundtrip: the value
// and whether it is finite.
func fromParts(significand uint64, exponent int64) (float64, bool) {
	f := float64(significand)
	for {
		index := exponent
		if index < 0 {
			index = -index
		}
		if index < int64(len(pow10)) {
			if exponent >= 0 {
				f *= pow10[index]
				return f, !math.IsInf(f, 0)
			}
			return f / pow10[index], true
		}
		if f == 0 {
			return f, true
		}
		if exponent >= 0 {
			return f, false
		}
		f /= 1e308
		exponent += 308
	}
}

func isNumberByte(c byte) bool {
	return c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' || (c >= '0' && c <= '9')
}

// scanString checks the string whose contents start at data[start] and returns
// the index of its closing quote. data is already known to be valid JSON, so
// only serde_json's surrogate pairing rule is checked: a leading surrogate
// escape must be directly followed by a trailing surrogate escape, and a
// trailing surrogate escape may not appear on its own.
func scanString(data []byte, start int) (int, bool) {
	for i := start; i < len(data); i++ {
		switch data[i] {
		case '"':
			return i, true
		case '\\':
			if data[i+1] != 'u' {
				i++
				continue
			}
			n := hexValue(data[i+2 : i+6])
			i += 5
			switch {
			case n >= 0xDC00 && n <= 0xDFFF:
				return 0, false
			case n >= 0xD800 && n <= 0xDBFF:
				if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
					return 0, false
				}
				if low := hexValue(data[i+3 : i+7]); low < 0xDC00 || low > 0xDFFF {
					return 0, false
				}
				i += 6
			}
		}
	}
	return 0, false
}

func hexValue(digits []byte) uint16 {
	value, _ := strconv.ParseUint(string(digits), 16, 16)
	return uint16(value)
}

// BatchRejection returns a fresh copy of the JSON-RPC error response that
// answers a batch array, without a trailing newline.
func BatchRejection() []byte { return []byte(batchRejection) }

// invalidRequest is the exact wire response, without a trailing newline, that
// the reference SDK sends for a valid JSON value that is not a JSON-RPC
// message and is not shaped like a response: Error::invalid_request() with no
// data, sent as an uncorrelated error response.
const invalidRequest = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request"}}`

// nullV2RequestID is the exact wire response, without a trailing newline,
// that the reference SDK sends for a protocol v2 request whose id is null
// (raw_id::null_v2_request_id). The id cannot be echoed, so it is null.
const nullV2RequestID = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"ACP protocol v2 request ids must be a string or an integer, not null"}}`

// InvalidRequest returns a fresh copy of the JSON-RPC error response that
// answers a valid JSON value that is not a JSON-RPC message, without a
// trailing newline.
func InvalidRequest() []byte { return []byte(invalidRequest) }

// NullV2RequestID returns a fresh copy of the JSON-RPC error response that
// answers a protocol v2 request with a null id, without a trailing newline.
func NullV2RequestID() []byte { return []byte(nullV2RequestID) }

// ParseError returns the JSON-RPC error response that answers line, a valid
// UTF-8 value that is not valid JSON, without a trailing newline. It matches
// the reference SDK byte for byte: Error::parse_error() with data
// {"line": line}, escaped as serde_json escapes strings.
func ParseError(line []byte) []byte {
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error","data":{"line":`)
	b.Write(Quote(line))
	b.WriteString(`}}}`)
	return b.Bytes()
}

// Quote encodes s, valid UTF-8, as a JSON string exactly as serde_json does:
// only '"', '\\', and control characters are escaped, using \b \t \n \f \r
// where they exist and lowercase \u00XX otherwise. Everything else, including '<', '>', '&',
// U+2028, and U+2029, is written as is.
func Quote(s []byte) []byte {
	const hex = "0123456789abcdef"
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for _, c := range s {
		switch c {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\b':
			out = append(out, '\\', 'b')
		case '\t':
			out = append(out, '\\', 't')
		case '\n':
			out = append(out, '\\', 'n')
		case '\f':
			out = append(out, '\\', 'f')
		case '\r':
			out = append(out, '\\', 'r')
		default:
			if c < 0x20 {
				out = append(out, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
			} else {
				out = append(out, c)
			}
		}
	}
	return append(out, '"')
}
