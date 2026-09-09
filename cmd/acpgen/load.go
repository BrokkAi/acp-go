package main

import "encoding/json"

// rawSchema mirrors the subset of JSON Schema draft 2020-12 that the ACP
// schema release actually uses. Anything outside this subset fails loudly in
// classification rather than silently generating wrong types.
type rawSchema struct {
	Description  string                `json:"description"`
	Title        string                `json:"title"`
	Type         json.RawMessage       `json:"type"` // string or [string, "null"]
	Format       string                `json:"format"`
	Const        json.RawMessage       `json:"const"`
	Enum         []json.RawMessage     `json:"enum"`
	Required     []string              `json:"required"`
	Properties   map[string]*rawSchema `json:"properties"`
	Items        *rawSchema            `json:"items"`
	Additional   json.RawMessage       `json:"additionalProperties"` // bool or schema
	AllOf        []*rawSchema          `json:"allOf"`
	AnyOf        []*rawSchema          `json:"anyOf"`
	OneOf        []*rawSchema          `json:"oneOf"`
	Ref          string                `json:"$ref"`
	Side         string                `json:"x-side"`
	Method       string                `json:"x-method"`
	Discriminate json.RawMessage       `json:"discriminator"` // OpenAPI-style; informational only
	Not          *rawSchema            `json:"not"`
}

type rootSchema struct {
	Title string                `json:"title"`
	AnyOf []*rawSchema          `json:"anyOf"`
	Defs  map[string]*rawSchema `json:"$defs"`
}

func loadSchema(path string) (*rootSchema, error) {
	b, err := readFile(path)
	if err != nil {
		return nil, err
	}
	var s rootSchema
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// metaFile mirrors schema/meta.json: the canonical method-name registry that
// accompanies a schema release.
type metaFile struct {
	ProtocolVersion int               `json:"version"`
	AgentMethods    map[string]string `json:"agentMethods"`
	ClientMethods   map[string]string `json:"clientMethods"`
	ProtocolMethods map[string]string `json:"protocolMethods"`
}

func loadMeta(path string) (*metaFile, error) {
	b, err := readFile(path)
	if err != nil {
		return nil, err
	}
	var m metaFile
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// typeName returns the def name a "#/$defs/X" reference points at.
func (s *rawSchema) typeName() string {
	if s.Ref == "" {
		return ""
	}
	const prefix = "#/$defs/"
	if len(s.Ref) > len(prefix) && s.Ref[:len(prefix)] == prefix {
		return s.Ref[len(prefix):]
	}
	return ""
}

// unwrap resolves the schemars artifact of a property schema being wrapped in
// allOf:[$ref], returning the referenced def name when the wrapper carries no
// constraints of its own.
func (s *rawSchema) unwrap() *rawSchema {
	for t := s; t != nil; {
		if t.Ref != "" || t.Type != nil || t.OneOf != nil || t.AnyOf != nil ||
			t.Properties != nil || t.Items != nil || t.Const != nil {
			return t
		}
		if len(t.AllOf) == 1 {
			t = t.AllOf[0]
			continue
		}
		return t
	}
	return nil
}

// isNull reports whether the type list includes "null".
func (s *rawSchema) isNull() bool {
	var list []string
	if err := json.Unmarshal(s.Type, &list); err != nil {
		return false
	}
	for _, t := range list {
		if t == "null" {
			return true
		}
	}
	return false
}

// soleType returns the single non-null JSON type, or "" when absent or unioned.
func (s *rawSchema) soleType() string {
	var single string
	if err := json.Unmarshal(s.Type, &single); err == nil {
		return single
	}
	var list []string
	if err := json.Unmarshal(s.Type, &list); err != nil {
		return ""
	}
	nonNull := ""
	for _, t := range list {
		if t == "null" {
			continue
		}
		if nonNull != "" {
			return ""
		}
		nonNull = t
	}
	return nonNull
}

// constString extracts a string const value, if present.
func (s *rawSchema) constString() (string, bool) {
	if s.Const == nil {
		return "", false
	}
	var v string
	if err := json.Unmarshal(s.Const, &v); err != nil {
		return "", false
	}
	return v, true
}

// constInt extracts an integer const value, if present.
func (s *rawSchema) constInt() (int64, bool) {
	if s.Const == nil {
		return 0, false
	}
	var v int64
	if err := json.Unmarshal(s.Const, &v); err != nil {
		return 0, false
	}
	return v, true
}
