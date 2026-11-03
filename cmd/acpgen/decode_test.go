package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// hintRoot is a small schema with one field per x-deserialize-* hint shape
// the pinned schemas use.
func hintRoot() *rootSchema {
	object := json.RawMessage(`"object"`)
	return &rootSchema{Defs: map[string]*rawSchema{
		"Item": {Type: object, Required: []string{"name"}, Properties: map[string]*rawSchema{
			"name": {Type: json.RawMessage(`"string"`)},
		}},
		"Update": {Type: object, Required: []string{"id", "items", "args"}, Properties: map[string]*rawSchema{
			"id": {Type: json.RawMessage(`"string"`)},
			// DefaultOnError<VecSkipError<_>> on a required list.
			"items": {Type: json.RawMessage(`"array"`), Items: &rawSchema{Ref: "#/$defs/Item"}, DefaultOnError: true, SkipInvalidItems: true},
			// VecSkipError<_> alone.
			"strict": {Type: json.RawMessage(`"array"`), Items: &rawSchema{Ref: "#/$defs/Item"}, SkipInvalidItems: true},
			// DefaultOnNull on a required list.
			"args": {Type: json.RawMessage(`"array"`), Items: &rawSchema{Type: json.RawMessage(`"string"`)}, DefaultOnNull: true},
			// DefaultOnError on an optional value.
			"title": {Type: json.RawMessage(`["string","null"]`), DefaultOnError: true},
			// DefaultOnError<MaybeUndefined<VecSkipError<_>>> on a patch.
			"summary": {Type: json.RawMessage(`["array","null"]`), Items: &rawSchema{Ref: "#/$defs/Item"}, DefaultOnError: true, SkipInvalidItems: true},
			// A plain optional field: null is accepted, a bad value fails.
			"note": {Type: json.RawMessage(`["string","null"]`)},
			// A plain optional map whose values must not be null.
			"env": {Type: object, Additional: json.RawMessage(`{"type":"string"}`)},
		}},
	}}
}

func TestFieldsCarryDeserializeHints(t *testing.T) {
	root := hintRoot()
	td, err := classify("Update", root.Defs["Update"], root, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]field{}
	for _, f := range td.Fields {
		got[f.JSONName] = f
	}
	for key, want := range map[string]field{
		"id":      {Required: true},
		"items":   {Required: true, DefaultOnError: true, SkipInvalid: true},
		"strict":  {SkipInvalid: true},
		"args":    {Required: true, DefaultOnNull: true},
		"title":   {DefaultOnError: true, NullOK: true},
		"summary": {DefaultOnError: true, SkipInvalid: true, NullOK: true},
		"note":    {NullOK: true},
		"env":     {},
	} {
		f := got[key]
		if f.Required != want.Required || f.DefaultOnError != want.DefaultOnError || f.SkipInvalid != want.SkipInvalid ||
			f.DefaultOnNull != want.DefaultOnNull || f.NullOK != want.NullOK || f.ItemNullOK {
			t.Errorf("%s = %+v, want %+v", key, f, want)
		}
	}

	// The hint on a field that is not a list fails generation.
	root.Defs["Update"].Properties["id"].SkipInvalidItems = true
	if _, err := classify("Update", root.Defs["Update"], root, true); err == nil || !strings.Contains(err.Error(), "not a list") {
		t.Fatalf("skip-invalid-items on a string: %v", err)
	}
}

