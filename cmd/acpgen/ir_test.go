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

// TestUntaggedUnionLiftsSharedProperties covers the elicitation mode shape:
// mode-specific properties declared beside an untagged anyOf of scope refs
// must become common fields, not be dropped.
func TestUntaggedUnionLiftsSharedProperties(t *testing.T) {
	object := json.RawMessage(`"object"`)
	scope := func(key string) *rawSchema {
		return &rawSchema{
			Type:       object,
			Required:   []string{key},
			Properties: map[string]*rawSchema{key: {Type: json.RawMessage(`"string"`)}},
		}
	}
	root := &rootSchema{Defs: map[string]*rawSchema{
		"SessionScope": scope("sessionId"),
		"RequestScope": scope("requestId"),
	}}
	def := &rawSchema{
		Type:     object,
		Required: []string{"url"},
		Properties: map[string]*rawSchema{
			"url":  {Type: json.RawMessage(`"string"`)},
			"note": {Type: json.RawMessage(`"string"`)},
		},
		AnyOf: []*rawSchema{
			{Title: "Session", AllOf: []*rawSchema{{Ref: "#/$defs/SessionScope"}}},
			{Title: "Request", AllOf: []*rawSchema{{Ref: "#/$defs/RequestScope"}}},
		},
	}

	td, err := classify("UrlMode", def, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if td.Kind != kindUntaggedUnion || len(td.Variants) != 2 {
		t.Fatalf("classified as %+v", td)
	}
	if len(td.Fields) != 2 || td.Fields[0].JSONName != "note" || td.Fields[0].Required ||
		td.Fields[1].JSONName != "url" || !td.Fields[1].Required {
		t.Fatalf("shared fields = %+v", td.Fields)
	}

	defs := []*typeDef{td}
	byName := map[string]*typeDef{"UrlMode": td}
	for _, name := range []string{"SessionScope", "RequestScope"} {
		scopeDef, err := classify(name, root.Defs[name], root, false)
		if err != nil {
			t.Fatal(err)
		}
		defs = append(defs, scopeDef)
		byName[name] = scopeDef
	}
	src := string(emitTypes(&ir{ByName: byName, Defs: defs}, "test", "testpkg"))
	for _, want := range []string{
		"`json:\"url\"`",
		"`json:\"note,omitempty\"`",
		"type urlModeShadow UrlMode",
		"json.Unmarshal(data, (*urlModeShadow)(v))",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("generated source missing %q:\n%s", want, src)
		}
	}
}

// TestUnionRejectsUnmodeledSharedProperties keeps the generator from silently
// dropping properties declared beside a union shape that cannot carry them.
func TestUnionRejectsUnmodeledSharedProperties(t *testing.T) {
	def := &rawSchema{
		Properties: map[string]*rawSchema{"extra": {Type: json.RawMessage(`"string"`)}},
		OneOf: []*rawSchema{
			{Type: json.RawMessage(`"string"`), Const: json.RawMessage(`"a"`)},
			{Type: json.RawMessage(`"string"`), Const: json.RawMessage(`"b"`)},
		},
	}
	_, err := classify("Letter", def, &rootSchema{}, false)
	if err == nil || !strings.Contains(err.Error(), `"extra"`) {
		t.Fatalf("classify error = %v, want unmodeled property error", err)
	}
}
