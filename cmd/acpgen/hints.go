package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// hintCase is one generated decode case: a wire document for a def with one
// field (or union tag) set to a value that exercises a deserialize rule.
// The expected outcome is captured from the Rust schema crate (see
// scripts/capture_hint_fixtures.py) and checked by the generated test.
type hintCase struct {
	name  string // "Def.field/case", or "Def[Variant].field/case"
	def   string // def (Go and Rust type) name
	input string
}

// envelopeDefs are JSON-RPC envelope defs. The Rust types of the same name
// are the routing enums of the method payloads, a different wire shape, so
// they get no cases.
var envelopeDefs = map[string]bool{
	"ClientNotification": true,
	"ClientRequest":      true,
	"ProxyNotification":  true,
	"ProxyRequest":       true,
	"ProxyResponse":      true,
}

// rustOmittedDefaults are field values the Rust types leave out when
// encoding (skip_serializing_if a default) that the schema does not record as
// defaults, keyed by def and field, with the protocol version. The generated
// check reads the field's absence as that value.
var rustOmittedDefaults = map[string]struct {
	protocol int
	value    string
}{
	"ToolCall.kind":   {1, `"other"`},   // ToolKind::is_default
	"ToolCall.status": {1, `"pending"`}, // ToolCallStatus::is_default
}

// patchField reports whether the def's field is a patch (Rust
// MaybeUndefined), whose null differs from omission. Elsewhere Rust decodes
// null as an omitted Option and does not encode it, while v2 Go types may
// keep it as Nullable.
func patchField(r *ir, def, key string) bool {
	if r.PreserveNull {
		p, ok := patchDefs[def]
		return ok && (p.upsert || key != "_meta")
	}
	for _, k := range v1PatchDefs[def] {
		if k == key {
			return true
		}
	}
	return false
}

// fieldRules lists, per Go type and wire key, what the generated check
// needs to compare a decoded value with the Rust one: the default a missing
// value means (the schema's default, or rustOmittedDefaults), and whether
// the field is a patch whose null differs from omission.
func fieldRules(r *ir) map[string]string {
	protocol := 1
	if r.PreserveNull {
		protocol = 2
	}
	out := map[string]string{}
	add := func(defName, owner string, fields []field) {
		for _, f := range fields {
			var rule []string
			def := f.Default
			if d, ok := rustOmittedDefaults[owner+"."+f.JSONName]; ok && d.protocol == protocol {
				def = d.value
			}
			if def != "" {
				rule = append(rule, fmt.Sprintf("Default: %q", def))
			}
			if f.Nullable && owner == defName && patchField(r, defName, f.JSONName) {
				rule = append(rule, "Patch: true")
			}
			if len(rule) > 0 {
				out[owner+"."+f.JSONName] = "{" + strings.Join(rule, ", ") + "}"
			}
		}
	}
	for _, td := range r.Defs {
		add(td.Name, td.Name, td.Fields)
		for _, v := range td.Variants {
			if !v.Open && v.Payload == "" {
				add(td.Name, td.Name+v.GoName, v.Inline)
			}
		}
	}
	return out
}

// hintCases derives the decode cases from the schema: every field gets a
// null value, and a null item or map value; every hinted or required field
// is also left out; every hinted field also gets a valid value, a malformed
// one, and for lists invalid items, or for refs an empty object; every
// tagged union gets a missing, null, non-string, empty, custom, and bare
// known tag; a flattened union gets custom, null, and malformed tags. Each
// case starts from the def's minimal document, so new pins are covered
// without editing the generator.
func hintCases(r *ir) ([]hintCase, error) {
	curIR = r
	var out []hintCase
	for _, td := range r.Defs {
		if envelopeDefs[td.Name] {
			continue
		}
		switch td.Kind {
		case kindStruct:
			cases, err := structHintCases(r, td)
			if err != nil {
				return nil, err
			}
			out = append(out, cases...)
		case kindTaggedUnion, kindUntaggedUnion:
			cases, err := unionHintCases(r, td)
			if err != nil {
				return nil, err
			}
			out = append(out, cases...)
		}
	}
	encode, err := encodeCases(r)
	if err != nil {
		return nil, err
	}
	out = append(out, encode...)
	seen := map[string]bool{}
	for _, c := range out {
		if strings.Contains(c.input, "`") {
			return nil, fmt.Errorf("%s: case input contains a backquote", c.name)
		}
		if seen[c.name] {
			return nil, fmt.Errorf("%s: duplicate case name", c.name)
		}
		seen[c.name] = true
	}
	return out, nil
}