func TestDecoderAppliesEachHint(t *testing.T) {
	root := hintRoot()
	r := &ir{PreserveNull: true, ByName: map[string]*typeDef{}}
	for _, name := range []string{"Item", "Update"} {
		td, err := classify(name, root.Defs[name], root, true)
		if err != nil {
			t.Fatal(err)
		}
		r.Defs = append(r.Defs, td)
		r.ByName[name] = td
	}
	src := string(emitTypes(r, "test", "testpkg"))
	for _, want := range []string{
		// Required fields must be present, hints or not, checked after
		// the values in Rust declaration order.
		"if _, ok := raws.get(\"items\"); !ok {\n\t\treturn missingField(\"items\")\n\t}",
		"if _, ok := raws.get(\"name\"); !ok {\n\t\treturn missingField(\"name\")\n\t}",
		// Values decode in wire order; splitObject has merged repeated keys.
		"for _, m := range raws {\n\t\traw := m.raw\n\t\tswitch string(m.key) {",
		// DefaultOnError<VecSkipError>: bad items are dropped, and a bad
		// list reads as the default, an empty list for a required field.
		"if err := decodeList(raw, &v.Items, false, true, func(raw json.RawMessage, dst *Item) error {\n\t\t\t\treturn decodeValue(raw, dst, false, true)\n\t\t\t}); err != nil {\n\t\t\t\tv.Items = []Item{}\n\t\t\t}",
		// VecSkipError alone: bad items are dropped, a bad list fails.
		"if err := decodeList(raw, &v.Strict, false, true, func(raw json.RawMessage, dst *Item) error {\n\t\t\t\treturn decodeValue(raw, dst, false, true)\n\t\t\t}); err != nil {\n\t\t\t\treturn err\n\t\t\t}",
		// DefaultOnNull: null reads as the default.
		"if isNull(raw) {\n\t\t\t\tv.Args = []string{}\n\t\t\t} else if err := decodeList(raw, &v.Args, false, false, func(raw json.RawMessage, dst *string) error {",
		// DefaultOnError on a Nullable field reads a bad value as unset.
		"if err := decodeNullable(raw, &v.Title, func(raw json.RawMessage, dst *string) error {\n\t\t\t\treturn decodeValue(raw, dst, false, true)\n\t\t\t}); err != nil {\n\t\t\t\tv.Title = Nullable[string]{}\n\t\t\t}",
		"\t\t\t\t})\n\t\t\t}); err != nil {\n\t\t\t\tv.Summary = Nullable[[]Item]{}\n\t\t\t}",
		// The array form reads fields positionally.
		"func (v *Update) decodeSeq(items []json.RawMessage, buffered bool) error {",
		"return seqTooShort(\"Update\", 0, 8)",
		// No hint: a bad value fails the message with its own error.
		"\t\t\t\treturn decodeValue(raw, dst, false, buffered)\n\t\t\t}); err != nil {\n\t\t\t\treturn err\n\t\t\t}\n\t\tcase \"strict\":",
		// A typed map rejects null values.
		"if err := decodeMap(raw, &v.Env, false, func(raw json.RawMessage, dst *string) error {",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("generated source missing %q:\n%s", want, src)
		}
	}
}

func TestTaggedUnionDecodeTriesUntaggedVariantsInSchemaOrder(t *testing.T) {
	object := json.RawMessage(`"object"`)
	root := &rootSchema{Defs: map[string]*rawSchema{
		"Http":  {Type: object, Required: []string{"url"}, Properties: map[string]*rawSchema{"url": str()}},
		"Stdio": {Type: object, Required: []string{"command"}, Properties: map[string]*rawSchema{"command": str()}},
	}}
	httpArm := constTag("type", "http")
	httpArm.AllOf = []*rawSchema{{Ref: "#/$defs/Http"}}
	other := &rawSchema{Type: object, Required: []string{"type"}, Properties: map[string]*rawSchema{"type": str()}}
	def := &rawSchema{AnyOf: []*rawSchema{httpArm, other, {Title: "stdio", AllOf: []*rawSchema{{Ref: "#/$defs/Stdio"}}}}}
	td, err := classify("Server", def, root, false)
	if err != nil {
		t.Fatal(err)
	}
	r := &ir{ByName: map[string]*typeDef{"Server": td}, Defs: []*typeDef{td}}
	for _, name := range []string{"Http", "Stdio"} {
		payload, err := classify(name, root.Defs[name], root, false)
		if err != nil {
			t.Fatal(err)
		}
		r.Defs = append(r.Defs, payload)
		r.ByName[name] = payload
	}
	src := string(emitTypes(r, "test", "testpkg"))
	open := strings.Index(src, "if tagged && tag != \"http\" {")
	stdio := strings.Index(src, "payloadStdio := new(Stdio)")
	if open < 0 || stdio < 0 || open > stdio {
		t.Fatalf("untagged variants are not tried in schema order:\n%s", src)
	}
	for _, want := range []string{
		"case \"http\":\n\t\t\tpayload := new(Http)\n\t\t\tif err = payload.decodeFields(data, raws, true); err == nil {",
		// A union with untagged variants decodes from buffered content.
		"\tbuffered = true\n\traw, present := raws.get(\"type\")",
		"func (v *Server) decodeSeq(items []json.RawMessage, buffered bool) error {",
		// Rust reads the union as untagged: every failure is one error.
		"return errors.New(\"data did not match any variant of untagged enum Server\")",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("generated source missing %q:\n%s", want, src)
		}
	}
}

