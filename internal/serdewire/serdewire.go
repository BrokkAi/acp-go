// Package serdewire writes JSON exactly as the reference Rust SDK's
// serde_json (built with preserve_order) writes the value it holds: the
// encoding of serdejson.Encode(serdejson.ParseAnyDepth(b)), computed without
// building a value for the JSON that is already, or nearly, in that form.
// It is the frame writer of the stdio connection and the client router.
package serdewire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/bits"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/BrokkAi/acp-go/internal/jsonline"
	"github.com/BrokkAi/acp-go/internal/serdejson"
)

// ErrUnrepresentable reports JSON the reference SDK cannot hold, and so
// never writes: invalid UTF-8, a lone surrogate escape, or a number that
// overflows f64. A Rust peer fails the transport or answers such a frame
// with -32700, so it is never written.
var ErrUnrepresentable = errors.New("acp: JSON holds invalid UTF-8, a lone surrogate escape, or a number out of range")

// Marshal encodes v with encoding/json and writes the result as serde_json
// writes it.
func Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Encode(b)
}

// Encode writes b, compact JSON such as encoding/json produces, as
// serde_json writes the value it holds: <, > and & are not escaped, U+2028
// and U+2029 and every other character serde_json does not escape are
// written raw, other escapes such as \u0061 or \/ are decoded, numbers are
// formatted as serde_json formats them, and a repeated object member keeps
// the position of its first occurrence with the value of its last. Object
// members keep their order.
//
// One scan classifies b without allocating. JSON already in serde_json's
// form, the usual case, is returned as it is. JSON whose only differences
// are in string escapes or number forms is rewritten token by token.
// Anything else, such as a repeated member, is parsed and re-encoded. It
// fails with ErrUnrepresentable for JSON the reference SDK cannot hold.
//
// b must be compact (as json.Marshal and json.Compact write it) and valid
// JSON apart from what ErrUnrepresentable reports.
func Encode(b []byte) ([]byte, error) { return encode(b, false) }

// MarshalTyped encodes v with encoding/json and writes the result as
// EncodeTyped does.
func MarshalTyped(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return EncodeTyped(b)
}

// EncodeTyped is Encode for the encoding of a Go value, such as json.Marshal
// output, as opposed to JSON text read from a peer. The reference SDK writes
// a Rust value with serde_json::to_value and to_string, which never parse an
// f64 it holds, so a number with a fraction or an exponent is read here as
// the float64 nearest to it: for the shortest round-trip digits encoding/json
// and the generated encoders write, that is exactly the float64 they
// encoded, and a number already in serde_json's form is written as it is.
// Encode instead reads every such number as serde_json reads wire text,
// which does not always round to the nearest float64. Raw JSON inside the
// value is read the same way; the generated encoders have already
// re-encoded the raw JSON the Rust types hold as serde_json reads it.
func EncodeTyped(b []byte) ([]byte, error) { return encode(b, true) }

func encode(b []byte, typed bool) ([]byte, error) {
	switch classify(b) {
	case serdeExact:
		return b, nil
	case serdeTokens:
		if out, ok := rewriteTokens(b, typed); ok {
			return out, nil
		}
	}
	parse := serdejson.ParseAnyDepth
	if typed {
		parse = serdejson.ParseTyped
	}
	value, err := parse(b)
	if err != nil {
		return nil, ErrUnrepresentable
	}
	return serdejson.Encode(value), nil
}

// AppendString appends s as a JSON string as serde_json writes it. Invalid
// UTF-8 is written as U+FFFD, as encoding/json writes it and Rust's strings
// hold it.
func AppendString(out []byte, s string) []byte {
	out = append(out, '"')
	for _, r := range s {
		out = appendSerdeRune(out, r)
	}
	return append(out, '"')
}

// form is how far marshaled JSON is from what serde_json writes for it.
type form int

const (
	// serdeExact is already exactly what serde_json writes.
	serdeExact form = iota
	// serdeTokens differs only inside strings that are not object keys and
	// in number forms, so rewriting those tokens one by one is exact.
	serdeTokens
	// serdeReparse needs the general path: invalid UTF-8, an object key
	// that is not in serde_json's form, or a repeated member.
	serdeReparse
)

// maxScannedMembers is how many keys of an object the repeated-member check
// compares one by one; past that, the object's keys go in a set.
const maxScannedMembers = 16

// maxPlainDigits is the longest integer that fits i64 and u64 whatever its
// digits, so serde_json keeps it as written.
const maxPlainDigits = 18

