package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

type defKind int

const (
	kindStruct defKind = iota
	kindNewtype
	kindEnum
	kindTaggedUnion
	kindUntaggedUnion
	kindAny
	kindAlias
)

type field struct {
	JSONName string // wire key
	GoName   string // Go field name
	GoType   string // rendered type expression
	Required bool
	Doc      string
}

type variant struct {
	GoName  string // Go payload field name on the union struct
	Const   string // tag const value; "" when untagged or open
	Open    bool   // unknown-tag catch-all preserving the raw payload
	Default bool   // used when the tag key is absent on the wire
	Payload string // def name for ref payloads; "" for inline or open payloads
	Inline  []field
	Doc     string
}

type typeDef struct {
	Name     string // def name == Go type name
	Doc      string
	Kind     defKind
	Base     string // newtype/enum base ("string", "int64", "uint64", "float64")
	Fields   []field
	Variants []variant
	TagJSON  string // discriminator wire key for tagged unions
	Alias    string // alias target def name
	Side     string // x-side annotation
	Method   string // x-method annotation
}

type methodDesc struct {
	Name         string
	GoName       string
	Side         string // "agent", "client", or "protocol"
	Params       string // params def name, ""
	Result       string // result def name, ""
	Notification bool
}

type ir struct {
	Pin     string
	Defs    []*typeDef
	ByName  map[string]*typeDef
	Methods []methodDesc
}

// skippedDefs are JSON-RPC routing envelopes. The acp runtime dispatches by
// method string and never decodes these wrapper shapes; the method registry
// generated from meta.json carries the same information in usable form.
var skippedDefs = map[string]bool{
	"AgentRequest":      true,
	"AgentResponse":     true,
	"AgentNotification": true,
	"ClientResponse":    true,
}

func buildIR(root *rootSchema, meta *metaFile, pin string) (*ir, error) {
	r := &ir{Pin: pin, ByName: map[string]*typeDef{}}
	// Sort def names for deterministic output independent of JSON map order.
	names := make([]string, 0, len(root.Defs))
	for name := range root.Defs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		def := root.Defs[name]
		if skippedDefs[name] {
			continue
		}
		td, err := classify(name, def, root)
		if err != nil {
			return nil, err
		}
		td.Side, td.Method = def.Side, def.Method
		r.Defs = append(r.Defs, td)
		r.ByName[name] = td
	}
	if err := r.resolveRefs(); err != nil {
		return nil, err
	}
	if err := r.checkCollisions(); err != nil {
		return nil, err
	}
	if err := r.buildMethods(meta); err != nil {
		return nil, err
	}
	return r, nil
}

func classify(name string, def *rawSchema, root *rootSchema) (*typeDef, error) {
	td := &typeDef{Name: name, Doc: def.Description}
	switch {
	case def.Ref != "":
		return nil, fmt.Errorf("%s: unexpected top-level $ref", name)

	case len(def.OneOf) > 0 || len(def.AnyOf) > 0:
		variants := def.OneOf
		if len(variants) == 0 {
			variants = def.AnyOf
		}
		return classifyUnion(name, def, variants, root)

	case def.Properties == nil && def.Type == nil && def.soleType() == "" &&
		len(def.OneOf) == 0 && len(def.AnyOf) == 0 && len(def.AllOf) == 0:
		return td, nil // description-only marker (e.g. ExtRequest)

	case def.Properties != nil || (def.Type != nil && def.soleType() == "object"):
		if len(def.Properties) == 0 {
			return td, nil // {} marker type (e.g. LogoutCapabilities)
		}
		td.Kind = kindStruct
		required := map[string]bool{}
		for _, k := range def.Required {
			required[k] = true
		}
		for _, p := range sortedProps(def.Properties) {
			f, err := fieldFrom(p.key, p.value, root)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", name, p.key, err)
			}
			f.Required = required[p.key]
			td.Fields = append(td.Fields, f)
		}
		return td, nil

	case def.soleType() == "string":
		td.Kind, td.Base = kindNewtype, "string"
		return td, nil

	case def.soleType() == "integer":
		td.Kind, td.Base = kindNewtype, "int64"
		if def.Format == "uint64" {
			td.Base = "uint64"
		}
		return td, nil

	case def.soleType() == "number":
		td.Kind, td.Base = kindNewtype, "float64"
		return td, nil

	default:
		return nil, fmt.Errorf("%s: unsupported def shape (type=%s)", name, string(def.Type))
	}
}

