package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestUntaggedUnionSupportsInlineObjectVariants covers the request-scoped
// MCP-over-ACP response shape: an untagged union whose branches are inline
// objects distinguished by required carrier keys ("result" versus "error").
func TestUntaggedUnionSupportsInlineObjectVariants(t *testing.T) {
	object := json.RawMessage(`"object"`)
	root := &rootSchema{Defs: map[string]*rawSchema{
		"Boom": {
			Type:       object,
			Required:   []string{"code"},
			Properties: map[string]*rawSchema{"code": {Type: json.RawMessage(`"integer"`)}},
		},
	}}
	def := &rawSchema{AnyOf: []*rawSchema{
		{
			Title:      "Result",
			Type:       object,
			Required:   []string{"result"},
			Properties: map[string]*rawSchema{"result": {}},
		},
		{
			Title:    "Error",
			Type:     object,
			Required: []string{"error"},
			Properties: map[string]*rawSchema{
				"error": {AllOf: []*rawSchema{{Ref: "#/$defs/Boom"}}},
			},
		},
	}}

	td, err := classify("Reply", def, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if td.Kind != kindUntaggedUnion {
		t.Fatalf("kind = %d, want untagged union", td.Kind)
	}
	if len(td.Variants) != 2 {
		t.Fatalf("variants = %d, want 2", len(td.Variants))
	}
	result, failure := td.Variants[0], td.Variants[1]
	if result.GoName != "Result" || result.Payload != "" || len(result.Inline) != 1 || !result.Inline[0].Required {
		t.Fatalf("result variant = %+v", result)
	}
	if failure.GoName != "Error" || failure.Payload != "" || len(failure.Inline) != 1 {
		t.Fatalf("error variant = %+v", failure)
	}
	if failure.Inline[0].GoType != "Boom" {
		t.Fatalf("error payload type = %q, want Boom", failure.Inline[0].GoType)
	}
	if probe := untaggedProbe(&ir{}, result); probe != `hasKeys(data, "result")` {
		t.Fatalf("result probe = %s", probe)
	}
	if probe := untaggedProbe(&ir{}, failure); probe != `hasKeys(data, "error")` {
		t.Fatalf("error probe = %s", probe)
	}

	boom, err := classify("Boom", root.Defs["Boom"], root, false)
	if err != nil {
		t.Fatal(err)
	}
	src := string(emitTypes(&ir{ByName: map[string]*typeDef{"Reply": td, "Boom": boom}, Defs: []*typeDef{boom, td}}, "test", "testpkg"))
	for _, want := range []string{
		"type ReplyResult struct",
		"type ReplyError struct",
		`hasKeys(data, "result")`,
		`hasKeys(data, "error")`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("generated source missing %q:\n%s", want, src)
		}
	}
}