// inStringPlain marks the bytes a string scan skips: everything but the
// closing quote and the backslash that starts an escape.
var inStringPlain = func() (table [256]bool) {
	for i := range table {
		table[i] = i != '"' && i != '\\'
	}
	return table
}()

// classify scans b, the compact output of json.Marshal, once. A string is
// in serde_json's form when each escape is one serde_json writes itself
// (\" \\ \b \f \n \r \t, and \u00xx in lowercase hex for any other control
// character). A number is when it is an integer of at most maxPlainDigits
// digits other than -0. Keys in serde_json's form are equal exactly when
// their bytes are, as each string has one such spelling.
func classify(b []byte) form {
	if !utf8.Valid(b) {
		return serdeReparse
	}
	result := serdeExact
	// keys holds a fingerprint of each key of the open objects; open holds
	// each open object or array, with where its keys begin in keys (-1 for
	// an array, so a key is only compared within its own object) and, once
	// an object has more than maxScannedMembers keys, the set they move to.
	// Equal keys have equal fingerprints; unequal keys that share one are
	// re-parsed, which is still exact.
	type container struct {
		start int
		set   *keySet
	}
	var keyBuf [32]uint64
	var openBuf [8]container
	keys := keyBuf[:0]
	open := openBuf[:0]
	for i := 0; i < len(b); i++ {
		switch c := b[i]; c {
		case '{':
			open = append(open, container{start: len(keys)})
		case '[':
			open = append(open, container{start: -1})
		case '}', ']':
			if n := len(open); n > 0 {
				if start := open[n-1].start; start >= 0 {
					keys = keys[:start]
				}
				open = open[:n-1]
			}
		case '"':
			end, exact := scanString(b, i)
			if end < 0 {
				return serdeReparse
			}
			if end+1 < len(b) && b[end+1] == ':' && len(open) > 0 && open[len(open)-1].start >= 0 {
				if !exact {
					return serdeReparse
				}
				object := &open[len(open)-1]
				key := fingerprint(b[i+1 : end])
				if object.set != nil {
					if !object.set.add(key) {
						return serdeReparse
					}
				} else {
					for _, earlier := range keys[object.start:] {
						if earlier == key {
							return serdeReparse
						}
					}
					keys = append(keys, key)
					if len(keys)-object.start > maxScannedMembers {
						object.set = newKeySet(keys[object.start:])
						keys = keys[:object.start]
					}
				}
			} else if !exact {
				result = serdeTokens
			}
			i = end
		case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			end, plain := scanNumber(b, i)
			if !plain {
				result = serdeTokens
			}
			i = end - 1
		}
	}
	return result
}

// keySet is an open-addressing set of key fingerprints for an object with
// many members. Zero marks an empty slot, so a zero fingerprint is stored as
// one; merging the two only makes a repeated member look likelier, which is
// still exact.
type keySet struct {
	slots []uint64
	count int
}

func newKeySet(keys []uint64) *keySet {
	set := &keySet{slots: make([]uint64, 8*maxScannedMembers)}
	for _, key := range keys {
		set.add(key)
	}
	return set
}

// add inserts key and reports false when it was already there.
func (s *keySet) add(key uint64) bool {
	if key == 0 {
		key = 1
	}
	if 2*(s.count+1) > len(s.slots) {
		old := s.slots
		s.slots = make([]uint64, 2*len(old))
		s.count = 0
		for _, earlier := range old {
			if earlier != 0 {
				s.add(earlier)
			}
		}
	}
	// Fibonacci hashing: the top bits of the product depend on every bit of
	// the fingerprint, whose low bits alone may barely vary between keys.
	mask := uint64(len(s.slots) - 1)
	shift := uint(64 - bits.Len64(mask))
	for i := (key * 0x9E3779B97F4A7C15) >> shift; ; i = (i + 1) & mask {
		switch s.slots[i] {
		case 0:
			s.slots[i] = key
			s.count++
			return true
		case key:
			return false
		}
	}
}

// hasByte is nonzero when any byte of v is c (the SWAR zero-byte test).
func hasByte(v uint64, c byte) uint64 {
	const ones, highs = 0x0101010101010101, 0x8080808080808080
	x := v ^ (ones * uint64(c))
	return (x - ones) &^ x & highs
}

