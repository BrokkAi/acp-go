package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func str() *rawSchema { return &rawSchema{Type: json.RawMessage(`"string"`)} }

func constTag(tag, value string) *rawSchema {
	return &rawSchema{
		Type:       json.RawMessage(`"object"`),
		Required:   []string{tag},
		Properties: map[string]*rawSchema{tag: {Type: json.RawMessage(`"string"`), Const: json.RawMessage(`"` + value + `"`)}},
	}
}

// idleDef mirrors the schema-v2.0.0-alpha.13 IdleStateUpdate: a stopReason
// union flattened into the object, with an allOf payload on its error arm,
// an open custom arm, and a null "not reported" arm.
func idleDef() (*rawSchema, *rootSchema) {
	errorArm := constTag("stopReason", "error")
	errorArm.AllOf = []*rawSchema{{Ref: "#/$defs/ErrorStopReason"}}
	other := &rawSchema{Title: "other", Type: json.RawMessage(`"object"`), Required: []string{"stopReason"},
		Properties: map[string]*rawSchema{"stopReason": str()}}
	none := &rawSchema{Title: "none", Type: json.RawMessage(`"object"`),
		Properties: map[string]*rawSchema{"stopReason": {Type: json.RawMessage(`"null"`)}}}
	def := &rawSchema{
		Type: json.RawMessage(`"object"`),
		Properties: map[string]*rawSchema{
			"stopReason": {Type: json.RawMessage(`["string","null"]`), Description: "Why work stopped.", DefaultOnError: true},
			"_meta":      {Type: json.RawMessage(`["object","null"]`), Additional: json.RawMessage(`true`)},
		},
		AnyOf: []*rawSchema{constTag("stopReason", "end_turn"), errorArm, other, none},
	}
	root := &rootSchema{Defs: map[string]*rawSchema{
		"Failure": {Type: json.RawMessage(`"object"`), Required: []string{"message"},
			Properties: map[string]*rawSchema{"message": str()}},
		"ErrorStopReason": {Type: json.RawMessage(`"object"`), Properties: map[string]*rawSchema{
			"error": {AnyOf: []*rawSchema{{Ref: "#/$defs/Failure"}, {Type: json.RawMessage(`"null"`)}}},
		}},
	}}
	return def, root
}