func classifyUnion(name string, def *rawSchema, variants []*rawSchema, root *rootSchema) (*typeDef, error) {
	td := &typeDef{Name: name, Doc: def.Description}

	// Enum: every variant is a bare string or integer; known members carry
	// consts, a trailing non-const member makes the enum open.
	if enums, base, ok := enumVariants(variants); ok {
		if base == "integer" {
			base = "int64"
		}
		td.Kind, td.Base = kindEnum, base
		for _, e := range enums {
			if e.value == "" {
				continue // open tail: unknown values stay valid
			}
			name := goNameOf(e.title)
			if name == "" {
				name = pascal(e.value)
			}
			td.Variants = append(td.Variants, variant{Const: e.value, GoName: name, Doc: e.doc})
		}
		return td, nil
	}

	// Union of bare primitives (string|int|bool|array|null): model as any.
	if allPrimitive(variants) {
		td.Kind = kindAny
		return td, nil
	}

	// Single-variant union (e.g. AvailableCommandInput): alias the payload.
	if len(variants) == 1 {
		target := variants[0].typeName()
		if target == "" && len(variants[0].AllOf) == 1 {
			target = variants[0].AllOf[0].typeName()
		}
		if target != "" {
			td.Kind, td.Alias = kindAlias, target
			return td, nil
		}
	}

	tagged, err := taggedVariants(name, variants, root)
	if err != nil {
		return nil, err
	}
	if tagged != nil {
		td.Kind = kindTaggedUnion
		td.TagJSON = tagged.tag
		td.Variants = tagged.list
		// Shared properties lifted beside the oneOf/anyOf by schemars.
		if len(def.Properties) > 0 {
			required := map[string]bool{}
			for _, k := range def.Required {
				required[k] = true
			}
			for _, p := range sortedProps(def.Properties) {
				if p.key == tagged.tag {
					continue
				}
				f, err := fieldFrom(p.key, p.value, root)
				if err != nil {
					return nil, fmt.Errorf("%s.%s: %w", name, p.key, err)
				}
				f.Required = required[p.key]
				td.Fields = append(td.Fields, f)
			}
		}
		return td, nil
	}

	// Untagged composite union: probe-decoded sum struct. Variants are either
	// direct $refs or arrays of a $ref (e.g. flat vs grouped select options).
	td.Kind = kindUntaggedUnion
	for _, v := range variants {
		payload := v.Ref
		if payload == "" && len(v.AllOf) == 1 {
			payload = v.AllOf[0].typeName()
		}
		if payload == "" && v.soleType() == "array" && v.Items != nil {
			if target := v.Items.typeName(); target != "" {
				payload = "[]" + target
			}
		}
		if payload == "" {
			return nil, fmt.Errorf("%s: untagged variant without $ref is unsupported", name)
		}
		td.Variants = append(td.Variants, variant{GoName: goNameOf(v.Title), Payload: payload, Doc: v.Description})
	}
	return td, nil
}

type constVariant struct{ title, value, doc string }

// enumVariants reports whether every variant is a same-base string or integer,
// with at least one const member.
func enumVariants(variants []*rawSchema) ([]constVariant, string, bool) {
	var out []constVariant
	base := ""
	for _, v := range variants {
		if v.Type == nil {
			return nil, "", false
		}
		switch t := v.soleType(); t {
		case "string", "integer":
			if base != "" && base != t {
				return nil, "", false
			}
			base = t
		default:
			return nil, "", false
		}
	}
	if base == "" {
		return nil, "", false
	}
	consts := 0
	for _, v := range variants {
		var value string
		if s, ok := v.constString(); ok {
			value, consts = s, consts+1
		} else if n, ok := v.constInt(); ok {
			value, consts = fmt.Sprintf("%d", n), consts+1
		}
		out = append(out, constVariant{title: v.Title, value: value, doc: v.Description})
	}
	if consts == 0 {
		return nil, "", false
	}
	return out, base, true
}

func allPrimitive(variants []*rawSchema) bool {
	for _, v := range variants {
		switch v.soleType() {
		case "string", "integer", "number", "boolean", "array", "null":
		default:
			return false
		}
	}
	return true
}

type taggedResult struct {
	tag  string
	list []variant
}