// fingerprint hashes a key's bytes eight at a time, a function of its bytes
// cheap enough to compute for every key.
func fingerprint(key []byte) uint64 {
	const prime = 0x9E3779B97F4A7C15
	h := uint64(len(key)) * prime
	for len(key) >= 8 {
		h = (h ^ binary.LittleEndian.Uint64(key)) * prime
		h ^= h >> 29
		key = key[8:]
	}
	var last uint64
	for i, c := range key {
		last |= uint64(c) << (8 * i)
	}
	h = (h ^ last) * prime
	return h ^ h>>32
}

// scanString returns the index of the quote closing the string that starts
// at b[start], or -1, and whether each escape in it is one serde_json writes.
func scanString(b []byte, start int) (int, bool) {
	exact := true
	for i := start + 1; i < len(b); i++ {
		// Skip eight bytes at a time while none is a quote or a backslash.
		for i+8 <= len(b) {
			v := binary.LittleEndian.Uint64(b[i:])
			if hasByte(v, '"')|hasByte(v, '\\') != 0 {
				break
			}
			i += 8
		}
		for i < len(b) && inStringPlain[b[i]] {
			i++
		}
		if i >= len(b) {
			break
		}
		if b[i] == '"' {
			return i, exact
		}
		// A backslash: valid JSON always has the escape's next byte.
		if i+1 >= len(b) {
			return -1, false
		}
		switch b[i+1] {
		case '"', '\\', 'b', 'f', 'n', 'r', 't':
			i++
		case 'u':
			if i+5 >= len(b) {
				return -1, false
			}
			if !serdeUnicodeEscape(b[i+2 : i+6]) {
				exact = false
			}
			i += 5
		default:
			exact = false
			i++
		}
	}
	return -1, false
}

// scanNumber returns the end of the number that starts at b[start], and
// whether serde_json writes it as it is: an integer of at most
// maxPlainDigits digits other than -0, or a decimal plainDecimal accepts.
func scanNumber(b []byte, start int) (int, bool) {
	i := start
	if b[i] == '-' {
		i++
	}
	digits := i
	for i < len(b) && b[i] >= '0' && b[i] <= '9' {
		i++
	}
	integer := i
	if i == len(b) || (b[i] != '.' && b[i] != 'e' && b[i] != 'E') {
		return i, i-digits > 0 && i-digits <= maxPlainDigits && !(b[start] == '-' && i-digits == 1 && b[digits] == '0')
	}
	plain := b[i] == '.'
	if plain {
		i++
		fraction := i
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			i++
		}
		plain = plainDecimal(b[digits:integer], b[fraction:i])
	}
	for i < len(b) && (b[i] == 'e' || b[i] == 'E' || b[i] == '+' || b[i] == '-' || (b[i] >= '0' && b[i] <= '9')) {
		plain = false
		i++
	}
	return i, plain
}

// plainDecimal reports whether serde_json writes the decimal integer.fraction
// (no exponent) exactly as it is: its significant digits number at most 15,
// it has no trailing zero, and its point falls where serde_json writes a
// plain decimal (16 or fewer integer digits, or fewer than five zeros after
// "0."). Such a decimal is the shortest round-trip form of the f64 it denotes,
// as no two decimals of at most 15 significant digits denote the same f64,
// and serde_json parses it exactly, as its significand is below 2^53 and its
// power of ten at most 10^15, so it writes the same digits back.
func plainDecimal(integer, fraction []byte) bool {
	if len(fraction) == 0 || fraction[len(fraction)-1] == '0' {
		return false
	}
	if len(integer) == 1 && integer[0] == '0' {
		zeros := 0
		for zeros < len(fraction) && fraction[zeros] == '0' {
			zeros++
		}
		return zeros < 5 && len(fraction)-zeros <= 15
	}
	return len(integer)+len(fraction) <= 15
}

// serdeUnicodeEscape reports whether hex, the four digits after \u, is an
// escape serde_json writes: 00 and two lowercase hex digits naming a control
// character that has no short form.
func serdeUnicodeEscape(hex []byte) bool {
	if hex[0] != '0' || hex[1] != '0' || (hex[2] != '0' && hex[2] != '1') {
		return false
	}
	var low byte
	switch d := hex[3]; {
	case d >= '0' && d <= '9':
		low = d - '0'
	case d >= 'a' && d <= 'f':
		low = d - 'a' + 10
	default:
		return false
	}
	switch (hex[2]-'0')<<4 | low {
	case '\b', '\t', '\n', '\f', '\r':
		return false
	}
	return true
}

