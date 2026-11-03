package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The generated encoders write every value the way the Rust schema crate's
// serde derives do, as serde_json::to_string writes them (the reference SDK
// builds serde_json with preserve_order):
//
//   - Struct fields in Rust declaration order, which is the schema's
//     property order. A union the Rust struct flattens is written where the
//     struct declares it (flattenAfter), an internally tagged union's tag
//     first.
//   - An Option field that is None is left out (skip_serializing_none), and
//     so is a null in a field that is not a patch: Rust reads that null as
//     None. A patch field (Rust MaybeUndefined) writes an explicit null.
//   - A field that is not an Option but has a default (#[serde(default)]) is
//     always written, as its default when the Go field is unset. A list or
//     map that is not an Option is left out when empty (skip_serializing_if =
//     "Vec::is_empty"), unless it has such a default. A field Rust leaves out
//     when it holds its default (rustOmittedDefaults) is left out then.
//   - An f64 is written as serde_json formats it, with a decimal point or an
//     exponent (serdejson.FormatFloat): 1.0, -0.0, 1e+20, 1e-7.
//   - An open variant's preserved wire value, and any other raw JSON the
//     Rust type holds as a serde_json::Value, is re-encoded as serde_json
//     re-encodes a Value: members in order, a repeated key once at its first
//     position with its last value, numbers in serde_json's form. An open
//     variant writes its tag, then the fields Rust's Other type declares,
//     then its other members sorted by key (a flattened BTreeMap).
//
// Strings are escaped as serde_json escapes them; json.Marshal then escapes
// <, >, & and U+2028/U+2029 as it does for any MarshalJSON output. Meta, a
// jsonvalue.Object, and other untyped values are written as serde_json writes
// the serde_json::Value holding them (jsonvalue.Append): members in order,
// integers as they are, and a float64 as an f64.
//
// Every struct and union gets MarshalJSON and two unexported methods:
// appendJSON appends the encoded value, and appendMembers appends its members
// each with a leading comma, so a union can splice its payload's members
// after its tag without re-parsing them.

// fieldEncoding is how one field is written.
type fieldEncoding struct {
	f field
	// patch is set on a Nullable patch field, whose null is written.
	patch bool
	// always holds the compact default JSON of a field Rust always writes,
	// written when the Go field is unset.
	always string
	// omitted holds the Go constant expression of the value Rust leaves out.
	omitted string
}

// rawPatchFields are the raw JSON fields outside the patch tables that Rust
// holds as MaybeUndefined<Value>, keyed by def and wire key: an explicit null
// is kept and written, where an Option<Value> reads it as None.
var rawPatchFields = map[string]bool{
	"McpError.data": true,
}

func encodingOf(r *ir, def, owner string, f field) fieldEncoding {
	enc := fieldEncoding{f: f}
	if owner == def && (f.Nullable || isRawType(f.GoType)) {
		enc.patch = patchField(r, def, f.JSONName) || rawPatchFields[def+"."+f.JSONName]
	}
	if f.Default != "" && !f.NullOK && !f.Required {
		if !json.Valid([]byte(f.Default)) {
			panic(fmt.Sprintf("%s.%s: default %s is not JSON", owner, f.JSONName, f.Default))
		}
		enc.always = compactJSON(f.Default)
	}
	protocol := 1
	if r.PreserveNull {
		protocol = 2
	}
	if d, ok := rustOmittedDefaults[owner+"."+f.JSONName]; ok && d.protocol == protocol {
		var value string
		if err := json.Unmarshal([]byte(d.value), &value); err != nil {
			panic(fmt.Sprintf("%s.%s: omitted default %s is not a string", owner, f.JSONName, d.value))
		}
		enc.omitted = strconv.Quote(value)
	}
	return enc
}