// taggedVariants recognizes const-tagged unions: at least one variant carries
// a const discriminator property. Variants without the tag property are the
// wire default (absent tag); a tag property without a const marks the open
// catch-all for unknown tag values.
func taggedVariants(name string, variants []*rawSchema, root *rootSchema) (*taggedResult, error) {
	tag := ""
	for _, v := range variants {
		if t, ok := unionTagProp(v); ok {
			tag = t
			break
		}
	}
	if tag == "" {
		return nil, nil
	}
	var list []variant
	seenConst := map[string]bool{}
	handled := map[int]bool{}
	for i, v := range variants {
		props := v.Properties
		if props != nil {
			if _, hasTag := props[tag]; hasTag {
				continue // tagged or open variant, handled below
			}
		}
		target := v.Ref
		if target == "" && len(v.AllOf) == 1 {
			target = v.AllOf[0].typeName()
		}
		title := v.Title
		if title == "" && target != "" {
			title = target
		}
		nv := variant{GoName: goNameOf(title), Default: true, Doc: v.Description}
		if target != "" {
			if skippedDefs[target] {
				return nil, fmt.Errorf("%s: variant references skipped def %s", name, target)
			}
			nv.Payload = target
		} else if len(props) > 0 {
			required := map[string]bool{}
			for _, k := range v.Required {
				required[k] = true
			}
			for _, p := range sortedProps(props) {
				f, err := fieldFrom(p.key, p.value, root)
				if err != nil {
					return nil, fmt.Errorf("%s default variant: %w", name, err)
				}
				f.Required = required[p.key]
				nv.Inline = append(nv.Inline, f)
			}
		} else {
			return nil, fmt.Errorf("%s: default variant without payload $ref", name)
		}
		list = append(list, nv)
		handled[i] = true
	}
	for i, v := range variants {
		if handled[i] {
			continue
		}
		props := v.Properties
		if props == nil {
			return nil, fmt.Errorf("%s: tagged variant without properties", name)
		}
		tp, ok := props[tag]
		if !ok {
			return nil, fmt.Errorf("%s: variant missing tag property %q", name, tag)
		}
		cv, hasConst := tp.constString()
		if !hasConst {
			list = append(list, variant{GoName: "Other", Open: true, Doc: v.Description})
			continue
		}
		if seenConst[cv] {
			return nil, fmt.Errorf("%s: duplicate tag const %q", name, cv)
		}
		seenConst[cv] = true
		goName := v.Title
		if goName == "" {
			goName = cv
		}
		nv := variant{GoName: goNameOf(goName), Const: cv, Doc: v.Description}
		if len(v.AllOf) == 1 && v.AllOf[0].typeName() != "" {
			nv.Payload = v.AllOf[0].typeName()
			if skippedDefs[nv.Payload] {
				return nil, fmt.Errorf("%s: variant references skipped def %s", name, nv.Payload)
			}
		} else {
			required := map[string]bool{}
			for _, k := range v.Required {
				required[k] = true
			}
			for _, p := range sortedProps(props) {
				if p.key == tag {
					continue
				}
				f, err := fieldFrom(p.key, p.value, root)
				if err != nil {
					return nil, fmt.Errorf("%s inline variant: %w", name, err)
				}
				f.Required = required[p.key]
				nv.Inline = append(nv.Inline, f)
			}
		}
		list = append(list, nv)
	}
	return &taggedResult{tag: tag, list: list}, nil
}

// unionTagProp returns the property name carrying a const discriminator.
func unionTagProp(v *rawSchema) (string, bool) {
	for _, p := range sortedProps(v.Properties) {
		if _, ok := p.value.constString(); ok {
			return p.key, true
		}
	}
	return "", false
}