// rewriteTokens writes b, classified serdeTokens, with each string and
// number as serde_json writes it, a number read as EncodeTyped reads one when
// typed is set. It reports false for what only the general path decides,
// such as a lone surrogate escape or a number that overflows.
func rewriteTokens(b []byte, typed bool) ([]byte, bool) {
	out := make([]byte, 0, len(b)+len(b)/8)
	for i := 0; i < len(b); i++ {
		switch c := b[i]; c {
		case '"':
			end, exact := scanString(b, i)
			if exact {
				out = append(out, b[i:end+1]...)
			} else {
				var ok bool
				if out, ok = appendSerdeString(out, b[i+1:end]); !ok {
					return nil, false
				}
			}
			i = end
		case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			end, plain := scanNumber(b, i)
			if plain {
				out = append(out, b[i:end]...)
			} else {
				var ok bool
				if out, ok = appendSerdeNumber(out, b[i:end], typed); !ok {
					return nil, false
				}
			}
			i = end - 1
		default:
			out = append(out, c)
		}
	}
	return out, true
}

// appendSerdeString appends the JSON string whose contents, without quotes,
// are s, as serde_json writes the text it holds: runs without escapes are
// copied, and each escape is decoded and written in serde_json's form. It
// reports false for a lone surrogate escape, which serde_json rejects.
func appendSerdeString(out, s []byte) ([]byte, bool) {
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		run := i
		for i < len(s) && s[i] != '\\' {
			i++
		}
		out = append(out, s[run:i]...)
		if i >= len(s) {
			break
		}
		i++
		var r rune
		switch s[i] {
		case '"', '\\', '/':
			r = rune(s[i])
		case 'b':
			r = '\b'
		case 'f':
			r = '\f'
		case 'n':
			r = '\n'
		case 'r':
			r = '\r'
		case 't':
			r = '\t'
		case 'u':
			var ok bool
			if r, ok = hex4(s[i+1:]); !ok {
				return nil, false
			}
			i += 4
			if utf16.IsSurrogate(r) {
				if r >= 0xDC00 || len(s) < i+7 || s[i+1] != '\\' || s[i+2] != 'u' {
					return nil, false
				}
				low, ok := hex4(s[i+3:])
				if !ok || low < 0xDC00 || low > 0xDFFF {
					return nil, false
				}
				r = utf16.DecodeRune(r, low)
				i += 6
			}
		default:
			return nil, false
		}
		out = appendSerdeRune(out, r)
	}
	return append(out, '"'), true
}

// appendSerdeRune appends r inside a string as serde_json writes it, the
// escaping jsonline.Quote implements: \" and \\, the short escapes, \u00xx
// in lowercase hex for any other control character, and the rest raw.
func appendSerdeRune(out []byte, r rune) []byte {
	const hex = "0123456789abcdef"
	switch r {
	case '"', '\\':
		return append(out, '\\', byte(r))
	case '\b':
		return append(out, '\\', 'b')
	case '\t':
		return append(out, '\\', 't')
	case '\n':
		return append(out, '\\', 'n')
	case '\f':
		return append(out, '\\', 'f')
	case '\r':
		return append(out, '\\', 'r')
	}
	if r < 0x20 {
		return append(out, '\\', 'u', '0', '0', hex[r>>4], hex[r&0xF])
	}
	return utf8.AppendRune(out, r)
}

func hex4(s []byte) (rune, bool) {
	if len(s) < 4 {
		return 0, false
	}
	value, err := strconv.ParseUint(string(s[:4]), 16, 32)
	if err != nil {
		return 0, false
	}
	return rune(value), true
}

// appendSerdeNumber appends the JSON number token as serde_json writes the
// number it parses: an integer that fits u64 or i64 as written, anything
// else as the f64 serde_json evaluates it to, or with typed set as the
// nearest f64. It reports false for a number that overflows f64, which
// serde_json rejects.
func appendSerdeNumber(out, token []byte, typed bool) ([]byte, bool) {
	if !hasFraction(token) && string(token) != "-0" {
		if _, err := strconv.ParseUint(string(token), 10, 64); err == nil {
			return append(out, token...), true
		}
		if _, err := strconv.ParseInt(string(token), 10, 64); err == nil {
			return append(out, token...), true
		}
	}
	if typed {
		f, err := strconv.ParseFloat(string(token), 64)
		if err != nil {
			return nil, false
		}
		return serdejson.AppendFloat(out, f), true
	}
	f, ok := jsonline.ParseFloat(token)
	if !ok {
		return nil, false
	}
	return serdejson.AppendFloat(out, f), true
}

func hasFraction(token []byte) bool {
	for _, c := range token {
		if c == '.' || c == 'e' || c == 'E' {
			return true
		}
	}
	return false
}
