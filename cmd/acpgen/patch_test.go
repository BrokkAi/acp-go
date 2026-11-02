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