func TestFlattenedUnionBecomesStructAndOpenEnum(t *testing.T) {
	def, root := idleDef()
	td, err := classify("IdleStateUpdate", def, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if td.Kind != kindStruct || len(td.Extra) != 1 {
		t.Fatalf("classified as %+v", td)
	}
	var names []string
	for _, f := range td.Fields {
		names = append(names, f.GoName+" "+f.GoType)
	}
	// Wire order: Rust flattens the union before the struct's own fields.
	if got := strings.Join(names, ", "); got != "StopReason StopReason, Error Failure, Meta Meta" {
		t.Fatalf("fields = %s", got)
	}
	for _, f := range td.Fields[:2] { // _meta keeps its v2 Nullable shape
		if f.Required || f.Nullable {
			t.Fatalf("field %s must be a plain optional field: %+v", f.GoName, f)
		}
	}
	enum := td.Extra[0]
	if enum.Name != "StopReason" || enum.Kind != kindEnum || enum.Base != "string" || len(enum.Variants) != 2 ||
		enum.Variants[0].GoName != "EndTurn" || enum.Variants[1].Const != "error" {
		t.Fatalf("enum = %+v", enum)
	}

	if td.Flatten == nil || td.Flatten.ExtrasGo != "StopReasonFields" || td.Fields[1].Variant != "error" {
		t.Fatalf("flatten = %+v, error field = %+v", td.Flatten, td.Fields[1])
	}
	failure, err := classify("Failure", root.Defs["Failure"], root, true)
	if err != nil {
		t.Fatal(err)
	}
	src := string(emitTypes(&ir{PreserveNull: true, ByName: map[string]*typeDef{"IdleStateUpdate": td, "Failure": failure, "StopReason": enum},
		Defs: []*typeDef{failure, td, enum}}, "test", "testpkg"))
	for _, want := range []string{
		"StopReasonFields map[string]json.RawMessage `json:\"-\"`",
		"\tcase \"error\":\n\t\t\tif v.Error != nil {",
		"appendRawMembers(b, v.StopReasonFields, []string{\"_meta\", \"stopReason\", \"usage\"})",
		"if tag, ok := decodeTag(raw, true, idleStateUpdateTags); ok {",
		"if v.decodeStopReasonVariant(raws) != nil {",
		"case \"_meta\", \"stopReason\", \"usage\":",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("generated source missing %q:\n%s", want, src)
		}
	}

	// A def already named after the tag keeps its name; the enum takes the
	// Rust type name.
	root.Defs["StopReason"] = &rawSchema{Type: json.RawMessage(`"string"`)}
	td, err = classify("IdleStateUpdate", def, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if td.Extra[0].Name != "IdleStopReason" {
		t.Fatalf("enum name = %s", td.Extra[0].Name)
	}
}

func TestFlattenedUnionRejectsUnmodeledVariants(t *testing.T) {
	def, root := idleDef()
	def.AnyOf[0].Properties["extra"] = str()
	if _, err := classify("IdleStateUpdate", def, root, true); err == nil {
		t.Fatal("variant with extra properties was accepted")
	}
}

func TestUntaggedUnionOpenArmKeepsRawValue(t *testing.T) {
	object := json.RawMessage(`"object"`)
	root := &rootSchema{Defs: map[string]*rawSchema{
		"NotFound": {Type: object, Required: []string{"sessionId"}, Properties: map[string]*rawSchema{"sessionId": str()}},
	}}
	def := &rawSchema{AnyOf: []*rawSchema{
		{Title: "SessionNotFound", AllOf: []*rawSchema{{Ref: "#/$defs/NotFound"}}},
		{Title: "Other"},
	}}
	td, err := classify("Data", def, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if td.Kind != kindUntaggedUnion || len(td.Variants) != 2 || !td.Variants[1].Open || td.Variants[1].GoName != "Other" {
		t.Fatalf("classified as %+v", td)
	}
	notFound, err := classify("NotFound", root.Defs["NotFound"], root, true)
	if err != nil {
		t.Fatal(err)
	}
	src := string(emitTypes(&ir{ByName: map[string]*typeDef{"Data": td, "NotFound": notFound}, Defs: []*typeDef{td, notFound}}, "test", "testpkg"))
	for _, want := range []string{
		"type DataOther struct{ Raw json.RawMessage }",
		"return appendRaw(b, v.Other.Raw)",
		"v.Other = &DataOther{Raw: append(json.RawMessage(nil), data...)}",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("generated source missing %q:\n%s", want, src)
		}
	}

	def.AnyOf[0], def.AnyOf[1] = def.AnyOf[1], def.AnyOf[0]
	if _, err := classify("Data", def, root, true); err == nil {
		t.Fatal("an open arm before a typed arm was accepted")
	}

	// A union of primitives with an open arm is any JSON value.
	primitive := &rawSchema{AnyOf: []*rawSchema{str(), {Title: "Other"}}}
	if td, err := classify("Value", primitive, root, true); err != nil || td.Kind != kindAny {
		t.Fatalf("primitive union = %+v, %v", td, err)
	}
}

func TestAliasAndRawMethodDefs(t *testing.T) {
	root := &rootSchema{Defs: map[string]*rawSchema{}}
	alias, err := classify("InitializeProxyRequest", &rawSchema{Method: "proxy/initialize", AllOf: []*rawSchema{{Ref: "#/$defs/InitializeRequest"}}}, root, false)
	if err != nil || alias.Kind != kindAlias || alias.Alias != "InitializeRequest" {
		t.Fatalf("alias = %+v, %v", alias, err)
	}
	raw, err := classify("ProxySuccessorResponse", &rawSchema{Method: "proxy/successor", Description: "The wrapped result."}, root, false)
	if err != nil || raw.Kind != kindRaw {
		t.Fatalf("method payload = %+v, %v", raw, err)
	}
	marker, err := classify("ExtResponse", &rawSchema{Description: "Extension response."}, root, false)
	if err != nil || marker.Kind != kindStruct || len(marker.Fields) != 0 {
		t.Fatalf("marker = %+v, %v", marker, err)
	}
}

func TestProxyMethodsGetProxySide(t *testing.T) {
	r := &ir{Defs: []*typeDef{
		{Name: "ProxySuccessorRequest", Kind: kindStruct, Side: "proxy", Method: "proxy/successor"},
		{Name: "ProxySuccessorNotification", Kind: kindStruct, Side: "proxy", Method: "proxy/successor"},
		{Name: "ProxySuccessorResponse", Kind: kindRaw, Side: "proxy", Method: "proxy/successor"},
	}}
	meta := &metaFile{ProxyMethods: map[string]string{"proxy_successor": "proxy/successor"}}
	if err := r.buildMethods(meta); err != nil {
		t.Fatal(err)
	}
	if len(r.Methods) != 1 || r.Methods[0].Side != "proxy" || r.Methods[0].NotificationParams != "ProxySuccessorNotification" {
		t.Fatalf("methods = %+v", r.Methods)
	}
	if sideConst("proxy") != "SideProxy" {
		t.Fatal("proxy side constant")
	}
	if err := r.buildMethods(&metaFile{}); err == nil {
		t.Fatal("a method missing from meta.json was accepted")
	}
}
