// Package serdejson reads and writes JSON exactly as the reference Rust SDK's
// serde_json does, built as the SDK builds it: with preserve_order, without
// arbitrary_precision or float_roundtrip. A Value is serde_json::Value:
// objects keep each member at the position of its first occurrence with the
// value of its last, integers that fit u64 or i64 stay integers, and every
// other number is an f64 evaluated as serde_json evaluates it. Encode is
// serde_json::to_string.
package serdejson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/BrokkAi/acp-go/internal/jsonline"
)

// Kind is the variant of a Value.
type Kind int

const (
	Null Kind = iota
	Bool
	// Integer is a number that fits u64 or i64; Value.Integer holds its
	// decimal text.
	Integer
	// Float is any other number; Value.Float holds it.
	Float
	String
	Array
	Object
)

// Value is a serde_json::Value.
type Value struct {
	Kind Kind
	// Bool is set for Bool.
	Bool bool
	// Integer is the canonical decimal text of an Integer, such as "5" or
	// "-3".
	Integer string
	// Float is set for Float.
	Float float64
	// String is the decoded text of a String.
	String string
	// Items are the elements of an Array.
	Items []*Value
	// Members are the members of an Object, in order, with unique keys.
	Members []Member
}

// Member is one member of an Object.
type Member struct {
	Key   string
	Value *Value
}

// ErrInvalid reports input serde_json does not accept as one JSON value.
var ErrInvalid = errors.New("serdejson: not a JSON value serde_json accepts")

// Parse reads data, one JSON value with optional surrounding whitespace, as
// serde_json::from_slice::<Value> does. It fails with ErrInvalid on anything
// serde_json rejects, such as a lone surrogate escape, a number that
// overflows f64, or nesting deeper than 127.
func Parse(data []byte) (*Value, error) {
	if !jsonline.Valid(data) {
		return nil, ErrInvalid
	}
	return parse(data)
}

// ParseAnyDepth is Parse without serde_json's recursion limit, for a value
// the reference SDK holds in memory rather than parses from text, such as one
// serde_json::to_value builds from a Rust value.
func ParseAnyDepth(data []byte) (*Value, error) {
	if !jsonline.ValidAnyDepth(data) {
		return nil, ErrInvalid
	}
	return parse(data)
}

func parse(data []byte) (*Value, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decode(decoder)
	if err != nil {
		return nil, ErrInvalid
	}
	return value, nil
}

func decode(decoder *json.Decoder) (*Value, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token := token.(type) {
	case nil:
		return &Value{Kind: Null}, nil
	case bool:
		return &Value{Kind: Bool, Bool: token}, nil
	case string:
		return &Value{Kind: String, String: token}, nil
	case json.Number:
		return number(string(token))
	case json.Delim:
		switch token {
		case '[':
			result := &Value{Kind: Array}
			for decoder.More() {
				item, err := decode(decoder)
				if err != nil {
					return nil, err
				}
				result.Items = append(result.Items, item)
			}
			_, err := decoder.Token()
			return result, err
		case '{':
			result := &Value{Kind: Object}
			// Set scans the members; a large object is indexed instead, so
			// parsing stays linear in its size.
			var index map[string]int
			for decoder.More() {
				token, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				item, err := decode(decoder)
				if err != nil {
					return nil, err
				}
				key := token.(string)
				switch {
				case index != nil:
					if i, ok := index[key]; ok {
						result.Members[i].Value = item
					} else {
						index[key] = len(result.Members)
						result.Members = append(result.Members, Member{Key: key, Value: item})
					}
				case len(result.Members) < indexedObject:
					result.Set(key, item)
				default:
					index = make(map[string]int, 2*len(result.Members))
					for i, member := range result.Members {
						index[member.Key] = i
					}
					if i, ok := index[key]; ok {
						result.Members[i].Value = item
					} else {
						index[key] = len(result.Members)
						result.Members = append(result.Members, Member{Key: key, Value: item})
					}
				}
			}
			_, err := decoder.Token()
			return result, err
		}
	}
	return nil, ErrInvalid
}