func structHintCases(r *ir, td *typeDef) ([]hintCase, error) {
	if len(td.Fields) == 0 {
		return nil, nil
	}
	minimal, err := synthWire(r, td.Name, false, map[string]int{})
	if err != nil {
		return nil, err
	}
	var out []hintCase
	for _, f := range td.Fields {
		if td.Flatten != nil && f.JSONName == td.Flatten.TagJSON {
			continue // the tag cases follow
		}
		base := minimal
		if fl := td.Flatten; fl != nil && f.Variant != "" {
			if base, err = withMember(base, fl.TagJSON, fmt.Sprintf("%q", f.Variant)); err != nil {
				return nil, err
			}
		}
		cases, err := fieldHintCases(r, td.Name, td.Name, base, f)
		if err != nil {
			return nil, err
		}
		out = append(out, cases...)
	}
	seqCases, err := seqHintCases(td.Name, minimal, structOrder(td), td.Fields)
	if err != nil {
		return nil, err
	}
	out = append(out, seqCases...)
	if fl := td.Flatten; fl != nil {
		type tagCase struct{ name, value, extra string }
		cases := []tagCase{{"custom", `"_custom"`, "_extra"}, {"null", "null", "_extra"}, {"malformed", "true", "_extra"}, {"index", "0", "_extra"}}
		for _, key := range flattenReservedKeys[td.Name] {
			cases = append(cases, tagCase{"custom-" + key, `"_custom"`, key})
		}
		for _, tc := range cases {
			doc, err := withMember(minimal, fl.TagJSON, tc.value)
			if err == nil {
				doc, err = withMember(doc, tc.extra, "1")
			}
			if err != nil {
				return nil, err
			}
			out = append(out, hintCase{name: td.Name + "." + fl.TagJSON + "/" + tc.name, def: td.Name, input: doc})
		}
	}
	return out, nil
}

func unionHintCases(r *ir, td *typeDef) ([]hintCase, error) {
	minimal, err := synthWire(r, td.Name, false, map[string]int{})
	if err != nil {
		return nil, err
	}
	var out []hintCase
	if strings.HasPrefix(minimal, "{") {
		for _, f := range td.Fields {
			cases, err := fieldHintCases(r, td.Name, td.Name, minimal, f)
			if err != nil {
				return nil, err
			}
			out = append(out, cases...)
		}
	}
	for i := range td.Variants {
		v := &td.Variants[i]
		if v.Open || v.Payload != "" || len(v.Inline) == 0 {
			continue
		}
		base, err := synthVariant(r, td, v, false, map[string]int{td.Name: 1})
		if err != nil {
			return nil, err
		}
		for _, f := range v.Inline {
			cases, err := fieldHintCases(r, td.Name, td.Name+"["+v.GoName+"]", base, f)
			if err != nil {
				return nil, err
			}
			out = append(out, cases...)
		}
	}
	if td.Kind == kindTaggedUnion && strings.HasPrefix(minimal, "{") {
		for _, tc := range []struct{ name, value string }{
			{"missing", ""}, {"null", "null"}, {"malformed", "true"}, {"empty", `""`}, {"custom", `"_custom"`},
		} {
			doc, err := withMember(minimal, td.TagJSON, tc.value)
			if err != nil {
				return nil, err
			}
			out = append(out, hintCase{name: td.Name + "." + td.TagJSON + "/" + tc.name, def: td.Name, input: doc})
		}
		// An integer tag: serde reads it as a variant index from buffered
		// content only.
		if tag, index, ok := tagIndex(td, minimal); ok {
			doc, err := withMember(minimal, td.TagJSON, fmt.Sprint(index))
			if err != nil {
				return nil, err
			}
			out = append(out, hintCase{name: td.Name + "." + td.TagJSON + "/index-" + tag, def: td.Name, input: doc})
		}
		// The array form, [tag, payload fields...].
		if len(td.Fields) == 0 {
			doc, err := unionSeqDoc(r, td, minimal)
			if err != nil {
				return nil, err
			}
			if doc != "" {
				out = append(out, hintCase{name: td.Name + "/array", def: td.Name, input: doc})
			}
		}
		// Each known tag with only the shared fields: the variant payload's
		// required fields are missing, so the untagged variants are tried.
		keep := map[string]bool{td.TagJSON: true}
		for _, f := range td.Fields {
			keep[f.JSONName] = true
		}
		for _, v := range td.Variants {
			if v.Const == "" {
				continue
			}
			doc, err := withMember(minimal, td.TagJSON, fmt.Sprintf("%q", v.Const))
			if err == nil {
				doc, err = onlyMembers(doc, keep)
			}
			if err != nil {
				return nil, err
			}
			out = append(out, hintCase{name: td.Name + "." + td.TagJSON + "/bare-" + v.Const, def: td.Name, input: doc})
		}
		// A custom tag with only the shared fields: the open variant's
		// required fields are missing.
		doc, err := withMember(minimal, td.TagJSON, `"_custom"`)
		if err == nil {
			doc, err = onlyMembers(doc, keep)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, hintCase{name: td.Name + "." + td.TagJSON + "/bare-custom", def: td.Name, input: doc})
	}
	return out, nil
}

