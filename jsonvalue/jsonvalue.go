// Package jsonvalue holds untyped JSON exactly as the reference Rust SDK's
// serde_json::Value does, built as the SDK builds it: with preserve_order,
// without arbitrary_precision or float_roundtrip.
//
// A decoded value is one of these Go values:
//
//   - nil for null
//   - bool
//   - string, with its escapes decoded
//   - int64 for an integer that fits i64, uint64 for a larger one that fits
//     u64 (Rust's PosInt and NegInt), never rounded through a float64
//   - float64 for any other number, evaluated as serde_json evaluates it
//   - []any for an array
//   - Object for an object: its members in wire order, a repeated key once at
//     its first position with its last value
//
// Encoding writes a value as serde_json::to_string writes the Value: compact,
// members in order, strings with only serde_json's escapes, integers as they
// are, and a float64 in serde_json's form, always with a decimal point or an
// exponent (1.0, -0.0, 1e+20). Decoding then encoding is exactly serde_json's
// parse and to_string.
//
// The generated schema packages hold _meta and the other objects Rust holds
// as a serde_json::Map as an Object (schema.Meta and v2.Meta are Object).
package jsonvalue

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/BrokkAi/acp-go/internal/jsonline"
	"github.com/BrokkAi/acp-go/internal/serdejson"
	"github.com/BrokkAi/acp-go/internal/serdewire"
)

// Object is a JSON object as serde_json::Map holds it with preserve_order:
// members in order. nil is no object (an absent _meta) and encodes as null;
// Object{} is the empty object.
//
// An Object built in Go may repeat a key; it then means what the same
// members mean on the wire: the key at its first position with its last
// value, as Get reads it and encoding writes it. Decoding never repeats a
// key.
//
// An Object is a slice, so copies share their members: Set, Delete, and
// SwapDelete may write into the backing array another copy still uses, and
// two copies that append may overwrite each other's new members. Clone an
// Object before changing one that is shared, such as one decoded into a
// message other code still reads. An Object is not safe for concurrent
// mutation, and not for mutation while another goroutine reads it.
type Object []Member

// Member is one member of an Object.
type Member struct {
	Key   string
	Value any
}

// ObjectOf returns the Object holding the members of values with their keys
// in sorted order, so the same map always encodes the same way. A nil map
// returns nil, and an empty one Object{}. The values are kept as they are.
func ObjectOf(values map[string]any) Object {
	if values == nil {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	o := make(Object, len(keys))
	for i, key := range keys {
		o[i] = Member{Key: key, Value: values[key]}
	}
	return o
}

// Get returns the value of the member named key, the last one if the key
// repeats.
func (o Object) Get(key string) (any, bool) {
	for i := len(o) - 1; i >= 0; i-- {
		if o[i].Key == key {
			return o[i].Value, true
		}
	}
	return nil, false
}

// Set sets the member named key as serde_json's Map::insert does: an existing
// member keeps its position and takes value, and a new one is appended. A
// repeated key is left once, at its first position.
func (o *Object) Set(key string, value any) {
	for i, member := range *o {
		if member.Key == key {
			(*o)[i].Value = value
			if tail := (*o)[i+1:]; slices.ContainsFunc(tail, func(m Member) bool { return m.Key == key }) {
				*o = (*o)[:i+1+len(slices.DeleteFunc(tail, func(m Member) bool { return m.Key == key }))]
			}
			return
		}
	}
	if *o == nil {
		*o = Object{}
	}
	*o = append(*o, Member{Key: key, Value: value})
}

// Delete removes the member named key and keeps the order of the others, as
// serde_json's Map::shift_remove does. It reports whether key was present.
func (o *Object) Delete(key string) bool {
	n := len(*o)
	*o = slices.DeleteFunc(*o, func(m Member) bool { return m.Key == key })
	return len(*o) != n
}

// SwapDelete removes the member named key as serde_json's Map::remove does
// with preserve_order (swap_remove): the last member moves into its place.
// It reports whether key was present. It is how Rust's apply_meta_update
// removes a key.
func (o *Object) SwapDelete(key string) bool {
	if o.hasRepeats() {
		*o = o.merged()
	}
	for i, member := range *o {
		if member.Key == key {
			last := len(*o) - 1
			(*o)[i] = (*o)[last]
			(*o)[last] = Member{}
			*o = (*o)[:last]
			return true
		}
	}
	return false
}

// Keys returns the keys in order, each once.
func (o Object) Keys() []string {
	if o.hasRepeats() {
		o = o.merged()
	}
	keys := make([]string, len(o))
	for i, member := range o {
		keys[i] = member.Key
	}
	return keys
}

// Clone returns a deep copy of o, as Rust's Map::clone is: nested Objects and
// []any arrays are copied too, and other values (strings, numbers, and Go
// values set in code) are shared. nil returns nil.
func (o Object) Clone() Object {
	if o == nil {
		return nil
	}
	out := make(Object, len(o))
	for i, member := range o {
		out[i] = Member{Key: member.Key, Value: cloneValue(member.Value)}
	}
	return out
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case Object:
		return x.Clone()
	case []any:
		if x == nil {
			return x
		}
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneValue(item)
		}
		return out
	}
	return v
}