// indexedObject is the member count from which parsing indexes an object's
// keys rather than scanning them.
const indexedObject = 16

// number reads a JSON number token as serde_json does.
func number(text string) (*Value, error) {
	if !strings.ContainsAny(text, ".eE") && text != "-0" {
		if _, err := strconv.ParseUint(text, 10, 64); err == nil {
			return &Value{Kind: Integer, Integer: text}, nil
		}
		if _, err := strconv.ParseInt(text, 10, 64); err == nil {
			return &Value{Kind: Integer, Integer: text}, nil
		}
	}
	f, ok := jsonline.ParseFloat([]byte(text))
	if !ok {
		return nil, ErrInvalid
	}
	return &Value{Kind: Float, Float: f}, nil
}

// Get returns the member named key of an Object.
func (v *Value) Get(key string) (*Value, bool) {
	if v == nil || v.Kind != Object {
		return nil, false
	}
	for _, member := range v.Members {
		if member.Key == key {
			return member.Value, true
		}
	}
	return nil, false
}

// Set sets the member named key of an Object as serde_json's Map::insert
// does with preserve_order: an existing member keeps its position, and a new
// one is appended.
func (v *Value) Set(key string, item *Value) {
	for i := range v.Members {
		if v.Members[i].Key == key {
			v.Members[i].Value = item
			return
		}
	}
	v.Members = append(v.Members, Member{Key: key, Value: item})
}

// Encode writes v as serde_json::to_string does: compact, members in order,
// strings with only the escapes serde_json writes, and floats in serde_json's
// shortest form. A nil Value encodes as null.
func Encode(v *Value) []byte {
	var b bytes.Buffer
	encode(&b, v)
	return b.Bytes()
}

func encode(b *bytes.Buffer, v *Value) {
	if v == nil {
		b.WriteString("null")
		return
	}
	switch v.Kind {
	case Null:
		b.WriteString("null")
	case Bool:
		b.WriteString(strconv.FormatBool(v.Bool))
	case Integer:
		b.WriteString(v.Integer)
	case Float:
		b.WriteString(FormatFloat(v.Float))
	case String:
		b.Write(jsonline.Quote([]byte(v.String)))
	case Array:
		b.WriteByte('[')
		for i, item := range v.Items {
			if i > 0 {
				b.WriteByte(',')
			}
			encode(b, item)
		}
		b.WriteByte(']')
	case Object:
		b.WriteByte('{')
		for i, member := range v.Members {
			if i > 0 {
				b.WriteByte(',')
			}
			b.Write(jsonline.Quote([]byte(member.Key)))
			b.WriteByte(':')
			encode(b, member.Value)
		}
		b.WriteByte('}')
	}
}

// FormatFloat formats an f64 as serde_json writes it and as serde's
// Unexpected::Float displays it: the shortest digits that round-trip, as a
// decimal with at least one fractional digit when the decimal point falls
// within 16 digits before or 5 zeros after the first digit, such as 1000.0
// or 0.00001, and in exponent form otherwise, such as 1e+300 or 1.5e-7. A
// non-finite value, which serde_json writes as null, formats as "null".
func FormatFloat(f float64) string {
	var buf [32]byte
	return string(AppendFloat(buf[:0], f))
}