// structOrder returns a struct's fields in Rust declaration order, or in
// field order for a struct that does not read from an array.
func structOrder(td *typeDef) []string {
	if len(td.SeqOrder) > 0 {
		return withoutLifted(td, td.SeqOrder)
	}
	var keys []string
	for _, f := range td.Fields {
		if f.Variant == "" {
			keys = append(keys, f.JSONName)
		}
	}
	return keys
}

// withoutLifted drops the lifted variant fields of a flattened union, which
// the schema does not list among the struct's properties.
func withoutLifted(td *typeDef, order []string) []string {
	var out []string
	for _, key := range order {
		for _, f := range td.Fields {
			if f.JSONName == key && f.Variant == "" {
				out = append(out, key)
			}
		}
	}
	return out
}

// positional renders the object doc as an array in order: members it has
// keep their value, others are null, and the array ends after the last
// member present. extra appends a trailing null.
func positional(doc string, order []string, extra bool) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &obj); err != nil {
		return "", err
	}
	last := -1
	for i, key := range order {
		if _, ok := obj[key]; ok {
			last = i
		}
	}
	n := last + 1
	if extra {
		n = len(order) + 1
	}
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		value := "null"
		if i < len(order) {
			if raw, ok := obj[order[i]]; ok {
				value = string(raw)
			}
		}
		parts = append(parts, value)
	}
	return "[" + strings.Join(parts, ", ") + "]", nil
}

// seqHintCases builds the array cases of a struct: its minimal document read
// positionally, an empty array, and one element more than it has fields.
func seqHintCases(def, minimal string, order []string, fields []field) ([]hintCase, error) {
	short, err := positional(minimal, order, false)
	if err != nil {
		return nil, err
	}
	long, err := positional(minimal, order, true)
	if err != nil {
		return nil, err
	}
	out := []hintCase{
		{name: def + "/array", def: def, input: short},
		{name: def + "/array-long", def: def, input: long},
	}
	if short != "[]" {
		out = append(out, hintCase{name: def + "/array-empty", def: def, input: "[]"})
	}
	return out, nil
}

// tagIndex returns the tag of a union value and the index serde maps an
// integer tag onto.
func tagIndex(td *typeDef, doc string) (string, int, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(doc), &obj) != nil {
		return "", 0, false
	}
	var tag string
	if json.Unmarshal(obj[td.TagJSON], &tag) != nil {
		return "", 0, false
	}
	consts := append([]variant(nil), td.Variants...)
	sort.SliceStable(consts, func(i, j int) bool { return consts[i].Rank < consts[j].Rank })
	index := 0
	for _, v := range consts {
		if v.Const == "" {
			continue
		}
		if v.Const == tag {
			return tag, index, true
		}
		index++
	}
	return "", 0, false
}