// fieldFrom maps one object property to a Go field.
func fieldFrom(prop string, ps *rawSchema, root *rootSchema) (field, error) {
	f := field{JSONName: prop, GoName: goNameOf(prop), Doc: ps.Description}
	inner := ps.unwrap()

	if target, ok := nullableRef(inner); ok {
		f.GoType = target
		return f, nil
	}

	switch {
	case inner.Ref != "":
		target := inner.typeName()
		if skippedDefs[target] {
			return f, fmt.Errorf("references skipped def %s", target)
		}
		f.GoType = target

	case inner.soleType() == "array" && inner.Items != nil:
		item := inner.Items.unwrap()
		if item.Ref != "" {
			target := item.typeName()
			if skippedDefs[target] {
				return f, fmt.Errorf("references skipped def %s", target)
			}
			f.GoType = "[]" + target
		} else {
			elem, err := primitiveGoType(item)
			if err != nil {
				return f, fmt.Errorf("array items: %w", err)
			}
			f.GoType = "[]" + elem
		}

	case inner.soleType() == "string":
		f.GoType = "string"

	case inner.soleType() == "integer":
		f.GoType = "int64"
		if inner.Format == "uint64" {
			f.GoType = "uint64"
		}

	case inner.soleType() == "number":
		f.GoType = "float64"

	case inner.soleType() == "boolean":
		f.GoType = "bool"

	case inner.soleType() == "object" && prop == "_meta":
		f.GoType = "Meta"

	case inner.soleType() == "object":
		if isTrue(inner.Additional) {
			f.GoType = "map[string]any"
		} else {
			var val *rawSchema
			if err := json.Unmarshal(inner.Additional, &val); err != nil || val == nil {
				return f, fmt.Errorf("unsupported object property")
			}
			val = val.unwrap()
			switch {
			case val.Ref != "":
				target := val.typeName()
				if skippedDefs[target] {
					return f, fmt.Errorf("references skipped def %s", target)
				}
				f.GoType = "map[string]" + target
			default:
				elem, err := primitiveGoType(val)
				if err != nil {
					return f, fmt.Errorf("map values: %w", err)
				}
				f.GoType = "map[string]" + elem
			}
		}

	default:
		// Typeless payloads (e.g. ExtRequest.params) are arbitrary JSON.
		f.GoType = "json.RawMessage"
	}
	return f, nil
}

// nullableRef recognizes the anyOf:[{$ref},{null}] idiom for optional
// typed fields, returning the referenced def name.
func nullableRef(s *rawSchema) (string, bool) {
	if len(s.AnyOf) != 2 {
		return "", false
	}
	a, b := s.AnyOf[0], s.AnyOf[1]
	if a.Ref != "" && b.soleType() == "null" && b.Ref == "" {
		return a.typeName(), true
	}
	if b.Ref != "" && a.soleType() == "null" && a.Ref == "" {
		return b.typeName(), true
	}
	return "", false
}

func primitiveGoType(s *rawSchema) (string, error) {
	switch t := s.soleType(); t {
	case "string":
		return "string", nil
	case "integer":
		if s.Format == "uint64" {
			return "uint64", nil
		}
		return "int64", nil
	case "number":
		return "float64", nil
	case "boolean":
		return "bool", nil
	default:
		return "", fmt.Errorf("unsupported primitive type %q", t)
	}
}

func isTrue(raw json.RawMessage) bool {
	if raw == nil {
		return false
	}
	var b bool
	return json.Unmarshal(raw, &b) == nil && b
}

func sortedProps(m map[string]*rawSchema) []propEntry {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]propEntry, 0, len(keys))
	for _, k := range keys {
		out = append(out, propEntry{key: k, value: m[k]})
	}
	return out
}

type propEntry struct {
	key   string
	value *rawSchema
}

// resolveRefs validates that every referenced def exists and is emitted.
func (r *ir) resolveRefs() error {
	for _, td := range r.Defs {
		refs := map[string]bool{}
		for _, f := range td.Fields {
			collectRefs(f.GoType, refs)
		}
		for _, v := range td.Variants {
			if v.Payload != "" {
				collectRefs(v.Payload, refs)
			}
			for _, f := range v.Inline {
				collectRefs(f.GoType, refs)
			}
		}
		if td.Alias != "" {
			refs[td.Alias] = true
		}
		for ref := range refs {
			if _, ok := r.ByName[ref]; !ok {
				return fmt.Errorf("%s references %q which is not emitted (skipped or missing)", td.Name, ref)
			}
		}
	}
	return nil
}

var builtinGoTypes = map[string]bool{
	"string": true, "int64": true, "uint64": true, "float64": true, "bool": true,
	"Meta": true, "any": true, "json.RawMessage": true, "map[string]any": true,
	"map[string]string": true, "map[string]int64": true, "map[string]uint64": true,
	"map[string]float64": true, "map[string]bool": true,
}