// scannedMembers is how many members hasRepeats compares by key hash
// without allocating; a larger object's keys go in a map.
const scannedMembers = 256

// hasRepeats reports whether a key occurs more than once.
func (o Object) hasRepeats() bool {
	if len(o) <= 16 {
		for i := 1; i < len(o); i++ {
			for j := 0; j < i; j++ {
				if o[i].Key == o[j].Key {
					return true
				}
			}
		}
		return false
	}
	if len(o) <= scannedMembers {
		// Keys with equal hashes are compared; the hashes live on the stack.
		var hashes [scannedMembers]uint64
		for i, member := range o {
			h := keyHash(member.Key)
			for j := 0; j < i; j++ {
				if hashes[j] == h && o[j].Key == member.Key {
					return true
				}
			}
			hashes[i] = h
		}
		return false
	}
	seen := make(map[string]struct{}, len(o))
	for _, member := range o {
		if _, ok := seen[member.Key]; ok {
			return true
		}
		seen[member.Key] = struct{}{}
	}
	return false
}

// keyHash is FNV-1a of key.
func keyHash(key string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(key); i++ {
		h ^= uint64(key[i])
		h *= 1099511628211
	}
	return h
}

// merged returns the members as the wire reads them: each key once, at its
// first position, with its last value.
func (o Object) merged() Object {
	index := make(map[string]int, len(o))
	out := make(Object, 0, len(o))
	for _, member := range o {
		if i, ok := index[member.Key]; ok {
			out[i].Value = member.Value
			continue
		}
		index[member.Key] = len(out)
		out = append(out, member)
	}
	return out
}

// Equal reports whether o and other hold the same members in the same order,
// each key at its first position with its last value, and with Equal values.
// A nil Object, which encodes as null, equals only another nil one. Unlike
// Rust's Map equality, which ignores order, the order matters here, as it
// does on the wire.
func (o Object) Equal(other Object) bool {
	if (o == nil) != (other == nil) {
		return false
	}
	if o.hasRepeats() {
		o = o.merged()
	}
	if other.hasRepeats() {
		other = other.merged()
	}
	if len(o) != len(other) {
		return false
	}
	for i := range o {
		if o[i].Key != other[i].Key || !Equal(o[i].Value, other[i].Value) {
			return false
		}
	}
	return true
}