// unionSeqDoc renders a tagged union's minimal document in the array form:
// the tag, then the chosen variant payload's fields positionally.
func unionSeqDoc(r *ir, td *typeDef, minimal string) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(minimal), &obj); err != nil {
		return "", err
	}
	var tag string
	if json.Unmarshal(obj[td.TagJSON], &tag) != nil {
		return "", nil
	}
	for _, v := range td.Variants {
		if v.Const != tag {
			continue
		}
		order := v.SeqOrder
		if v.Payload != "" {
			payload := resolve(v.Payload)
			if payload == nil || payload.Kind != kindStruct {
				return "", nil
			}
			order = structOrder(payload)
		}
		delete(obj, td.TagJSON)
		rest, err := json.Marshal(obj)
		if err != nil {
			return "", err
		}
		doc, err := positional(string(rest), order, false)
		if err != nil {
			return "", err
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(doc, "["), "]")
		if inner == "" {
			return fmt.Sprintf("[%q]", tag), nil
		}
		return fmt.Sprintf("[%q, %s]", tag, inner), nil
	}
	return "", nil
}

// onlyMembers returns doc with only the keep keys.
func onlyMembers(doc string, keep map[string]bool) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &obj); err != nil {
		return "", err
	}
	out := "{}"
	for key, value := range obj {
		if keep[key] {
			var err error
			if out, err = withMember(out, key, string(value)); err != nil {
				return "", err
			}
		}
	}
	return out, nil
}

// fieldHintCases builds the cases of one field, starting from base (the
// owner's minimal document) with the field set to its synthesized value.
func fieldHintCases(r *ir, def, owner, base string, f field) ([]hintCase, error) {
	hinted := f.DefaultOnError || f.SkipInvalid || f.DefaultOnNull
	value, err := synthValue(r, f, true, map[string]int{def: 1})
	if err != nil {
		return nil, err
	}
	valid, err := withMember(base, f.JSONName, value)
	if err != nil {
		return nil, err
	}
	type variantCase struct{ name, value string }
	// Every field: null, and a null list item or map value.
	cases := []variantCase{{"null", "null"}}
	var item string
	elem, list := strings.CutPrefix(f.GoType, "[]")
	if list {
		if item, err = synthElem(r, elem, map[string]int{def: 1}); err != nil {
			return nil, err
		}
		cases = append(cases, variantCase{"null-item", "[null, " + item + "]"})
	}
	// The values of a _meta are covered by its encode cases (metaSamples).
	if strings.HasPrefix(f.GoType, "map[") || (f.GoType == "Meta" && f.JSONName != "_meta") {
		cases = append(cases, variantCase{"null-value", `{"k": null}`})
	}
	if f.Required || hinted {
		cases = append(cases, variantCase{"missing", ""})
	}
	// Enums: an unknown value and the map forms. Unions of primitives: an
	// object. Tagged unions: an integer tag.
	elemType := f.GoType
	if list {
		elemType = elem
	}
	wrap := func(v string) string {
		if list {
			return "[" + v + ", " + item + "]"
		}
		if strings.HasPrefix(f.GoType, "map[") {
			return `{"k": ` + v + `}`
		}
		return v
	}
	if strings.HasPrefix(f.GoType, "map[") {
		elemType = f.GoType[strings.Index(f.GoType, "]")+1:]
	}
	sample := value
	if list {
		sample = item
	}
	if td := resolve(elemType); td != nil {
		switch {
		case td.Kind == kindEnum && td.Base == "string":
			cases = append(cases, variantCase{"unknown", wrap(`"_unknown"`)})
			// serde also reads a unit variant from the map form.
			if len(td.Variants) > 0 {
				cases = append(cases,
					variantCase{"map-form", wrap(fmt.Sprintf(`{%q: null}`, td.Variants[0].Const))},
					variantCase{"map-form-unknown", wrap(`{"_unknown": null}`)},
					variantCase{"map-form-value", wrap(fmt.Sprintf(`{%q: 1}`, td.Variants[0].Const))})
			}
		case td.Kind == kindAny:
			cases = append(cases, variantCase{"object-value", wrap(`{"x": 1}`)})
		case td.Kind == kindTaggedUnion && !strings.HasPrefix(f.GoType, "map["):
			if _, index, ok := tagIndex(td, sample); ok {
				if indexed, err := withMember(sample, td.TagJSON, fmt.Sprint(index)); err == nil {
					if list {
						cases = append(cases, variantCase{"index-tag", "[" + indexed + "]"})
					} else {
						cases = append(cases, variantCase{"index-tag", indexed})
					}
				}
			}
		}
	}
	// Hinted fields: a valid value, and values the hint reads leniently.
	if hinted {
		malformed := "true"
		if f.GoType == "bool" {
			malformed = `"true"`
		}
		cases = append(cases, variantCase{"valid", value}, variantCase{"malformed", malformed})
		if f.NonZero {
			cases = append(cases, variantCase{"zero", "0"})
		}
		switch {
		case list:
			cases = append(cases, variantCase{"invalid-item", "[true, " + item + "]"})
			if r.ByName[elem] != nil && strings.HasPrefix(item, "{") {
				cases = append(cases, variantCase{"empty-item", "[{}, " + item + "]"})
			}
		case r.ByName[f.GoType] != nil && strings.HasPrefix(value, "{"):
			cases = append(cases, variantCase{"empty-object", "{}"})
		}
	}
	var out []hintCase
	for _, c := range cases {
		doc, err := withMember(valid, f.JSONName, c.value)
		if err != nil {
			return nil, err
		}
		out = append(out, hintCase{name: owner + "." + f.JSONName + "/" + c.name, def: def, input: doc})
	}
	return out, nil
}