// compactJSON compacts a JSON document, keeping its member order.
func compactJSON(doc string) string {
	var out strings.Builder
	inString, escaped := false, false
	for _, c := range doc {
		switch {
		case inString:
			out.WriteRune(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
			out.WriteRune(c)
		case c == ' ' || c == '\n' || c == '\t' || c == '\r':
		default:
			out.WriteRune(c)
		}
	}
	return out.String()
}

// keyLit renders the Go string literal of a member's leading comma and key.
func keyLit(key string) string {
	return strconv.Quote("," + string(quoteJSON(key)) + ":")
}

func quoteJSON(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

// encoder renders encode statements, numbering loop variables.
type encoder struct {
	b     *strings.Builder
	depth int
}

func (e *encoder) line(format string, args ...any) {
	fmt.Fprintf(e.b, format+"\n", args...)
}

// value appends statements that append expr, an addressable value of Go
// type typ, to b.
func (e *encoder) value(typ, expr string) {
	switch typ {
	case "string":
		e.line("b = appendString(b, %s)", expr)
		return
	case "bool":
		e.line("b = strconv.AppendBool(b, %s)", expr)
		return
	case "int8", "int16", "int32", "int64":
		e.line("b = strconv.AppendInt(b, int64(%s), 10)", expr)
		return
	case "uint8", "uint16", "uint32", "uint64":
		e.line("b = strconv.AppendUint(b, uint64(%s), 10)", expr)
		return
	case "float64":
		e.line("b = appendFloat(b, float64(%s))", expr)
		return
	case "Meta":
		e.line("if b, err = %s.AppendJSON(b); err != nil {\nreturn nil, err\n}", expr)
		return
	case "any", "map[string]any":
		e.line("if b, err = appendAny(b, %s); err != nil {\nreturn nil, err\n}", expr)
		return
	case "json.RawMessage":
		e.line("if b, err = appendRaw(b, %s); err != nil {\nreturn nil, err\n}", expr)
		return
	}
	if elem, ok := strings.CutPrefix(typ, "[]"); ok {
		i := fmt.Sprintf("i%d", e.depth)
		e.depth++
		e.line("b = append(b, '[')")
		e.line("for %s := range %s {", i, expr)
		e.line("if %s > 0 {\nb = append(b, ',')\n}", i)
		e.value(elem, expr+"["+i+"]")
		e.line("}")
		e.line("b = append(b, ']')")
		e.depth--
		return
	}
	if elem, ok := strings.CutPrefix(typ, "map[string]"); ok {
		k := fmt.Sprintf("k%d", e.depth)
		item := fmt.Sprintf("item%d", e.depth)
		e.depth++
		e.line("b = append(b, '{')")
		e.line("for n, %s := range sortedKeys(%s) {", k, expr)
		e.line("if n > 0 {\nb = append(b, ',')\n}")
		e.line("b = appendString(b, %s)\nb = append(b, ':')", k)
		e.line("%s := %s[%s]", item, expr, k)
		e.value(elem, item)
		e.line("}")
		e.line("b = append(b, '}')")
		e.depth--
		return
	}
	td := resolve(typ)
	if td == nil {
		panic(fmt.Sprintf("encode: unknown Go type %q", typ))
	}
	switch td.Kind {
	case kindEnum, kindNewtype:
		switch {
		case td.Base == "string":
			e.line("b = appendString(b, string(%s))", expr)
		case td.Base == "float64":
			e.line("b = appendFloat(b, float64(%s))", expr)
		case strings.HasPrefix(td.Base, "uint"):
			e.line("b = strconv.AppendUint(b, uint64(%s), 10)", expr)
		default:
			e.line("b = strconv.AppendInt(b, int64(%s), 10)", expr)
		}
	case kindAny:
		if len(td.AnyArms) > 0 {
			e.line("if b, err = append%sUnion(b, %s); err != nil {\nreturn nil, err\n}", td.Name, expr)
		} else {
			e.line("if b, err = appendAny(b, %s); err != nil {\nreturn nil, err\n}", expr)
		}
	case kindRaw:
		e.line("if b, err = appendRaw(b, json.RawMessage(%s)); err != nil {\nreturn nil, err\n}", expr)
	default:
		e.line("if b, err = %s.appendJSON(b); err != nil {\nreturn nil, err\n}", expr)
	}
}

// field appends the statements that write one field of the value v.
func (e *encoder) field(enc fieldEncoding) {
	f := enc.f
	key := keyLit(f.JSONName)
	get := "v." + f.GoName
	writeAlways := func() {
		if enc.always != "" {
			e.line("b = append(b, %s...)", strconv.Quote(","+string(quoteJSON(f.JSONName))+":"+enc.always))
		}
	}
	switch {
	case f.Required:
		e.line("b = append(b, %s...)", key)
		e.value(f.GoType, get)
	case f.Nullable:
		cond := get + ".Set && !" + get + ".Null"
		if enc.omitted != "" {
			cond += " && " + get + ".Value != " + enc.omitted
		}
		e.line("switch {")
		e.line("case %s:", cond)
		e.line("b = append(b, %s...)", key)
		e.value(f.GoType, get+".Value")
		if enc.patch {
			e.line("case %s.Set && %s.Null:", get, get)
			e.line("b = append(b, %s...)", strconv.Quote(","+string(quoteJSON(f.JSONName))+":null"))
		}
		if enc.always != "" {
			e.line("default:")
			writeAlways()
		}
		e.line("}")
	case f.pointer():
		cond := get + " != nil"
		if enc.omitted != "" {
			cond += " && *" + get + " != " + enc.omitted
		}
		e.line("if %s {", cond)
		e.line("b = append(b, %s...)", key)
		e.value(f.GoType, "(*"+get+")")
		if enc.always != "" {
			e.line("} else {")
			writeAlways()
		}
		e.line("}")
	default: // optional container
		var cond string
		switch {
		case isRawType(f.GoType):
			// nil is None, and json.RawMessage("null") Some(Value::Null),
			// which Rust writes. Decoding reads null as None unless the
			// field is a patch (see markRawOptions).
			cond = get + " != nil"
		case f.GoType == "any" || resolveKind(f.GoType) == kindAny:
			cond = get + " != nil"
		case f.NullOK || enc.always != "":
			cond = get + " != nil"
		default:
			cond = "len(" + get + ") > 0"
		}
		e.line("if %s {", cond)
		e.line("b = append(b, %s...)", key)
		e.value(f.GoType, get)
		if enc.always != "" {
			e.line("} else {")
			writeAlways()
		}
		e.line("}")
	}
}

// isRawType reports whether a Go type holds raw JSON.
func isRawType(typ string) bool {
	return typ == "json.RawMessage" || resolveKind(typ) == kindRaw
}

// resolveKind returns the kind of the def a Go type names, or -1.
func resolveKind(typ string) defKind {
	if td := resolve(typ); td != nil {
		return td.Kind
	}
	return -1
}

// emitMarshal writes MarshalJSON and appendJSON of an object-shaped type
// whose members appendMembers writes.
func emitMarshal(b *strings.Builder, name string) {
	fmt.Fprintf(b, "func (v %s) MarshalJSON() ([]byte, error) { return v.appendJSON(nil) }\n\n", name)
	fmt.Fprintf(b, "func (v *%s) appendJSON(b []byte) ([]byte, error) {\n\tstart := len(b)\n\tb, err := v.appendMembers(b)\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\treturn closeObject(b, start), nil\n}\n\n", name)
}

// emitStructEncode writes the encoder of a struct with the given fields, in
// wire order. def is the def the struct belongs to (the union for an inline
// variant payload), owner the Go type.
func emitStructEncode(b *strings.Builder, r *ir, td *typeDef, def, owner string, fields []field) {
	emitMarshal(b, owner)
	fmt.Fprintf(b, "func (v *%s) appendMembers(b []byte) ([]byte, error) {\n", owner)
	e := &encoder{b: b}
	e.line("var err error")
	e.line("_ = err")
	for _, f := range fields {
		if f.Variant != "" {
			continue // written with its tag
		}
		if td != nil && td.Flatten != nil && f.JSONName == td.Flatten.TagJSON {
			emitFlattenedTag(e, r, td)
			continue
		}
		e.field(encodingOf(r, def, owner, f))
	}
	if td != nil && td.Extras {
		e.line("if len(v.Extra) > 0 {\nif b, err = appendObjectMembers(b, v.Extra); err != nil {\nreturn nil, err\n}\n}")
	}
	e.line("return b, nil")
	e.line("}")
	b.WriteString("\n")
}

// emitFlattenedTag writes the members of a union flattened into a struct:
// the tag, then the payload fields of a known value, or the other fields of a
// custom one sorted by key, as Rust's Other type writes its BTreeMap.
func emitFlattenedTag(e *encoder, r *ir, td *typeDef) {
	fl := td.Flatten
	get := "v." + fl.TagGo
	e.line("if %s != nil {", get)
	e.line("b = append(b, %s...)", keyLit(fl.TagJSON))
	e.line("b = appendString(b, string(*%s))", get)
	e.line("switch *%s {", get)
	payloads := map[string][]field{}
	for _, f := range td.Fields {
		if f.Variant != "" {
			payloads[f.Variant] = append(payloads[f.Variant], f)
		}
	}
	plain := []string{`""`}
	for _, known := range fl.Known {
		if len(payloads[known]) == 0 {
			plain = append(plain, strconv.Quote(known))
			continue
		}
		e.line("case %q:", known)
		for _, f := range payloads[known] {
			e.field(encodingOf(r, td.Name, td.Name, f))
		}
	}
	e.line("case %s:", strings.Join(plain, ", "))
	e.line("default:")
	skip := sharedKeys(td) // the tag among them
	e.line("if b, err = appendRawMembers(b, %s, %s); err != nil {\nreturn nil, err\n}", "v."+fl.ExtrasGo, goStrings(skip))
	e.line("}")
	e.line("}")
}

func goStrings(items []string) string {
	if len(items) == 0 {
		return "nil"
	}
	return "[]string{" + quoteList(items) + "}"
}

// emitUnionEncode writes the encoder of a tagged or untagged union.
func emitUnionEncode(b *strings.Builder, r *ir, td *typeDef) {
	tagged := td.Kind == kindTaggedUnion
	open := hasOpenVariant(td)
	var arrays []variant
	for _, v := range td.Variants {
		if strings.HasPrefix(v.Payload, "[]") {
			arrays = append(arrays, v)
		}
	}
	count := func() {
		fmt.Fprintf(b, "\tset := 0\n")
		for _, v := range td.Variants {
			fmt.Fprintf(b, "\tif v.%s != nil {\n\t\tset++\n\t}\n", v.GoName)
		}
		fmt.Fprintf(b, "\tif set != 1 {\n\t\treturn nil, fmt.Errorf(\"%%s: exactly one variant must be set, have %%d\", \"%s\", set)\n\t}\n", td.Name)
	}

	fmt.Fprintf(b, "func (v %s) MarshalJSON() ([]byte, error) { return v.appendJSON(nil) }\n\n", td.Name)
	fmt.Fprintf(b, "func (v *%s) appendJSON(b []byte) ([]byte, error) {\n\tvar err error\n", td.Name)
	if !tagged && (open || len(arrays) > 0) {
		count()
		e := &encoder{b: b}
		if open {
			e.line("switch v.rawVariant {")
			for _, v := range td.Variants {
				if !v.Open {
					e.line("case %q:\nif v.%s != nil && unchangedSince(v.raw, v.%s) {\nreturn appendRaw(b, v.raw)\n}", v.GoName, v.GoName, v.GoName)
				}
			}
			e.line("}")
			for _, v := range td.Variants {
				if v.Open {
					e.line("if v.%s != nil {\nreturn appendRaw(b, v.%s.Raw)\n}", v.GoName, v.GoName)
				}
			}
		}
		for _, v := range arrays {
			e.line("if v.%s != nil {", v.GoName)
			e.value(v.Payload, "(*v."+v.GoName+")")
			e.line("return b, nil\n}")
		}
	}
	fmt.Fprintf(b, "\tstart := len(b)\n\tif b, err = v.appendMembers(b); err != nil {\n\t\treturn nil, err\n\t}\n\treturn closeObject(b, start), nil\n}\n\n")

	fmt.Fprintf(b, "func (v *%s) appendMembers(b []byte) ([]byte, error) {\n", td.Name)
	count()
	e := &encoder{b: b}
	e.line("var err error")
	e.line("_ = err")
	at := 0
	if len(td.Fields) > 0 {
		var err error
		if at, err = flattenIndex(td.Name, td.Fields); err != nil {
			panic(err.Error())
		}
	}
	for _, f := range td.Fields[:at] {
		e.field(encodingOf(r, td.Name, td.Name, f))
	}
	var shared []string
	for _, f := range td.Fields {
		shared = append(shared, f.JSONName)
	}
	e.line("switch {")
	for _, v := range td.Variants {
		e.line("case v.%s != nil:", v.GoName)
		switch {
		case v.Open && tagged:
			e.line("if b, err = appendOpenMembers(b, v.%s.Raw, %q, %s, %s, %s); err != nil {\nreturn nil, err\n}",
				v.GoName, td.TagJSON, goStrings(openDeclared(v)), openScopes(v), goStrings(shared))
		case v.Open:
			e.line("return nil, fmt.Errorf(\"%s: the %s variant is not an object\")", td.Name, v.GoName)
		case strings.HasPrefix(v.Payload, "[]"):
			e.line("return nil, fmt.Errorf(\"%s: the %s variant is not an object\")", td.Name, v.GoName)
		default:
			if v.Const != "" {
				e.line("b = append(b, %s...)", strconv.Quote(","+string(quoteJSON(td.TagJSON))+":"+string(quoteJSON(v.Const))))
			}
			e.line("if b, err = v.%s.appendMembers(b); err != nil {\nreturn nil, err\n}", v.GoName)
		}
	}
	e.line("}")
	for _, f := range td.Fields[at:] {
		e.field(encodingOf(r, td.Name, td.Name, f))
	}
	e.line("return b, nil")
	e.line("}")
	b.WriteString("\n")
}

// openDeclared lists the fields Rust's Other type declares after the tag, in
// order; an optional one is marked with a trailing "?".
func openDeclared(v variant) []string {
	var keys []string
	for _, f := range v.Inline {
		if f.Required {
			keys = append(keys, f.JSONName)
		} else {
			keys = append(keys, f.JSONName+"?")
		}
	}
	return keys
}

// openScopes renders the arms of the union an open variant also flattens
// (an elicitation scope), each as its fields in order with the optional ones
// marked; the first arm whose required fields are present is written.
func openScopes(v variant) string {
	if len(v.OpenAnyOf) == 0 {
		return "nil"
	}
	var arms []string
	for _, name := range v.OpenAnyOf {
		td := resolve(name)
		if td == nil || td.Kind != kindStruct {
			panic(fmt.Sprintf("open variant scope %s is not a struct", name))
		}
		var keys []string
		for _, key := range structOrder(td) {
			for _, f := range td.Fields {
				if f.JSONName == key {
					if !f.Required {
						key += "?"
					}
					keys = append(keys, key)
				}
			}
		}
		arms = append(arms, "{"+quoteList(keys)+"}")
	}
	return "[][]string{" + strings.Join(arms, ", ") + "}"
}

// emitAnyEncoder writes appendX for a union of primitives: each Go value its
// decoder yields is written as the Rust arm holding it, a float64 as an f64
// only where an arm is one. Any other value is written by encoding/json.
func emitAnyEncoder(b *strings.Builder, td *typeDef) {
	if len(td.AnyArms) == 0 {
		return
	}
	fmt.Fprintf(b, "// append%sUnion writes a %s value as the Rust arm holding it.\nfunc append%sUnion(b []byte, v %s) ([]byte, error) {\n", td.Name, td.Name, td.Name, td.Name)
	e := &encoder{b: b}
	e.line("var err error")
	e.line("_ = err")
	e.line("switch x := v.(type) {")
	e.line("case nil:\nreturn append(b, \"null\"...), nil")
	e.line("case json.RawMessage:\nreturn appendRaw(b, x)")
	seen := map[string]bool{"json.RawMessage": true}
	for _, arm := range td.AnyArms {
		var typ string
		switch arm.Kind {
		case "string":
			typ = "string"
		case "boolean":
			typ = "bool"
		case "number":
			typ = "float64"
		case "integer":
			typ = arm.GoType
		case "array":
			typ = "[]" + arm.GoType
		default:
			continue
		}
		if seen[typ] {
			continue
		}
		seen[typ] = true
		e.line("case %s:", typ)
		e.value(typ, "x")
		e.line("return b, nil")
	}
	e.line("}")
	e.line("return appendAny(b, v)")
	e.line("}")
	b.WriteString("\n")
}

// emitEncodeHelpers writes the helpers the generated encoders share.
func emitEncodeHelpers(b *strings.Builder) {
	b.WriteString(`// closeObject finishes an object whose members, each written with a leading
// comma, start at start: the first comma becomes the opening brace.
func closeObject(b []byte, start int) []byte {
	if len(b) == start {
		return append(b, '{', '}')
	}
	b[start] = '{'
	return append(b, '}')
}

// appendString writes s as a JSON string with the escapes serde_json writes:
// \", \\, \b, \f, \n, \r, \t, and \u00XX for the other control characters.
// Invalid UTF-8 is written as U+FFFD, as encoding/json does.
func appendString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c < utf8.RuneSelf {
			i++
			continue
		}
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				b = append(b, s[start:i]...)
				b = append(b, "\ufffd"...)
				i++
				start = i
				continue
			}
			i += size
			continue
		}
		b = append(b, s[start:i]...)
		switch c {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
		}
		i++
		start = i
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

// appendFloat writes an f64 as serde_json does.
func appendFloat(b []byte, f float64) []byte {
	return append(b, serdejson.FormatFloat(f)...)
}

// appendAny writes an untyped value as serde_json writes the
// serde_json::Value holding it (jsonvalue.Append).
func appendAny(b []byte, v any) ([]byte, error) {
	return jsonvalue.Append(b, v)
}

// appendRaw writes raw JSON as serde_json re-encodes the serde_json::Value
// it reads it as. Empty raw JSON is null.
func appendRaw(b []byte, raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return append(b, "null"...), nil
	}
	var compact bytes.Buffer
	compact.Grow(len(raw))
	if err := json.Compact(&compact, raw); err != nil {
		return nil, fmt.Errorf("invalid raw JSON %q: %w", raw, err)
	}
	encoded, err := serdewire.Encode(compact.Bytes())
	if err != nil {
		return nil, fmt.Errorf("invalid raw JSON %q: %w", raw, err)
	}
	return append(b, encoded...), nil
}

// sortedKeys returns the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// appendRawMembers writes the members of m, the other fields of a custom
// flattened value, sorted by key as Rust's BTreeMap holds them, leaving out
// the keys in skip.
func appendRawMembers(b []byte, m map[string]json.RawMessage, skip []string) ([]byte, error) {
	var err error
	for _, key := range sortedKeys(m) {
		if slices.Contains(skip, key) {
			continue
		}
		b = append(b, ',')
		b = appendString(b, key)
		b = append(b, ':')
		if b, err = appendRaw(b, m[key]); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// appendObjectMembers writes the members of raw, a JSON object such as the
// other members of a struct Rust flattens a serde_json::Map into, each with a
// leading comma, as serde_json writes that map.
func appendObjectMembers(b []byte, raw json.RawMessage) ([]byte, error) {
	value, err := serdejson.ParseAnyDepth(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid raw JSON %q: %w", raw, err)
	}
	if value.Kind != serdejson.Object {
		return nil, fmt.Errorf("other members are not an object: %s", raw)
	}
	for _, member := range value.Members {
		b = append(b, ',')
		b = appendString(b, member.Key)
		b = append(b, ':')
		b = append(b, serdejson.Encode(member.Value)...)
	}
	return b, nil
}

// appendOpenMembers writes the members of an open variant's wire object as
// Rust's Other type does: the tag, then the fields it declares in order (one
// marked optional with a trailing "?" is left out when null), then those of
// the first scope arm whose required fields are present, then every other
// member but the keys of any scope arm sorted by key, as its flattened
// BTreeMap holds them. Values are
// re-encoded as serde_json::Value. skip lists the keys the enclosing struct
// owns.
func appendOpenMembers(b []byte, raw json.RawMessage, tag string, declared []string, scopes [][]string, skip []string) ([]byte, error) {
	value, err := serdejson.ParseAnyDepth(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid raw JSON %q: %w", raw, err)
	}
	if value.Kind != serdejson.Object {
		return nil, fmt.Errorf("open variant is not an object: %s", raw)
	}
	written := map[string]bool{}
	write := func(key string, optional bool) {
		item, ok := value.Get(key)
		written[key] = true
		if !ok || (optional && item.Kind == serdejson.Null) {
			return
		}
		b = append(b, ',')
		b = appendString(b, key)
		b = append(b, ':')
		b = append(b, serdejson.Encode(item)...)
	}
	write(tag, false)
	fields := func(keys []string) {
		for _, key := range keys {
			name, optional := strings.CutSuffix(key, "?")
			write(name, optional)
		}
	}
	fields(declared)
	for _, arm := range scopes {
		matched := true
		for _, key := range arm {
			if !strings.HasSuffix(key, "?") {
				if _, ok := value.Get(key); !ok {
					matched = false
				}
			}
		}
		if matched {
			fields(arm)
			break
		}
	}
	// Rust's Other type takes every scope key out of its other fields,
	// whichever arm matched.
	for _, arm := range scopes {
		for _, key := range arm {
			written[strings.TrimSuffix(key, "?")] = true
		}
	}
	rest := make([]serdejson.Member, 0, len(value.Members))
	for _, member := range value.Members {
		if !written[member.Key] && !slices.Contains(skip, member.Key) {
			rest = append(rest, member)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].Key < rest[j].Key })
	for _, member := range rest {
		b = append(b, ',')
		b = appendString(b, member.Key)
		b = append(b, ':')
		b = append(b, serdejson.Encode(member.Value)...)
	}
	return b, nil
}

`)
}
