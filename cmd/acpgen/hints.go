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
	if strings.HasPrefix(f.GoType, "map[") {
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