// withMember returns doc with key set to the raw JSON value, or removed when
// value is empty. Keys are sorted, so the result is deterministic.
func withMember(doc, key, value string) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &obj); err != nil {
		return "", fmt.Errorf("synthesized document %s: %w", doc, err)
	}
	if obj == nil {
		obj = map[string]json.RawMessage{}
	}
	if value == "" {
		delete(obj, key)
	} else {
		obj[key] = json.RawMessage(value)
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%q: %s", k, obj[k]))
	}
	return "{" + strings.Join(parts, ", ") + "}", nil
}

func emitHints(r *ir, pin, packageName string) []byte {
	cases, err := hintCases(r)
	if err != nil {
		panic(fmt.Sprintf("hint cases: %v", err))
	}
	protocol := "v1"
	if r.PreserveNull {
		protocol = "v2"
	}
	var b strings.Builder
	b.WriteString(header(pin, "deserialize-hint decode cases"))
	b.WriteString("package " + packageName + "\n\nimport (\n\t\"reflect\"\n\t\"testing\"\n\n\t\"" + modulePath + "/internal/hintcheck\"\n)\n\n")
	b.WriteString("// TestDeserializeHints decodes each case and compares the outcome with the\n// Rust schema crate's, captured in testdata/hints_rust.json.\n")
	fmt.Fprintf(&b, "func TestDeserializeHints(t *testing.T) {\n\thintcheck.Run(t, hintcheck.Suite{\n\t\tPin: %q,\n\t\tProtocol: %q,\n\t\tFixtures: \"testdata/hints_rust.json\",\n\t\tFields: map[string]hintcheck.Field{\n", pin, protocol)
	rules := fieldRules(r)
	keys := make([]string, 0, len(rules))
	for k := range rules {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "\t\t\t%q: %s,\n", k, rules[k])
	}
	b.WriteString("\t\t},\n\t\tCases: hintCases,\n\t})\n}\n\n")
	b.WriteString("var hintCases = []hintcheck.Case{\n")
	for _, c := range cases {
		fmt.Fprintf(&b, "\t{Name: %q, Type: reflect.TypeOf(new(%s)).Elem(), Def: %q, Input: `%s`},\n", c.name, c.def, c.def, c.input)
	}
	b.WriteString("}\n")
	return formatSrc(b.String())
}

// modulePath is the import path of the module the generated packages live in.
const modulePath = "github.com/BrokkAi/acp-go"

// floatSamples are the number values the encode cases put in every f64
// field: serde_json writes an f64 with a decimal point or an exponent, by
// rules of its own, where encoding/json writes an integral float64 as an
// integer.
var floatSamples = []struct{ name, value string }{
	{"integral", "1"},
	{"negative-zero", "-0.0"},
	{"fraction", "0.1"},
	{"large", "1e20"},
	{"huge", "1e300"},
	{"small", "1e-7"},
	{"tiny", "5e-324"},
	{"digits", "123456789.123456789"},
	// serde_json, built without float_roundtrip, reads these as a float64
	// next to the nearest one.
	{"inexact", "7.2e-300"},
	{"rounded", "9007199254740993.0"},
	// Shortest digits serde_json's parsing would not read back as the same
	// float64, so a writer must not re-parse a typed value.
	{"shortest", "0.9405161066866751"},
}