// AppendFloat appends f as FormatFloat formats it, without allocating
// beyond out.
func AppendFloat(out []byte, f float64) []byte {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return append(out, "null"...)
	}
	if f == 0 {
		if math.Signbit(f) {
			return append(out, "-0.0"...)
		}
		return append(out, "0.0"...)
	}
	if f < 0 {
		out = append(out, '-')
		f = -f
	}
	// The shortest digits that round-trip, as d.ddde±x.
	var buf [32]byte
	scientific := strconv.AppendFloat(buf[:0], f, 'e', -1, 64)
	e := bytes.IndexByte(scientific, 'e')
	var digitBuf [24]byte
	digits := append(digitBuf[:0], scientific[0])
	if e > 1 {
		digits = append(digits, scientific[2:e]...)
	}
	exponent := 0
	for _, c := range scientific[e+2:] {
		exponent = exponent*10 + int(c-'0')
	}
	if scientific[e+1] == '-' {
		exponent = -exponent
	}
	point := exponent + 1
	switch {
	case point > 0 && point <= 16:
		if len(digits) <= point {
			out = append(out, digits...)
			for i := len(digits); i < point; i++ {
				out = append(out, '0')
			}
			return append(out, '.', '0')
		}
		out = append(out, digits[:point]...)
		out = append(out, '.')
		return append(out, digits[point:]...)
	case point > -5 && point <= 0:
		out = append(out, '0', '.')
		for i := 0; i < -point; i++ {
			out = append(out, '0')
		}
		return append(out, digits...)
	}
	out = append(out, digits[0])
	if len(digits) > 1 {
		out = append(out, '.')
		out = append(out, digits[1:]...)
	}
	out = append(out, 'e')
	if exponent >= 0 {
		out = append(out, '+')
	}
	return strconv.AppendInt(out, int64(exponent), 10)
}

// Unexpected describes v as serde's de::Unexpected does in an invalid-type
// message, such as "integer `5`", "string \"x\"", "sequence", or "map".
func Unexpected(v *Value) string {
	switch v.Kind {
	case Null:
		return "null"
	case Bool:
		return "boolean `" + strconv.FormatBool(v.Bool) + "`"
	case Integer:
		return "integer `" + v.Integer + "`"
	case Float:
		return "floating point `" + FormatFloat(v.Float) + "`"
	case String:
		return "string " + DebugString(v.String)
	case Array:
		return "sequence"
	default:
		return "map"
	}
}

// InvalidType is serde's invalid-type error message for v, such as
// "invalid type: integer `5`, expected a string".
func InvalidType(v *Value, expected string) string {
	return fmt.Sprintf("invalid type: %s, expected %s", Unexpected(v), expected)
}

// graphemeExtend reports Rust's char::is_grapheme_extended: the Unicode
// Grapheme_Extend property, Mn, Me, and Other_Grapheme_Extend.
func graphemeExtend(r rune) bool {
	if r == 0xff9e || r == 0xff9f {
		// Unicode 16, which Rust's tables follow, removed the halfwidth
		// katakana sound marks from Other_Grapheme_Extend.
		return false
	}
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Other_Grapheme_Extend)
}

// printable reports Rust's core::unicode::printable::is_printable: every
// assigned character except the categories Cc, Cf, Cs, Co, Zl, Zp, and Zs
// other than U+0020.
func printable(r rune) bool {
	switch {
	case r == ' ':
		return true
	case unicode.In(r, unicode.Cc, unicode.Cf, unicode.Cs, unicode.Co, unicode.Zl, unicode.Zp, unicode.Zs):
		return false
	case unicode.In(r, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector):
		// Default-ignorable characters, such as the Hangul fillers, are not
		// printable either.
		return false
	}
	// An unassigned code point (Cn) belongs to no category.
	return unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S)
}

// DebugString quotes s as Rust's Debug formatting of a str does: '"' and
// '\\' are escaped, \0 \t \r \n use their short forms, a grapheme-extending
// or unprintable character (as char::escape_debug judges them: control,
// format, surrogate, private-use, separator other than space, or unassigned)
// uses \u{..}, and everything else is written as is.
func DebugString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == 0:
			b.WriteString(`\0`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\n':
			b.WriteString(`\n`)
		case graphemeExtend(r) || !printable(r):
			fmt.Fprintf(&b, `\u{%x}`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