func TestNonZeroFieldsMustBeIntegersWithMinimumOne(t *testing.T) {
	one, zero := 1.0, 0.0
	def := &rawSchema{Type: json.RawMessage(`"object"`), Properties: map[string]*rawSchema{
		"line": {Type: json.RawMessage(`["integer","null"]`), Format: "uint32", Minimum: &one},
	}}
	td, err := classify("ToolCallLocation", def, &rootSchema{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyNonZeroFields(td, def, 2); err != nil || !td.Fields[0].NonZero {
		t.Fatalf("non-zero line = %+v, %v", td.Fields[0], err)
	}
	td, _ = classify("ToolCallLocation", def, &rootSchema{}, false)
	if err := applyNonZeroFields(td, def, 1); err != nil || td.Fields[0].NonZero {
		t.Fatal("v1 line must stay a plain integer")
	}
	def.Properties["line"].Minimum = &zero
	td, _ = classify("ToolCallLocation", def, &rootSchema{}, true)
	if err := applyNonZeroFields(td, def, 2); err == nil {
		t.Fatal("a listed field without minimum 1 was accepted")
	}
}

func TestHintCasesCoverEachField(t *testing.T) {
	root := hintRoot()
	r := &ir{ByName: map[string]*typeDef{}}
	for _, name := range []string{"Item", "Update"} {
		td, err := classify(name, root.Defs[name], root, false)
		if err != nil {
			t.Fatal(err)
		}
		r.Defs = append(r.Defs, td)
		r.ByName[name] = td
	}
	cases, err := hintCases(r)
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]string{}
	for _, c := range cases {
		inputs[c.name] = c.input
	}
	for name, want := range map[string]string{
		"Update.items/invalid-item": `{"args": [""], "id": "", "items": [true, {"name": ""}]}`,
		"Update.items/empty-item":   `{"args": [""], "id": "", "items": [{}, {"name": ""}]}`,
		"Update.items/missing":      `{"args": [""], "id": ""}`,
		"Update.args/null":          `{"args": null, "id": "", "items": [{"name": ""}]}`,
		"Update.title/malformed":    `{"args": [""], "id": "", "items": [{"name": ""}], "title": true}`,
		"Update.note/null":          `{"args": [""], "id": "", "items": [{"name": ""}], "note": null}`,
		"Update.env/null-value":     `{"args": [""], "env": {"k": null}, "id": "", "items": [{"name": ""}]}`,
		"Item.name/missing":         `{}`,
	} {
		if inputs[name] != want {
			t.Errorf("%s = %s, want %s", name, inputs[name], want)
		}
	}
	if _, ok := inputs["Update.note/malformed"]; ok {
		t.Error("a field without hints got a malformed case")
	}
	for name, want := range map[string]string{
		"Update/array":      `[[""], null, "", [{"name": ""}]]`,
		"Update/array-long": `[[""], null, "", [{"name": ""}], null, null, null, null, null]`,
	} {
		if inputs[name] != want {
			t.Errorf("%s = %s, want %s", name, inputs[name], want)
		}
	}
}

func TestClosedEnumsRejectUnknownValues(t *testing.T) {
	def := &rawSchema{OneOf: []*rawSchema{
		{Type: json.RawMessage(`"string"`), Const: json.RawMessage(`"read"`)},
		{Type: json.RawMessage(`"string"`), Const: json.RawMessage(`"other"`)},
	}}
	td, err := classify("ToolKind", def, &rootSchema{}, false)
	if err != nil || td.OpenEnum {
		t.Fatalf("closed enum = %+v, %v", td, err)
	}
	r := &ir{ByName: map[string]*typeDef{"ToolKind": td}, Defs: []*typeDef{td}}
	if src := string(emitTypes(r, "test", "testpkg")); !strings.Contains(src, "return unknownVariant(s, toolKindVariants)") {
		t.Fatalf("closed enum decoder missing:\n%s", src)
	}
	// v1 ToolKind has a #[serde(other)] member.
	if err := applyDecodeTables(td, 1); err != nil || td.Fallback != "other" {
		t.Fatalf("fallback = %q, %v", td.Fallback, err)
	}
	if src := string(emitTypes(r, "test", "testpkg")); !strings.Contains(src, "default:\n\t\t*v = \"other\"") {
		t.Fatalf("fallback decoder missing:\n%s", src)
	}
	// An enum with a catch-all arm is open.
	def.OneOf = append(def.OneOf, &rawSchema{Type: json.RawMessage(`"string"`)})
	if td, _ := classify("ToolKind", def, &rootSchema{}, false); !td.OpenEnum {
		t.Fatal("an enum with a catch-all arm is closed")
	}
}

func TestPrimitiveUnionsGetATypedCodec(t *testing.T) {
	root := &rootSchema{Defs: map[string]*rawSchema{
		"Option": {Type: json.RawMessage(`"object"`), Required: []string{"value"}, Properties: map[string]*rawSchema{"value": str()}},
	}}
	def := &rawSchema{AnyOf: []*rawSchema{
		str(),
		{Type: json.RawMessage(`"integer"`), Format: "int64"},
		{Type: json.RawMessage(`"array"`), Items: &rawSchema{Ref: "#/$defs/Option"}},
	}}
	td, err := classify("Value", def, root, false)
	if err != nil || td.Kind != kindAny || len(td.AnyArms) != 3 || td.AnyArms[2].GoType != "Option" {
		t.Fatalf("value = %+v, %v", td, err)
	}
	opt, _ := classify("Option", root.Defs["Option"], root, false)
	r := &ir{ByName: map[string]*typeDef{"Value": td, "Option": opt}, Defs: []*typeDef{td, opt}}
	src := string(emitTypes(r, "test", "testpkg"))
	for _, want := range []string{
		"func decodeValueUnion(raw json.RawMessage) (Value, error) {",
		"if jsonKind(raw) == '\"' {",
		"if value := new(int64); !isNull(raw) && !isNegativeZero(raw) && json.Unmarshal(raw, value) == nil {",
		"if value := []Option(nil); decodeList(raw, &value, false, false,",
		"return nil, errors.New(\"data did not match any variant of untagged enum Value\")",
		"func appendValueUnion(b []byte, v Value) ([]byte, error) {",
		"case []Option:",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("generated source missing %q:\n%s", want, src)
		}
	}
}