// anySamples are the values the encode cases decode as each arm of a union
// of primitives, by arm kind.
var anySamples = map[string][]string{
	"string":  {`"s"`},
	"integer": {"-5", "0", "-0", "9223372036854775807", "18446744073709551615"},
	"number":  {"1", "1.0", "-0.0", "1e20", "0.1", "7.2e-300", "18446744073709551616"},
	"boolean": {"true", "false"},
	"null":    {"null"},
	"any":     {rawSample},
}

// escapeSample is a string serde_json writes with only its own escapes: the
// control characters, the quote, and the backslash. The rest is written as
// is, a solidus and an escaped surrogate pair included.
const escapeSample = `"<a href=\"x\">&amp;</a> \u2028\u2029 \u0001\u001f\b\f\t\n\r\\ \u00e9 \ud83d\ude00 \/"`

// integerType reports whether a Go field type is an integer, or a newtype or
// enum of one.
func integerType(typ string) bool {
	if strings.HasPrefix(typ, "int") || strings.HasPrefix(typ, "uint") {
		return true
	}
	td := resolve(typ)
	return td != nil && (td.Kind == kindNewtype || td.Kind == kindEnum) &&
		(strings.HasPrefix(td.Base, "int") || strings.HasPrefix(td.Base, "uint"))
}

// rawSample is the value the encode cases put in every raw JSON field.
const rawSample = `{"z": 1.0, "a": [1e2, -0, 0.10, 12345678901234567890, 123456789.123456789, 0.9405161066866751], "z": {"y": null, "x": "\u0041\/"}}`

// metaSample is the value the encode cases put in every Meta field: Rust
// holds it as a serde_json::Map, which keeps member order, merges a repeated
// key, keeps integers that fit u64 or i64 exact, and writes other numbers as
// the f64 its inexact parse reads.
const metaSample = `{"z": 1, "a": {"y": [1.0, -0.0, 1e2, 0.10, 7.2e-300, 0.9405161066866751], "x": null}, "z": 9007199254740993, "u": 18446744073709551615, "i": -9223372036854775808, "h": 12345678901234567890, "o": 18446744073709551616, "\u0041\/": "\ud83d\ude00\u2028\"\\\u0001<&>"}`

// metaSamples are the values the Meta cases decode as a Meta itself, Rust's
// serde_json::Map<String, Value>.
var metaSamples = []struct{ name, value string }{
	{"empty", `{}`},
	{"order", `{"z": 1, "a": 2, "m": {"y": 1, "b": 2}, "b": [3, 1, 2]}`},
	{"repeated", `{"z": 1, "a": 2, "z": {"y": 1, "x": 2, "y": 3}, "a": null}`},
	{"integers", `{"safe": 9007199254740991, "unsafe": 9007199254740993, "i64max": 9223372036854775807, "i64min": -9223372036854775808, "above": 9223372036854775808, "u64max": 18446744073709551615, "over": 18446744073709551616, "under": -9223372036854775809, "long": 12345678901234567890, "zero": 0, "negzero": -0, "neg": -1}`},
	{"floats", `{"one": 1.0, "negzero": -0.0, "e2": 1e2, "E2": 1E+2, "e20": 1e20, "e300": 1e300, "small": 1e-7, "tenth": 0.10, "subnormal": 5e-324, "max": 1.7976931348623157e308, "inexact": 7.2e-300, "rounded": 9007199254740993.0, "shortest": 0.9405161066866751, "long": 123456789.123456789, "pi": 3.141592653589793}`},
	{"nested", `{"a": [{"b": [1, [2.5, {"c": null, "a": true}]], "a": {}}, [], {}, [[[]]]], "t": true, "f": false, "n": null, "s": ""}`},
	{"escapes", `{"\u0041\/key": ` + escapeSample + `, "\u00e9": "\ud83d\ude00", "\"\\": "\u001f"}`},
	{"large", largeMetaSample()},
	{"whitespace", " {\n\t\"b\" : [ 1 , 2 ] ,\n \"a\" : { } } "},
	{"null", `null`},
	{"bool", `true`},
	{"integer", `5`},
	{"float", `1.5`},
	{"string", `"s"`},
	{"array", `[1]`},
}