func collectRefs(goType string, out map[string]bool) {
	goType = strings.TrimPrefix(goType, "[]")
	if idx := strings.Index(goType, "]"); strings.HasPrefix(goType, "map[") && idx >= 0 {
		goType = goType[idx+1:]
		goType = strings.TrimPrefix(goType, "[]")
	}
	if builtinGoTypes[goType] {
		return
	}
	out[goType] = true
}

func (r *ir) checkCollisions() error {
	generated := map[string]bool{} // names the generator synthesizes
	for _, td := range r.Defs {
		if td.Kind == kindTaggedUnion {
			generated[td.Name+"Kind"] = true
			for _, v := range td.Variants {
				if len(v.Inline) > 0 {
					generated[td.Name+v.GoName] = true
				}
				if v.Open {
					generated[td.Name+"Other"] = true
				}
			}
		}
	}
	for _, td := range r.Defs {
		if generated[td.Name] {
			return fmt.Errorf("%s collides with a generated helper type", td.Name)
		}
	}
	for name := range generated {
		if _, dup := generated[name]; dup && countKey(generated, name) > 1 {
			return fmt.Errorf("duplicate generated helper %s", name)
		}
	}
	// Union payload field names must not collide with common field names.
	for _, td := range r.Defs {
		if td.Kind != kindTaggedUnion && td.Kind != kindUntaggedUnion {
			continue
		}
		seen := map[string]bool{"Kind": true}
		for _, f := range td.Fields {
			if seen[f.GoName] {
				return fmt.Errorf("%s: duplicate field %s", td.Name, f.GoName)
			}
			seen[f.GoName] = true
		}
		for _, v := range td.Variants {
			if seen[v.GoName] {
				return fmt.Errorf("%s: variant %s collides with a field name", td.Name, v.GoName)
			}
			seen[v.GoName] = true
		}
	}
	return nil
}

func countKey(m map[string]bool, key string) int {
	n := 0
	for k := range m {
		if k == key {
			n++
		}
	}
	return n
}

func (r *ir) buildMethods(meta *metaFile) error {
	type entry struct {
		side           string
		params, result string
		notification   bool
	}
	entries := map[string]*entry{}
	for _, td := range r.Defs {
		if td.Method == "" {
			continue
		}
		e := entries[td.Method]
		if e == nil {
			e = &entry{}
			entries[td.Method] = e
		}
		if td.Side != "" {
			e.side = td.Side
		}
		switch {
		case strings.HasSuffix(td.Name, "Response"):
			if e.result != "" {
				return fmt.Errorf("method %s has multiple response defs", td.Method)
			}
			e.result = td.Name
		case strings.HasSuffix(td.Name, "Notification"):
			if e.params != "" {
				return fmt.Errorf("method %s has multiple param defs", td.Method)
			}
			e.params, e.notification = td.Name, true
		default:
			if e.params != "" {
				return fmt.Errorf("method %s has multiple param defs", td.Method)
			}
			e.params = td.Name
		}
	}

	sideOf := func(method string) (string, error) {
		for _, m := range meta.AgentMethods {
			if m == method {
				return "agent", nil
			}
		}
		for _, m := range meta.ClientMethods {
			if m == method {
				return "client", nil
			}
		}
		for _, m := range meta.ProtocolMethods {
			if m == method {
				return "protocol", nil
			}
		}
		return "", fmt.Errorf("method %q not present in pinned meta.json", method)
	}

	names := make([]string, 0, len(entries))
	for m := range entries {
		names = append(names, m)
	}
	sort.Strings(names)
	for _, m := range names {
		e := entries[m]
		side, err := sideOf(m)
		if err != nil {
			return err
		}
		if e.side != "" && e.side != side {
			return fmt.Errorf("method %s: x-side %q contradicts meta.json side %q", m, e.side, side)
		}
		r.Methods = append(r.Methods, methodDesc{
			Name: m, GoName: methodGoName(m), Side: side,
			Params: e.params, Result: e.result, Notification: e.notification,
		})
	}
	// Protocol-level notifications (e.g. $/cancel_request) have typed params
	// but no per-side ownership; add any not already covered above.
	for key, m := range meta.ProtocolMethods {
		if _, ok := entries[m]; ok {
			continue
		}
		r.Methods = append(r.Methods, methodDesc{
			Name: m, GoName: methodGoName(key), Side: "protocol", Notification: true,
		})
	}
	return nil
}