// Equal reports whether a and b are the same JSON value, as serde_json
// compares Values except that object members compare in order (see
// Object.Equal). Numbers compare by their class, as serde_json's Number
// does: an integer equals an integer of the same value whatever its Go type
// (int, int64, uint64, ...), a float (float32 or float64) equals a float of
// the same value (so -0.0 equals 0.0, and NaN nothing), and an integer never
// equals a float: 1 is not 1.0. A json.RawMessage or json.Number compares as
// the value serde_json reads from it, a map[string]any as its keys sorted,
// and any other Go value as the value its encoding holds.
func Equal(a, b any) bool {
	a, b = normalize(a), normalize(b)
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case int64:
		y, ok := b.(int64)
		return ok && x == y
	case uint64:
		y, ok := b.(uint64)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case Object:
		y, ok := b.(Object)
		return ok && x != nil && y != nil && x.Equal(y)
	}
	return false
}

// normalize returns v as one of the classes Equal compares: nil, bool,
// string, int64 for a negative integer, uint64 for a non-negative one,
// float64, []any, or a non-nil Object.
func normalize(v any) any {
	switch x := v.(type) {
	case nil, bool, string, float64:
		return v
	case int64:
		if x >= 0 {
			return uint64(x)
		}
		return x
	case int:
		return normalize(int64(x))
	case int8:
		return normalize(int64(x))
	case int16:
		return normalize(int64(x))
	case int32:
		return normalize(int64(x))
	case uint64:
		return x
	case uint:
		return uint64(x)
	case uint8:
		return uint64(x)
	case uint16:
		return uint64(x)
	case uint32:
		return uint64(x)
	case float32:
		return float64(x)
	case Object:
		if x == nil {
			return nil
		}
		return x
	case []any:
		if x == nil {
			return nil
		}
		return x
	case map[string]any:
		if x == nil {
			return nil
		}
		return ObjectOf(x)
	}
	encoded, err := Marshal(v)
	if err != nil {
		return encodingFailed{}
	}
	parsed, err := Parse(encoded)
	if err != nil {
		return encodingFailed{}
	}
	return normalize(parsed)
}

// encodingFailed stands for a value that does not encode, which equals
// nothing.
type encodingFailed struct{}

// MarshalJSON writes o as serde_json writes the Map. nil writes null.
func (o Object) MarshalJSON() ([]byte, error) {
	return o.AppendJSON(nil)
}

// AppendJSON appends o as MarshalJSON writes it.
func (o Object) AppendJSON(b []byte) ([]byte, error) {
	if o == nil {
		return append(b, "null"...), nil
	}
	if o.hasRepeats() {
		o = o.merged()
	}
	b = append(b, '{')
	var err error
	for i, member := range o {
		if i > 0 {
			b = append(b, ',')
		}
		b = serdewire.AppendString(b, member.Key)
		b = append(b, ':')
		if b, err = Append(b, member.Value); err != nil {
			return nil, err
		}
	}
	return append(b, '}'), nil
}

// UnmarshalJSON reads data as serde_json reads a Map from a Value: it must
// be an object, and anything else, null included, fails with serde's error,
// such as "invalid type: null, expected a map".
func (o *Object) UnmarshalJSON(data []byte) error {
	value, err := Parse(data)
	if err != nil {
		return err
	}
	object, ok := value.(Object)
	if !ok {
		return fmt.Errorf("invalid type: %s, expected a map", unexpected(value))
	}
	*o = object
	return nil
}

// unexpected describes v as serde's de::Unexpected does.
func unexpected(v any) string {
	switch v := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean `" + strconv.FormatBool(v) + "`"
	case int64:
		return "integer `" + strconv.FormatInt(v, 10) + "`"
	case uint64:
		return "integer `" + strconv.FormatUint(v, 10) + "`"
	case float64:
		return "floating point `" + serdejson.FormatFloat(v) + "`"
	case string:
		return "string " + serdejson.DebugString(v)
	case []any:
		return "sequence"
	}
	return "map"
}

// IsNull reports whether v encodes as JSON null: nil, a nil Object, slice or
// map, json.RawMessage("null"), or a value whose encoding is null. It is the
// removal signal of a metadata patch.
func IsNull(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool, string, float64, float32, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, json.Number:
		return false
	case Object:
		return x == nil
	case []any:
		return x == nil
	case map[string]any:
		return x == nil
	case json.RawMessage:
		return x == nil || string(bytes.TrimSpace(x)) == "null"
	}
	encoded, err := Marshal(v)
	return err == nil && string(encoded) == "null"
}