// largeMetaSample is an object with more members than parsing scans for a
// repeated key, in reverse key order, with keys repeated near both ends.
func largeMetaSample() string {
	var members []string
	for i := 24; i >= 0; i-- {
		members = append(members, fmt.Sprintf(`"k%02d": %d`, i, i))
	}
	members = append(members, `"k24": "first again"`, `"k03": [3]`, `"k00": {"zero": 0.0}`)
	return "{" + strings.Join(members, ", ") + "}"
}

// encodeCases derives the cases that check encoding: every def's minimal and
// full documents, every union variant's full document, and each f64 field
// set to the floatSamples. The generated test requires json.Marshal of the
// decoded value to be byte for byte what the Rust schema crate writes for
// the same input.
func encodeCases(r *ir) ([]hintCase, error) {
	var out []hintCase
	fieldCases := func(name, def, doc string, fields []field) error {
		if !strings.HasPrefix(doc, "{") {
			return nil
		}
		for _, f := range fields {
			// An empty list or map: Rust writes it for an Option, and
			// leaves it out where it skips empty values.
			if !f.Required && f.Variant == "" {
				empty := ""
				switch {
				case strings.HasPrefix(f.GoType, "[]"):
					empty = "[]"
				case strings.HasPrefix(f.GoType, "map[") || f.GoType == "Meta":
					empty = "{}"
				}
				if empty != "" {
					input, err := withMember(doc, f.JSONName, empty)
					if err != nil {
						return err
					}
					out = append(out, hintCase{name: name + "." + f.JSONName + "/encode-empty", def: def, input: input})
				}
			}
			// Each arm of a union of primitives, which decodes as its own
			// Go value and must encode as Rust's arm does. A union of
			// primitives is a Go interface type, so it is exercised as a
			// field, where the generated codec handles it.
			elem, list := strings.CutPrefix(f.GoType, "[]")
			mapped := false
			if strings.HasPrefix(elem, "map[string]") {
				elem, mapped = strings.TrimPrefix(elem, "map[string]"), true
			}
			if td := resolve(elem); td != nil && td.Kind == kindAny && f.Variant == "" {
				for i, arm := range td.AnyArms {
					samples := anySamples[arm.Kind]
					if arm.Kind == "array" {
						item, err := synthElem(r, arm.GoType, map[string]int{def: 1})
						if err != nil {
							return err
						}
						samples = []string{"[]", "[" + item + "]"}
					}
					for j, sample := range samples {
						value := sample
						switch {
						case list:
							value = "[" + sample + "]"
						case mapped:
							value = `{"k": ` + sample + `}`
						}
						input, err := withMember(doc, f.JSONName, value)
						if err != nil {
							return err
						}
						out = append(out, hintCase{name: fmt.Sprintf("%s.%s/encode-arm-%d-%s-%d", name, f.JSONName, i, arm.Kind, j), def: def, input: input})
					}
				}
			}
			// Raw JSON, which Rust holds as a serde_json::Value: member
			// order kept, a repeated key once with its last value, and
			// numbers and escapes re-encoded.
			if f.GoType == "json.RawMessage" || (r.ByName[f.GoType] != nil && r.ByName[f.GoType].Kind == kindRaw) {
				input, err := withMember(doc, f.JSONName, rawSample)
				if err != nil {
					return err
				}
				out = append(out, hintCase{name: name + "." + f.JSONName + "/encode-raw", def: def, input: input})
			}
			// A Meta, which Rust holds as a serde_json::Map.
			if f.GoType == "Meta" {
				input, err := withMember(doc, f.JSONName, metaSample)
				if err != nil {
					return err
				}
				out = append(out, hintCase{name: name + "." + f.JSONName + "/encode-meta", def: def, input: input})
			}
			// -0, which serde_json reads as the f64 -0.0 that an integer
			// rejects.
			if integerType(f.GoType) && f.Variant == "" {
				input, err := withMember(doc, f.JSONName, "-0")
				if err != nil {
					return err
				}
				out = append(out, hintCase{name: name + "." + f.JSONName + "/negative-zero", def: def, input: input})
			}
			if f.GoType != "float64" {
				continue
			}
			for _, s := range floatSamples {
				input, err := withMember(doc, f.JSONName, s.value)
				if err != nil {
					return err
				}
				out = append(out, hintCase{name: name + "." + f.JSONName + "/float-" + s.name, def: def, input: input})
			}
		}
		return nil
	}
	// The discriminator enum synthesized for a flattened union is no Rust
	// type; a Rust type of the same name is the whole union.
	synthesized := map[string]bool{}
	for _, td := range r.Defs {
		if td.Flatten != nil {
			synthesized[td.Flatten.Enum] = true
		}
	}
	for _, td := range r.Defs {
		if envelopeDefs[td.Name] || synthesized[td.Name] {
			continue
		}
		switch td.Kind {
		case kindStruct, kindTaggedUnion, kindUntaggedUnion, kindNewtype, kindEnum, kindAny, kindRaw:
		default:
			continue
		}
		minimal, err := synthWire(r, td.Name, false, map[string]int{})
		if err != nil {
			return nil, err
		}
		full, err := synthWire(r, td.Name, true, map[string]int{})
		if err != nil {
			return nil, err
		}
		out = append(out,
			hintCase{name: td.Name + "/encode-minimal", def: td.Name, input: minimal},
			hintCase{name: td.Name + "/encode-full", def: td.Name, input: full})
		if td.Kind == kindRaw {
			out = append(out, hintCase{name: td.Name + "/encode-raw", def: td.Name, input: rawSample})
		}
		// A custom elicitation mode carrying the keys of both scopes: Rust's
		// Other type writes the scope that matched and drops the other's.
		if td.Kind == kindTaggedUnion {
			for _, v := range td.Variants {
				if v.Open && len(v.OpenAnyOf) > 1 {
					input := fmt.Sprintf(`{%q: "_custom", "message": "", "requestId": 1, "sessionId": "s", "toolCallId": null, "z": 1, "a": 2}`, td.TagJSON)
					out = append(out, hintCase{name: td.Name + "[" + v.GoName + "]/encode-scopes", def: td.Name, input: input})
				}
			}
		}
		// Other members of a struct Rust flattens a map into.
		if td.Extras && strings.HasPrefix(full, "{") && full != "{}" {
			extras := `"z": 1, "a": {"y": 2, "x": 1}, "z": 1.0, "q": null`
			out = append(out,
				hintCase{name: td.Name + "/encode-extras-first", def: td.Name, input: "{" + extras + ", " + full[1:]},
				hintCase{name: td.Name + "/encode-extras-last", def: td.Name, input: strings.TrimSuffix(full, "}") + ", " + extras + "}"})
		}
		// String escapes, in the text of a text content block.
		if td.Name == "TextContent" {
			input, err := withMember(full, "text", escapeSample)
			if err != nil {
				return nil, err
			}
			out = append(out, hintCase{name: td.Name + ".text/encode-escapes", def: td.Name, input: input})
		}
		if td.Kind == kindNewtype && td.Base == "float64" {
			for _, s := range floatSamples {
				out = append(out, hintCase{name: td.Name + "/float-" + s.name, def: td.Name, input: s.value})
			}
		}
		if err := fieldCases(td.Name, td.Name, full, td.Fields); err != nil {
			return nil, err
		}
		if td.Kind != kindTaggedUnion && td.Kind != kindUntaggedUnion {
			continue
		}
		for i := range td.Variants {
			v := &td.Variants[i]
			if v.Open {
				continue
			}
			doc, err := synthVariant(r, td, v, true, map[string]int{td.Name: 1})
			if err != nil {
				return nil, err
			}
			name := td.Name + "[" + v.GoName + "]"
			out = append(out, hintCase{name: name + "/encode-full", def: td.Name, input: doc})
			if err := fieldCases(name, td.Name, doc, v.Inline); err != nil {
				return nil, err
			}
		}
	}
	// Meta itself, Rust's serde_json::Map<String, Value>.
	for _, s := range metaSamples {
		out = append(out, hintCase{name: "Meta/" + s.name, def: "Meta", input: s.value})
	}
	return out, nil
}