func TestDefaultOnNullPayloadsMatchTheRustInventory(t *testing.T) {
	r := &ir{Pin: "schema-v9.0.0", Defs: []*typeDef{
		{Name: "LogoutRequest", Kind: kindStruct, Method: "logout", Fields: []field{{JSONName: "_meta"}}},
	}}
	defaultOnNullPayloads[9] = struct{ stable, unstable []string }{stable: []string{"LogoutRequest"}}
	defer delete(defaultOnNullPayloads, 9)
	if err := r.checkDefaultOnNull(9); err != nil {
		t.Fatal(err)
	}
	r.Defs = append(r.Defs, &typeDef{Name: "NewThing", Kind: kindStruct, Method: "new", Fields: []field{{JSONName: "_meta"}}})
	if err := r.checkDefaultOnNull(9); err == nil {
		t.Fatal("a method payload without required fields outside the inventory was accepted")
	}
	r.Defs = r.Defs[:0]
	if err := r.checkDefaultOnNull(9); err == nil {
		t.Fatal("an inventory entry without its payload was accepted")
	}
}

func TestMapOnlyStructsDoNotReadArrays(t *testing.T) {
	def := &rawSchema{Type: json.RawMessage(`"object"`), Properties: map[string]*rawSchema{"toolCallId": str()}}
	td, err := classify("ToolCallUpdate", def, &rootSchema{}, false)
	if err != nil || td.ObjectOnly {
		t.Fatalf("struct = %+v, %v", td, err)
	}
	// v2 ToolCallUpdate has no flattened field and reads from an array.
	if err := applyDecodeTables(td, 2); err != nil || td.ObjectOnly {
		t.Fatal("v2 ToolCallUpdate must read from an array")
	}
	if err := applyDecodeTables(td, 1); err != nil || !td.ObjectOnly {
		t.Fatal("v1 ToolCallUpdate, which Rust flattens, must not read from an array")
	}
	// Listing a def that is not a plain struct fails.
	union := &typeDef{Name: "McpError", Kind: kindTaggedUnion}
	if err := applyDecodeTables(union, 1); err == nil {
		t.Fatal("a union in mapOnlyStructs was accepted")
	}
}