// Marshal writes v as serde_json::to_string writes the Value that holds it.
// See Append.
func Marshal(v any) ([]byte, error) {
	return Append(nil, v)
}

// Append appends v as serde_json writes the Value that holds it: the values
// Parse returns as serde_json writes them, any Go integer as an integer, a
// float32 or float64 as an f64 (a NaN or infinity as null, as
// serde_json::to_value holds it), a json.Number or json.RawMessage as
// serde_json reads and re-writes that JSON text, a map[string]any with its
// keys sorted, and any other value as encoding/json encodes it, with its
// strings and numbers in serde_json's form (encoding/json writes an integral
// float64 inside such a value, such as a struct field, without a decimal
// point, so it is then written as an integer).
func Append(b []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(b, "null"...), nil
	case Object:
		return x.AppendJSON(b)
	case string:
		return serdewire.AppendString(b, x), nil
	case bool:
		return strconv.AppendBool(b, x), nil
	case int64:
		return strconv.AppendInt(b, x, 10), nil
	case uint64:
		return strconv.AppendUint(b, x, 10), nil
	case float64:
		return serdejson.AppendFloat(b, x), nil
	case []any:
		if x == nil {
			return append(b, "null"...), nil
		}
		b = append(b, '[')
		var err error
		for i, item := range x {
			if i > 0 {
				b = append(b, ',')
			}
			if b, err = Append(b, item); err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case map[string]any:
		if x == nil {
			return append(b, "null"...), nil
		}
		return ObjectOf(x).AppendJSON(b)
	case int:
		return strconv.AppendInt(b, int64(x), 10), nil
	case int8:
		return strconv.AppendInt(b, int64(x), 10), nil
	case int16:
		return strconv.AppendInt(b, int64(x), 10), nil
	case int32:
		return strconv.AppendInt(b, int64(x), 10), nil
	case uint:
		return strconv.AppendUint(b, uint64(x), 10), nil
	case uint8:
		return strconv.AppendUint(b, uint64(x), 10), nil
	case uint16:
		return strconv.AppendUint(b, uint64(x), 10), nil
	case uint32:
		return strconv.AppendUint(b, uint64(x), 10), nil
	case float32:
		return serdejson.AppendFloat(b, float64(x)), nil
	case json.Number:
		return appendText(b, []byte(x))
	case json.RawMessage:
		if x == nil {
			return append(b, "null"...), nil
		}
		return appendText(b, x)
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if encoded, err = serdewire.EncodeTyped(encoded); err != nil {
		return nil, err
	}
	return append(b, encoded...), nil
}

// appendText appends JSON text as serde_json re-writes the Value it reads.
func appendText(b, text []byte) ([]byte, error) {
	value, err := Parse(text)
	if err != nil {
		return nil, fmt.Errorf("jsonvalue: invalid JSON %q: %w", text, err)
	}
	return Append(b, value)
}

// ErrInvalid reports input serde_json does not accept as one JSON value: not
// JSON, invalid UTF-8, a lone surrogate escape, or a number that overflows
// f64.
var ErrInvalid = errors.New("jsonvalue: not a JSON value serde_json accepts")

// ErrTooDeep reports input nested deeper than MaxDepth.
var ErrTooDeep = errors.New("jsonvalue: JSON nested too deeply")

// MaxDepth is how deeply Parse nests arrays and objects, as encoding/json
// limits it, so a hostile input cannot exhaust the stack. serde_json's own
// limit is 128; acp-go applies it where it reads a frame, so a received
// message never reaches Parse nested deeper than that.
const MaxDepth = 10000

// Parse reads data, one JSON value with optional surrounding whitespace, as
// serde_json reads a Value, nested at most MaxDepth deep. See the package
// documentation for the Go values it returns.
func Parse(data []byte) (any, error) {
	p := parser{data: data}
	p.space()
	value, ok := p.value()
	if p.tooDeep {
		return nil, ErrTooDeep
	}
	if !ok {
		return nil, ErrInvalid
	}
	p.space()
	if p.i != len(p.data) {
		return nil, ErrInvalid
	}
	return value, nil
}

// parser reads one JSON value in a single pass, checking it as serde_json
// does.
type parser struct {
	data    []byte
	i       int
	depth   int
	tooDeep bool
}

func (p *parser) space() {
	for p.i < len(p.data) {
		switch p.data[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) literal(word string) bool {
	if !bytes.HasPrefix(p.data[p.i:], []byte(word)) {
		return false
	}
	p.i += len(word)
	return true
}

func (p *parser) value() (any, bool) {
	if p.i >= len(p.data) {
		return nil, false
	}
	switch c := p.data[p.i]; {
	case c == '{' || c == '[':
		if p.depth++; p.depth > MaxDepth {
			p.tooDeep = true
			return nil, false
		}
		defer func() { p.depth-- }()
		if c == '{' {
			return p.object()
		}
		return p.array()
	case c == '"':
		s, ok := p.string()
		return s, ok
	case c == 't':
		return true, p.literal("true")
	case c == 'f':
		return false, p.literal("false")
	case c == 'n':
		return nil, p.literal("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, false
}

func (p *parser) object() (any, bool) {
	p.i++ // {
	o := Object{}
	// Repeated keys are found by scanning the members, or once the object
	// is large through an index, so parsing stays linear in its size.
	var index map[string]int
	p.space()
	if p.i < len(p.data) && p.data[p.i] == '}' {
		p.i++
		return o, true
	}
	for {
		p.space()
		if p.i >= len(p.data) || p.data[p.i] != '"' {
			return nil, false
		}
		key, ok := p.string()
		if !ok {
			return nil, false
		}
		p.space()
		if p.i >= len(p.data) || p.data[p.i] != ':' {
			return nil, false
		}
		p.i++
		p.space()
		value, ok := p.value()
		if !ok {
			return nil, false
		}
		at := -1
		if index != nil {
			if i, ok := index[key]; ok {
				at = i
			}
		} else {
			for i := range o {
				if o[i].Key == key {
					at = i
					break
				}
			}
		}
		if at >= 0 {
			o[at].Value = value
		} else {
			o = append(o, Member{Key: key, Value: value})
			switch {
			case index != nil:
				index[key] = len(o) - 1
			case len(o) > 16:
				index = make(map[string]int, 2*len(o))
				for i, member := range o {
					index[member.Key] = i
				}
			}
		}
		p.space()
		if p.i >= len(p.data) {
			return nil, false
		}
		switch p.data[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return o, true
		default:
			return nil, false
		}
	}
}

func (p *parser) array() (any, bool) {
	p.i++ // [
	items := []any{}
	p.space()
	if p.i < len(p.data) && p.data[p.i] == ']' {
		p.i++
		return items, true
	}
	for {
		p.space()
		item, ok := p.value()
		if !ok {
			return nil, false
		}
		items = append(items, item)
		p.space()
		if p.i >= len(p.data) {
			return nil, false
		}
		switch p.data[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return items, true
		default:
			return nil, false
		}
	}
}

// string reads a string token at p.i, rejecting control characters, invalid
// UTF-8, and lone surrogate escapes, as serde_json does.
func (p *parser) string() (string, bool) {
	start := p.i + 1
	i := start
	ascii := true
	for i < len(p.data) {
		c := p.data[i]
		switch {
		case c == '"':
			raw := p.data[start:i]
			if !ascii && !utf8.Valid(raw) {
				return "", false
			}
			p.i = i + 1
			return string(raw), true
		case c == '\\':
			return p.escapedString(start)
		case c < 0x20:
			return "", false
		case c >= 0x80:
			ascii = false
		}
		i++
	}
	return "", false
}

// escapedString reads a string token that starts at start and holds escapes.
func (p *parser) escapedString(start int) (string, bool) {
	out := make([]byte, 0, 2*(len(p.data)-start)/3+8)
	i := start
	for i < len(p.data) {
		c := p.data[i]
		switch {
		case c == '"':
			if !utf8.Valid(out) {
				return "", false
			}
			p.i = i + 1
			return string(out), true
		case c < 0x20:
			return "", false
		case c != '\\':
			out = append(out, c)
			i++
			continue
		}
		if i+1 >= len(p.data) {
			return "", false
		}
		switch p.data[i+1] {
		case '"', '\\', '/':
			out = append(out, p.data[i+1])
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, ok := hex4(p.data, i+2)
			if !ok {
				return "", false
			}
			i += 6
			if utf16.IsSurrogate(r) {
				if r >= 0xDC00 || i+6 > len(p.data) || p.data[i] != '\\' || p.data[i+1] != 'u' {
					return "", false
				}
				low, ok := hex4(p.data, i+2)
				if !ok || low < 0xDC00 || low > 0xDFFF {
					return "", false
				}
				r = utf16.DecodeRune(r, low)
				i += 6
			}
			out = utf8.AppendRune(out, r)
			continue
		default:
			return "", false
		}
		i += 2
	}
	return "", false
}

func hex4(data []byte, at int) (rune, bool) {
	if at+4 > len(data) {
		return 0, false
	}
	var r rune
	for _, c := range data[at : at+4] {
		switch {
		case c >= '0' && c <= '9':
			r = r<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	return r, true
}

// number reads a number token at p.i: an integer that fits i64 or u64 as an
// integer, anything else as serde_json evaluates the f64.
func (p *parser) number() (any, bool) {
	start := p.i
	i := p.i
	if p.data[i] == '-' {
		i++
	}
	switch {
	case i < len(p.data) && p.data[i] == '0':
		i++
	case i < len(p.data) && p.data[i] >= '1' && p.data[i] <= '9':
		for i < len(p.data) && p.data[i] >= '0' && p.data[i] <= '9' {
			i++
		}
	default:
		return nil, false
	}
	integer := true
	if i < len(p.data) && p.data[i] == '.' {
		integer = false
		i++
		digits := i
		for i < len(p.data) && p.data[i] >= '0' && p.data[i] <= '9' {
			i++
		}
		if i == digits {
			return nil, false
		}
	}
	if i < len(p.data) && (p.data[i] == 'e' || p.data[i] == 'E') {
		integer = false
		i++
		if i < len(p.data) && (p.data[i] == '+' || p.data[i] == '-') {
			i++
		}
		digits := i
		for i < len(p.data) && p.data[i] >= '0' && p.data[i] <= '9' {
			i++
		}
		if i == digits {
			return nil, false
		}
	}
	p.i = i
	token := p.data[start:i]
	if integer && string(token) != "-0" {
		// "-0" is the f64 -0.0 in serde_json.
		if token[0] == '-' {
			if n, ok := parseNegative(token[1:]); ok {
				return n, true
			}
		} else if n, ok := parseUnsigned(token); ok {
			if n <= math.MaxInt64 {
				return int64(n), true
			}
			return n, true
		}
	}
	f, ok := jsonline.ParseFloat(token)
	if !ok {
		return nil, false
	}
	return f, true
}

// parseUnsigned parses decimal digits that fit u64.
func parseUnsigned(digits []byte) (uint64, bool) {
	if len(digits) > 20 {
		return 0, false
	}
	var n uint64
	for _, c := range digits {
		d := uint64(c - '0')
		if n > (math.MaxUint64-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	return n, true
}

// parseNegative parses the digits of a negative integer that fits i64.
func parseNegative(digits []byte) (int64, bool) {
	n, ok := parseUnsigned(digits)
	if !ok || n > 1<<63 {
		return 0, false
	}
	return -int64(n), true
}
