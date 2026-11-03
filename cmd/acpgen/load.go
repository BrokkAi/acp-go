package main

import (
	"bytes"
	"encoding/json"
)

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
	Default      json.RawMessage       `json:"default"`
	Minimum      *float64              `json:"minimum"`
	// The x-deserialize-* hints record the serde_with adapters the Rust
	// schema crate decodes a field with.
	//
	// DefaultOnError (serde_with::DefaultOnError): a value that does not
	// decode, null included, reads as the field's default.
	DefaultOnError bool `json:"x-deserialize-default-on-error"`
	// SkipInvalidItems (serde_with::VecSkipError): list items that do not
	// decode are dropped and the others kept.
	SkipInvalidItems bool `json:"x-deserialize-skip-invalid-items"`
	// DefaultOnNull (serde_with::DefaultOnNull): null reads as the field's
	// default.
	DefaultOnNull bool `json:"x-deserialize-default-on-null"`
	// PropOrder lists the property names in schema order, which is the
	// Rust struct's field declaration order (schemars keeps it). serde reads
	// a struct from a JSON array positionally in that order.
	PropOrder []string `json:"-"`
}

func (s *rawSchema) UnmarshalJSON(data []byte) error {
	type plain rawSchema
	if err := json.Unmarshal(data, (*plain)(s)); err != nil {
		return err
	}
	if s.Properties == nil {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return err
	}
	keys, err := objectKeys(top["properties"])
	s.PropOrder = keys
	return err
}

// objectKeys returns the keys of a JSON object in document order.
func objectKeys(data []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// propOrder returns the property names in schema order, falling back to
// sorted order for schemas built in code.
func (s *rawSchema) propOrder() []string {
	if len(s.PropOrder) == len(s.Properties) {
		return s.PropOrder
	}
	var keys []string
	for _, p := range sortedProps(s.Properties) {
		keys = append(keys, p.key)
	}
	return keys
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
	// ProxyMethods lists the conductor/proxy methods of the proxy-chains
	// extension (proxy/initialize, proxy/successor). They belong to neither
	// the agent nor the client API surface.
	ProxyMethods map[string]string `json:"proxyMethods"`
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

// isEmpty reports whether the schema places no constraint on the value
// (only a title or description), so it accepts any JSON value.
func (s *rawSchema) isEmpty() bool {
	return s.Ref == "" && s.Type == nil && s.Const == nil && s.Enum == nil &&
		s.Properties == nil && s.Items == nil && s.Additional == nil &&
		len(s.AllOf) == 0 && len(s.AnyOf) == 0 && len(s.OneOf) == 0 && s.Not == nil
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