func TestPropertyOrderIsKept(t *testing.T) {
	var s rawSchema
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"cwd":{},"cursor":{},"_meta":{}}}`), &s); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.propOrder(), ","); got != "cwd,cursor,_meta" {
		t.Fatalf("order = %s", got)
	}
}

// TestPathFieldsExistInAV1Pin keeps pathFields from going stale: every entry
// must be a property of a def in the stable or unstable v1 pin.
func TestPathFieldsExistInAV1Pin(t *testing.T) {
	var roots []*rootSchema
	for _, path := range []string{"../../schema/schema.json", "../../schema/unstable/schema.json"} {
		root, err := loadSchema(path)
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, root)
	}
	for key := range pathFields {
		name, prop, _ := strings.Cut(key, ".")
		found := false
		for _, root := range roots {
			if def := root.Defs[name]; def != nil && def.Properties[prop] != nil {
				found = true
			}
		}
		if !found {
			t.Errorf("pathFields lists %s, which no v1 pin has", key)
		}
	}
}

// TestStringTagRulesNameUnionMembers requires a stringTagUnions entry to
// name the union's tag, variant fields, and tag values only.
func TestStringTagRulesNameUnionMembers(t *testing.T) {
	td := &typeDef{Name: "Value", Kind: kindTaggedUnion, TagJSON: "type", Variants: []variant{
		{Const: "id", Inline: []field{{JSONName: "value"}}},
	}}
	if err := checkStringTagRule(td, stringTagRule{Required: []string{"value", "type"}, Payload: map[string]string{"id": "x"}}); err != nil {
		t.Fatal(err)
	}
	if checkStringTagRule(td, stringTagRule{Required: []string{"other"}}) == nil {
		t.Fatal("an unknown required key was accepted")
	}
	if checkStringTagRule(td, stringTagRule{Payload: map[string]string{"boolean": "x"}}) == nil {
		t.Fatal("an unknown tag value was accepted")
	}
}
