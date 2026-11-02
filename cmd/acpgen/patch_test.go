package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// In a v2 upsert, an anyOf [$ref, null] property is a patch field whose null
// differs from omission; elsewhere it keeps its pointer shape.
func TestPatchDefsKeepNullableRefsDistinct(t *testing.T) {
	object := json.RawMessage(`"object"`)
	nullableRef := func() *rawSchema {
		return &rawSchema{AnyOf: []*rawSchema{{Ref: "#/$defs/Size"}, {Type: json.RawMessage(`"null"`)}}}
	}
	properties := func() map[string]*rawSchema {
		return map[string]*rawSchema{
			"terminalId": {Type: json.RawMessage(`"string"`)},
			"size":       nullableRef(),
			"_meta":      {Type: json.RawMessage(`["object","null"]`), Additional: json.RawMessage(`true`)},
		}
	}
	root := &rootSchema{Defs: map[string]*rawSchema{
		"Size":           {Type: object, Properties: map[string]*rawSchema{"cols": {Type: json.RawMessage(`"integer"`), Format: "uint16"}}},
		"TerminalUpdate": {Type: object, Required: []string{"terminalId"}, Properties: properties()},
		"Other":          {Type: object, Required: []string{"terminalId"}, Properties: properties()},
	}}
	r, err := buildIR(root, &metaFile{ProtocolVersion: 2}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !r.ByName["TerminalUpdate"].Upsert || r.ByName["Other"].Upsert {
		t.Fatal("only patch defs are upserts")
	}
	src := string(emitTypes(r, "test", "testpkg"))
	for _, want := range []string{
		"func ApplyMetaUpdate(stored *Nullable[Meta], update Nullable[Meta])",
		"func (v *TerminalUpdate) ApplyUpdate(update TerminalUpdate)",
		"\tApplyMetaUpdate(&v.Meta, update.Meta)\n",
		"\tif update.Size.Set {\n\t\tv.Size = update.Size\n\t}\n",
		"\tv.TerminalID = update.TerminalID\n",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("generated source missing %q:\n%s", want, src)
		}
	}
	if strings.Contains(src, "func (v *Other) ApplyUpdate") {
		t.Fatal("non-patch def got ApplyUpdate")
	}
	other := r.ByName["Other"].Fields
	for _, f := range other {
		if f.JSONName == "size" && f.declType() != "*Size" {
			t.Fatalf("non-patch def size = %s", f.declType())
		}
	}

	// v1 has no patch semantics.
	r, err = buildIR(root, &metaFile{ProtocolVersion: 1}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if r.ByName["TerminalUpdate"].Upsert {
		t.Fatal("v1 def became an upsert")
	}
}

func TestUpsertRejectsFieldsWithoutNull(t *testing.T) {
	for name, td := range map[string]*typeDef{
		"pointer": {Name: "T", Fields: []field{
			{JSONName: "_meta", GoType: "Meta", Nullable: true},
			{JSONName: "size", GoType: "Size"},
		}},
		"no meta": {Name: "T", Fields: []field{{JSONName: "id", GoType: "string", Required: true}}},
	} {
		if err := checkUpsert(td); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// v1's compaction_update and session_info_update have patch fields, the
// fields Rust models with MaybeUndefined. Exactly the listed fields keep null
// distinct; the defs' other optional fields, such as session_info_update's
// _meta, and every field of other defs keep their plain optional shape, and
// v1 gets no v2 metadata patch helpers.
func TestV1PatchDefsKeepNullDistinctOnListedFieldsWithoutUpserts(t *testing.T) {
	object := json.RawMessage(`"object"`)
	properties := func() map[string]*rawSchema {
		return map[string]*rawSchema{
			"compactionId": {Type: json.RawMessage(`"string"`)},
			"summary":      {Type: json.RawMessage(`["array","null"]`), Items: &rawSchema{Type: json.RawMessage(`"string"`)}},
			"error":        {Type: json.RawMessage(`["string","null"]`)},
			"_meta":        {Type: json.RawMessage(`["object","null"]`), Additional: json.RawMessage(`true`)},
		}
	}
	info := func() map[string]*rawSchema {
		return map[string]*rawSchema{
			"title":     {Type: json.RawMessage(`["string","null"]`)},
			"updatedAt": {Type: json.RawMessage(`["string","null"]`)},
			"_meta":     {Type: json.RawMessage(`["object","null"]`), Additional: json.RawMessage(`true`)},
		}
	}
	root := &rootSchema{Defs: map[string]*rawSchema{
		"CompactionUpdate":  {Type: object, Required: []string{"compactionId"}, Properties: properties()},
		"SessionInfoUpdate": {Type: object, Properties: info()},
		"Other":             {Type: object, Required: []string{"compactionId"}, Properties: properties()},
	}}
	r, err := buildIR(root, &metaFile{ProtocolVersion: 1}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !r.HasNullable || r.PreserveNull || r.ByName["CompactionUpdate"].Upsert || r.ByName["SessionInfoUpdate"].Upsert {
		t.Fatalf("v1 patch def IR: nullable=%t preserve=%t", r.HasNullable, r.PreserveNull)
	}
	declared := func(def string) map[string]string {
		types := map[string]string{}
		for _, f := range r.ByName[def].Fields {
			types[f.JSONName] = f.declType()
		}
		return types
	}
	for def, want := range map[string]map[string]string{
		"CompactionUpdate":  {"summary": "Nullable[[]string]", "error": "Nullable[string]", "_meta": "Nullable[Meta]"},
		"SessionInfoUpdate": {"title": "Nullable[string]", "updatedAt": "Nullable[string]", "_meta": "Meta"},
		"Other":             {"summary": "[]string", "error": "*string", "_meta": "Meta"},
	} {
		got := declared(def)
		for name, typ := range want {
			if got[name] != typ {
				t.Errorf("%s.%s = %s, want %s", def, name, got[name], typ)
			}
		}
	}
	src := string(emitTypes(r, "test", "testpkg"))
	if !strings.Contains(src, "type Nullable[T any] struct") {
		t.Fatal("v1 package with a patch def lacks Nullable")
	}
	if strings.Contains(src, "ApplyMetaUpdate") || strings.Contains(src, "ApplyUpdate") {
		t.Fatal("v1 patch def got v2 metadata patch helpers")
	}
}

// A listed v1 patch field that is missing, required, or not nullable in the
// schema fails generation instead of silently staying a plain optional.
func TestV1PatchFieldsMustBeOptionalNullable(t *testing.T) {
	object := json.RawMessage(`"object"`)
	for name, def := range map[string]*rawSchema{
		"missing": {Type: object, Properties: map[string]*rawSchema{
			"title": {Type: json.RawMessage(`["string","null"]`)},
		}},
		"required": {Type: object, Required: []string{"title", "updatedAt"}, Properties: map[string]*rawSchema{
			"title":     {Type: json.RawMessage(`["string","null"]`)},
			"updatedAt": {Type: json.RawMessage(`["string","null"]`)},
		}},
		"not nullable": {Type: object, Properties: map[string]*rawSchema{
			"title":     {Type: json.RawMessage(`"string"`)},
			"updatedAt": {Type: json.RawMessage(`["string","null"]`)},
		}},
	} {
		root := &rootSchema{Defs: map[string]*rawSchema{"SessionInfoUpdate": def}}
		if _, err := buildIR(root, &metaFile{ProtocolVersion: 1}, "test"); err == nil {
			t.Errorf("%s: buildIR accepted the def", name)
		}
	}
}
