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
	kindRaw // arbitrary JSON preserved verbatim (json.RawMessage alias)
)

type field struct {
	JSONName string // wire key
	GoName   string // Go field name
	GoType   string // rendered type expression
	Required bool
	Nullable bool // JSON null is distinct from an omitted optional field
	Doc      string
	// DefaultOnError mirrors x-deserialize-default-on-error: a value that
	// does not decode, null included, reads as the field's default.
	DefaultOnError bool
	// SkipInvalid mirrors x-deserialize-skip-invalid-items: list items
	// that do not decode are dropped and the others kept.
	SkipInvalid bool
	// DefaultOnNull mirrors x-deserialize-default-on-null: null reads as
	// the field's default.
	DefaultOnNull bool
	// NullOK is set when the schema admits null for the value, as Rust's
	// Option (or a type that accepts null itself) does. Null fails any
	// other field, unless a hint reads it as the default.
	NullOK bool
	// ItemNullOK is set when the schema admits null for the items of a list
	// or the values of a map. Null fails any other item or value.
	ItemNullOK bool
	// Default is the schema's default value of the field, as JSON, or "".
	Default string
	// NonZero marks an integer Rust decodes as NonZero, rejecting 0 (see
	// nonZeroFields).
	NonZero bool
	// Variant names the flattened tag value this lifted payload field
	// belongs to; it is decoded and encoded only alongside that value.
	Variant string
	// Path marks a string field Rust decodes as a path (see pathFields),
	// which serde's errors name "path string".
	Path bool
}

// flattenInfo describes a tagged union flattened into its parent struct.
type flattenInfo struct {
	TagJSON  string   // wire key of the discriminator
	TagGo    string   // Go field name of the discriminator
	Enum     string   // synthesized open enum type
	Known    []string // const tag values
	ExtrasGo string   // Go field holding a custom variant's other fields
}

type variant struct {
	GoName  string // Go payload field name on the union struct
	Const   string // tag const value; "" when untagged or open
	Open    bool   // unknown-tag or unmatched-shape catch-all preserving the raw payload
	Default bool   // untagged: decodes any object the tagged variants do not
	Rank    int    // position in the schema, which orders untagged decoding
	// SeqOrder lists the inline fields' wire keys in Rust declaration order.
	SeqOrder []string
	// OpenAnyOf lists defs one of which an open variant's value must also
	// decode as.
	OpenAnyOf []string
	// OpenOneOf requires exactly one of OpenAnyOf to match (a oneOf).
	OpenOneOf bool
	Payload   string // def name for ref payloads; "" for inline or open payloads
	Inline    []field
	Doc       string
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
	// Extra holds types synthesized while classifying this def, such as the
	// discriminator enum of a union flattened into a struct. They are
	// emitted right after the def itself.
	Extra []*typeDef
	// Flatten is set on a struct carrying a flattened tagged union.
	Flatten *flattenInfo
	// Upsert structs get an ApplyUpdate method (see patchDefs).
	Upsert bool
	// AnySynth is the parity fixture value of a kindAny def: a value of its
	// first arm, so the fixture also decodes in the Rust schema crate.
	AnySynth string
	// AnyArms are the arms of a kindAny def, tried in order to validate a
	// value as Rust's untagged enum does.
	AnyArms []anyArm
	// OpenEnum is set on an enum with a catch-all member, which keeps any
	// string. A closed enum rejects unknown values, as Rust does, unless
	// Fallback names the member they read as (Rust's #[serde(other)]).
	OpenEnum bool
	Fallback string
	// StringTag is set on a tagged union whose tag must be a string (see
	// stringTagUnions), and StringTagRule to its entry.
	StringTag     bool
	StringTagRule stringTagRule
	// DefaultOnNull is set on a method payload the Rust crate wraps in
	// default_on_null! (see defaultOnNullPayloads).
	DefaultOnNull bool
	// FieldOrder lists a tagged union's shared fields in Rust declaration
	// order, the order serde reports the first missing one in.
	FieldOrder []string
	// SeqOrder lists a struct's wire keys in Rust declaration order, the
	// order serde reads a struct from a JSON array.
	SeqOrder []string
	// ObjectOnly is set on a struct Rust reads only from an object (a
	// flattened field; see mapOnlyStructs).
	ObjectOnly bool
}

// anyArm is one arm of a union of primitives.
type anyArm struct {
	Kind       string // "string", "integer", "number", "boolean", "null", "array", or "any"
	GoType     string // integer type, or the list item type
	ItemNullOK bool   // a list item may be null
}

// patchDefs are the v2 updates whose optional fields are patches: omission
// leaves the stored value unchanged, null clears it, and a value replaces it.
// Rust models every optional field of these with MaybeUndefined, but the
// schema writes some of them as anyOf [$ref, null], which elsewhere means that
// omission and null are equivalent. Within these defs such fields keep null
// distinct as Nullable. Upserts also get an ApplyUpdate method that applies a
// later update to stored entity state, patching the top-level _meta by key
// (see the v2 Metadata Patches rule). UsageUpdate is not an entity upsert: its
// _meta describes the notification and is not a patch.
var patchDefs = map[string]struct{ upsert bool }{
	"AgentMessage":      {upsert: true},
	"AgentThought":      {upsert: true},
	"CompactionUpdate":  {upsert: true},
	"SessionInfoUpdate": {upsert: true},
	"SessionMessage":    {upsert: true},
	"SubagentUpdate":    {upsert: true},
	"TerminalUpdate":    {upsert: true},
	"ToolCallUpdate":    {upsert: true},
	"UsageUpdate":       {},
	"UserMessage":       {upsert: true},
}

// v1PatchDefs lists, per v1 def, the optional fields that are patches. v1
// otherwise treats omission and null as equivalent, but a few of its updates
// give fields patch semantics (omission leaves the stored value unchanged,
// null clears it, and a value replaces it), and Rust models exactly those
// fields with MaybeUndefined. Only the listed fields keep null distinct as
// Nullable; the def's other fields stay plain optionals (SessionInfoUpdate's
// _meta is an Option<Meta> in Rust). v1 has no Metadata Patches rule, so
// these defs get no ApplyUpdate: a concrete _meta replaces the stored
// metadata as a whole.
var v1PatchDefs = map[string][]string{
	"CompactionUpdate":  {"summary", "error", "_meta"},
	"SessionInfoUpdate": {"title", "updatedAt"},
	// Unstable subagent updates (present only in the unstable v1 pin).
	"SessionMessage": {"senderSessionId", "recipientSessionId", "senderMessageId", "recipientMessageId", "content", "_meta"},
	"SubagentUpdate": {"title", "description", "capabilities", "state", "cwd", "additionalDirectories", "_meta"},
}

// rustEnumNames gives the Rust type name of a flattened union's enum when
// the tag-derived name is already a def (the schema does not carry it).
var rustEnumNames = map[string]string{
	"IdleStateUpdate.stopReason": "IdleStopReason",
}

// The x-deserialize-* hints cover the serde adapters of the Rust schema
// crate. The tables below record the few decode rules the schema does not
// express; everything else follows from the schema and its hints.

// nonZeroFields are the integer fields Rust decodes as a NonZero integer,
// which rejects 0, keyed by def and field, with the lowest protocol version
// where they are. The schema records them only as "minimum": 1, which it
// also writes for plain integers Rust does not range-check (v2
// TerminalSize). A listed field that is not an integer with minimum 1 fails
// generation.
var nonZeroFields = map[string]int{
	"ToolCallLocation.line": 2,
}

// flattenReservedKeys are wire keys Rust keeps out of a flattened union's
// custom-variant fields, whether or not the pin models them as fields of the
// parent (Rust's remove_idle_state_update_fields reserves usage even when
// the end-turn token usage feature is off).
var flattenReservedKeys = map[string][]string{
	"IdleStateUpdate": {"usage"},
}

// enumFallbacks are closed enums with a #[serde(other)] member, keyed by def,
// with the protocol version they apply to: an unknown value reads as that
// member instead of failing. A listed enum that is closed in the pin but has
// no such member fails generation.
var enumFallbacks = map[string]struct {
	protocol int
	value    string
}{
	"ToolKind": {1, "other"}, // v1 tool_call.rs; v2's ToolKind is open
}

// mapOnlyStructs are the plain structs whose Rust type has a
// #[serde(flatten)] field, keyed by def, with the protocol versions where it
// does. serde reads such a struct only from a JSON object, while every other
// struct also reads from a JSON array, positionally. The schema inlines
// flattened fields, so it cannot tell the two apart. Unions with shared
// fields and structs carrying a flattened union are flattened in Rust too
// and never read from an array, so they are not listed. A listed def that is
// not such a plain struct fails generation.
var mapOnlyStructs = map[string][]int{
	"McpError":                    {1, 2}, // #[serde(flatten)] data fields
	"ToolCallUpdate":              {1},    // v1 tool_call.rs: #[serde(flatten)] fields
	"WaitForTerminalExitResponse": {1},    // #[serde(flatten)] exit status
}

// defaultOnNullPayloads mirrors the Rust crate's default_on_null! inventory
// (serde_util.rs): the method payloads that decode from null as their
// default. The generated decoders read null as the zero value for every
// struct without required fields, so the generator requires these to be
// exactly the method payloads without required fields of each pin. A
// mismatch means Rust and Go would differ on a null payload.
var defaultOnNullPayloads = map[int]struct{ stable, unstable []string }{
	1: {
		stable: []string{"AuthenticateResponse", "CloseSessionResponse", "DeleteSessionResponse", "KillTerminalResponse",
			"ListSessionsRequest", "LoadSessionResponse", "LogoutRequest", "LogoutResponse", "ReleaseTerminalResponse",
			"ResumeSessionResponse", "SetSessionModeResponse", "WaitForTerminalExitResponse", "WriteTextFileResponse"},
		unstable: []string{"ArchiveSessionResponse", "AuthStatusRequest", "DisableProviderResponse", "ListProvidersRequest",
			"SetProviderResponse", "UnarchiveSessionResponse"},
	},
	2: {
		stable: []string{"CancelTerminalResponse", "CancelToolCallResponse", "CloseSessionResponse", "DeleteSessionResponse",
			"ListSessionsRequest", "LoginAuthResponse", "LogoutAuthRequest", "LogoutAuthResponse", "ResumeSessionResponse"},
		unstable: []string{"ArchiveSessionResponse", "AuthStatusRequest", "DisableProviderResponse", "ListProvidersRequest",
			"SetProviderResponse", "UnarchiveSessionResponse"},
	},
}

// checkDefaultOnNull fails when the method payloads without required fields
// differ from defaultOnNullPayloads.
func (r *ir) checkDefaultOnNull(protocol int) error {
	table, ok := defaultOnNullPayloads[protocol]
	if !ok || !strings.HasPrefix(r.Pin, "schema-v") {
		return nil // only released pins carry the Rust inventory
	}
	names := table.stable
	if strings.Contains(r.Pin, "unstable") {
		names = append(append([]string{}, names...), table.unstable...)
	}
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	for _, td := range r.Defs {
		if td.Method == "" || td.Kind != kindStruct || len(td.Fields) == 0 {
			continue
		}
		required := false
		for _, f := range td.Fields {
			required = required || f.Required
		}
		if required {
			continue
		}
		if !want[td.Name] {
			return fmt.Errorf("%s: a method payload without required fields is not in defaultOnNullPayloads", td.Name)
		}
		delete(want, td.Name)
		td.DefaultOnNull = true
	}
	missing := make([]string, 0, len(want))
	for n := range want {
		missing = append(missing, n)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("defaultOnNullPayloads lists %v, which are not method payloads without required fields", missing)
	}
	return nil
}

// stringTagUnions are tagged unions whose Rust Deserialize is hand-written
// and requires a string tag, keyed by def, with the protocol version: serde
// would otherwise read an integer tag as a variant index from buffered
// content (v2 SessionConfigOptionValue, flattened into
// SetSessionConfigOptionRequest). The hand-written Deserialize also words
// its own errors: it requires the members in Required, in that order, then
// a string tag ("`type` must be a string"), and prefixes the error of a
// known tag's payload with that tag's Payload text.
var stringTagUnions = map[string]stringTagRule{
	"SetSessionConfigOptionRequest": {
		Since:    2,
		Required: []string{"value", "type"},
		Payload: map[string]string{
			"id":      "`value` must be a string for `type: id`",
			"boolean": "`value` must be a boolean for `type: boolean`",
		},
	},
}

// stringTagRule describes a stringTagUnions entry.
type stringTagRule struct {
	Since    int
	Required []string
	Payload  map[string]string
}

// pathFields are the v1 string fields Rust decodes as a PathBuf, keyed by def
// and field: serde's errors for them expect a "path string" rather than "a
// string". The schema records a path as a plain string. v2 names the type
// AbsolutePath, so its path fields need no table. A listed field present in
// the pin that is not a string or a list of strings fails generation.
var pathFields = map[string]bool{
	"CreateTerminalRequest.cwd": true,
	"Diff.path":                 true,
	"ForkSessionRequest.additionalDirectories":   true,
	"ForkSessionRequest.cwd":                     true,
	"ListSessionsRequest.cwd":                    true,
	"LoadSessionRequest.additionalDirectories":   true,
	"LoadSessionRequest.cwd":                     true,
	"McpServerStdio.command":                     true,
	"NewSessionRequest.additionalDirectories":    true,
	"NewSessionRequest.cwd":                      true,
	"ReadTextFileRequest.path":                   true,
	"ResumeSessionRequest.additionalDirectories": true,
	"ResumeSessionRequest.cwd":                   true,
	"SessionInfo.additionalDirectories":          true,
	"SessionInfo.cwd":                            true,
	"SubagentUpdate.additionalDirectories":       true,
	"SubagentUpdate.cwd":                         true,
	"ToolCallLocation.path":                      true,
	"WriteTextFileRequest.path":                  true,
}

// flattenedEnums names the Rust enum of each union with shared fields that
// has untagged variants, keyed by def: Rust flattens that enum, which serde
// reads as untagged, beside the shared fields, so an object no variant
// decodes fails naming it. The schema inlines the enum without its name. An
// unlisted union of that shape fails generation.
var flattenedEnums = map[string]string{
	"CreateElicitationRequest":      "ElicitationMode",
	"CreateElicitationResponse":     "ElicitationAction",
	"DiffChange":                    "DiffChangeOperation",
	"SessionConfigOption":           "SessionConfigKind",
	"SetSessionConfigOptionRequest": "SessionConfigOptionValue",
}

// applyPathFields marks the def's path fields: the v1 pathFields, and in v2
// every field of type AbsolutePath.
func applyPathFields(td *typeDef, protocol int) error {
	for i := range td.Fields {
		f := &td.Fields[i]
		elem := strings.TrimPrefix(strings.TrimPrefix(f.GoType, "[]"), "*")
		switch {
		case protocol == 1 && pathFields[td.Name+"."+f.JSONName]:
			if elem != "string" {
				return fmt.Errorf("%s.%s: path field is not a string", td.Name, f.JSONName)
			}
			f.Path = true
		case protocol >= 2 && elem == "AbsolutePath":
			f.Path = true
		}
	}
	return nil
}

// applyDecodeTables applies enumFallbacks and mapOnlyStructs to a def.
func applyDecodeTables(td *typeDef, protocol int) error {
	if fb, ok := enumFallbacks[td.Name]; ok && fb.protocol == protocol && td.Kind == kindEnum && !td.OpenEnum {
		for _, v := range td.Variants {
			if v.Const == fb.value {
				td.Fallback = fb.value
			}
		}
		if td.Fallback == "" {
			return fmt.Errorf("%s: enum fallback %q is not a member", td.Name, fb.value)
		}
	}
	for _, p := range mapOnlyStructs[td.Name] {
		if p != protocol {
			continue
		}
		if td.Kind != kindStruct || td.Flatten != nil || len(td.Fields) == 0 {
			return fmt.Errorf("%s: mapOnlyStructs lists a def that is not a plain struct", td.Name)
		}
		td.ObjectOnly = true
	}
	if td.Flatten != nil {
		td.ObjectOnly = true
	}
	if rule, ok := stringTagUnions[td.Name]; ok && protocol >= rule.Since {
		if td.Kind != kindTaggedUnion {
			return fmt.Errorf("%s: string-tag union is not a tagged union", td.Name)
		}
		if err := checkStringTagRule(td, rule); err != nil {
			return err
		}
		td.StringTag, td.StringTagRule = true, rule
	}
	return nil
}

// checkStringTagRule fails when a stringTagUnions entry names a member the
// union does not have: each Required key must be its tag or a field of one
// of its variants, and each Payload key one of its tag values.
func checkStringTagRule(td *typeDef, rule stringTagRule) error {
	keys := map[string]bool{td.TagJSON: true}
	tags := map[string]bool{}
	for _, v := range td.Variants {
		if v.Const != "" {
			tags[v.Const] = true
		}
		for _, f := range v.Inline {
			keys[f.JSONName] = true
		}
	}
	for _, key := range rule.Required {
		if !keys[key] {
			return fmt.Errorf("%s: stringTagUnions requires %q, which is not the tag or a variant field", td.Name, key)
		}
	}
	for tag := range rule.Payload {
		if !tags[tag] {
			return fmt.Errorf("%s: stringTagUnions words the payload of %q, which is not a tag value", td.Name, tag)
		}
	}
	return nil
}

// applyNonZeroFields marks the def's nonZeroFields.
func applyNonZeroFields(td *typeDef, def *rawSchema, protocol int) error {
	for key, since := range nonZeroFields {
		name, prop, _ := strings.Cut(key, ".")
		if name != td.Name || protocol < since {
			continue
		}
		ps := def.Properties[prop]
		found := false
		for i := range td.Fields {
			f := &td.Fields[i]
			if f.JSONName != prop {
				continue
			}
			if ps == nil || ps.Minimum == nil || *ps.Minimum != 1 || !strings.Contains(f.GoType, "int") {
				return fmt.Errorf("%s: non-zero field is not an integer with minimum 1", key)
			}
			f.NonZero, found = true, true
		}
		if !found {
			return fmt.Errorf("%s: non-zero field is not in the schema", key)
		}
	}
	return nil
}

type methodDesc struct {
	Name               string
	GoName             string
	Side               string // "agent", "client", "both", or "protocol"
	Params             string // request/notification params def name, ""
	NotificationParams string // distinct bidirectional notification params, ""
	Result             string // result def name, ""
	Notification       bool
}

type ir struct {
	Pin          string
	PreserveNull bool
	// HasNullable is set when any generated field is Nullable, so the
	// package needs the Nullable type even without PreserveNull.
	HasNullable bool
	Defs        []*typeDef
	ByName      map[string]*typeDef
	Methods     []methodDesc
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
	// v2 introduced patch objects whose omitted and null fields can have
	// different meanings. Preserve that distinction in Go. v1 treats the two
	// states as equivalent, so its established API remains unchanged.
	preserveNull := meta.ProtocolVersion >= 2
	r := &ir{Pin: pin, PreserveNull: preserveNull, ByName: map[string]*typeDef{}}
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
		td, err := classify(name, def, root, preserveNull)
		if err != nil {
			return nil, err
		}
		if !preserveNull {
			if err := applyV1PatchFields(td, def, v1PatchDefs[name]); err != nil {
				return nil, err
			}
		}
		if err := applyNonZeroFields(td, def, meta.ProtocolVersion); err != nil {
			return nil, err
		}
		if err := applyDecodeTables(td, meta.ProtocolVersion); err != nil {
			return nil, err
		}
		if err := applyPathFields(td, meta.ProtocolVersion); err != nil {
			return nil, err
		}
		for _, f := range td.Fields {
			r.HasNullable = r.HasNullable || f.Nullable
		}
		td.Side, td.Method = def.Side, def.Method
		if preserveNull && td.Kind == kindStruct && patchDefs[name].upsert {
			if err := checkUpsert(td); err != nil {
				return nil, err
			}
			td.Upsert = true
		}
		for _, out := range append([]*typeDef{td}, td.Extra...) {
			if _, taken := r.ByName[out.Name]; taken {
				return nil, fmt.Errorf("%s: generated type name is already taken", out.Name)
			}
			r.Defs = append(r.Defs, out)
			r.ByName[out.Name] = out
		}
	}
	if err := r.checkDefaultOnNull(meta.ProtocolVersion); err != nil {
		return nil, err
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

// applyV1PatchFields makes the named optional fields of a v1 struct def
// Nullable. It fails when a listed field is missing, required, or does not
// admit null in the schema, so a schema change cannot silently drop a patch.
func applyV1PatchFields(td *typeDef, def *rawSchema, fields []string) error {
	for _, name := range fields {
		found := false
		for i := range td.Fields {
			f := &td.Fields[i]
			if f.JSONName != name {
				continue
			}
			found = true
			ps := def.Properties[name]
			_, ref := nullableRef(ps.unwrap())
			if td.Kind != kindStruct || f.Required || f.Variant != "" || !(ps.isNull() || ref) {
				return fmt.Errorf("%s.%s: v1 patch field is not an optional nullable struct field", td.Name, name)
			}
			f.Nullable = true
		}
		if !found {
			return fmt.Errorf("%s.%s: v1 patch field is not in the schema", td.Name, name)
		}
	}
	return nil
}

// checkUpsert fails when an upsert has a field ApplyUpdate cannot patch
// without losing the omitted/null/value distinction, or no top-level _meta.
func checkUpsert(td *typeDef) error {
	meta := false
	for _, f := range td.Fields {
		switch {
		case f.Variant != "":
			return fmt.Errorf("%s: upsert with a flattened union is not supported", td.Name)
		case f.JSONName == "_meta":
			if !f.Nullable || f.GoType != "Meta" {
				return fmt.Errorf("%s: upsert _meta is not a nullable Meta", td.Name)
			}
			meta = true
		case f.Required, f.Nullable, f.GoType == "json.RawMessage":
		default:
			return fmt.Errorf("%s.%s: upsert patch field %s cannot represent null", td.Name, f.JSONName, f.declType())
		}
	}
	if !meta {
		return fmt.Errorf("%s: upsert without a top-level _meta", td.Name)
	}
	return nil
}

func classify(name string, def *rawSchema, root *rootSchema, preserveNull bool) (*typeDef, error) {
	td := &typeDef{Name: name, Doc: def.Description}
	switch {
	case def.Ref != "":
		return nil, fmt.Errorf("%s: unexpected top-level $ref", name)

	case len(def.OneOf) > 0 || len(def.AnyOf) > 0:
		variants := def.OneOf
		if len(variants) == 0 {
			variants = def.AnyOf
		}
		if flat, ok, err := classifyFlattenedUnion(name, def, variants, root, preserveNull); ok || err != nil {
			return flat, err
		}
		return classifyUnion(name, def, variants, root, preserveNull)

	case def.Properties == nil && def.Type == nil && len(def.AllOf) == 1 && def.AllOf[0].typeName() != "":
		// A def that is exactly another def (e.g. InitializeProxyRequest is
		// InitializeRequest under a different method) aliases it.
		td.Kind, td.Alias = kindAlias, def.AllOf[0].typeName()
		return td, nil

	case def.Properties == nil && def.Type == nil && def.soleType() == "" &&
		len(def.OneOf) == 0 && len(def.AnyOf) == 0 && len(def.AllOf) == 0:
		if def.Method != "" {
			// A typeless method payload (e.g. ProxySuccessorResponse, the
			// wrapped request's result forwarded unchanged) is arbitrary
			// JSON that must survive decoding verbatim.
			td.Kind = kindRaw
			return td, nil
		}
		return td, nil // description-only marker (e.g. ExtRequest)

	case def.Properties != nil || (def.Type != nil && def.soleType() == "object"):
		if len(def.Properties) == 0 {
			return td, nil // {} marker type (e.g. LogoutCapabilities)
		}
		td.Kind = kindStruct
		td.SeqOrder = def.propOrder()
		required := map[string]bool{}
		for _, k := range def.Required {
			required[k] = true
		}
		for _, p := range sortedProps(def.Properties) {
			f, err := fieldFrom(p.key, p.value, root, preserveNull)
			if err != nil {
				return nil, fmt.Errorf("%s.%s: %w", name, p.key, err)
			}
			f.Required = required[p.key]
			if _, patch := patchDefs[name]; patch && preserveNull && !f.Required {
				if _, ok := nullableRef(p.value.unwrap()); ok {
					f.Nullable = true
				}
			}
			td.Fields = append(td.Fields, f)
		}
		return td, nil

	case def.soleType() == "string":
		td.Kind, td.Base = kindNewtype, "string"
		return td, nil

	case def.soleType() == "integer":
		base, err := integerGoType(def)
		if err != nil {
			return nil, err
		}
		td.Kind, td.Base = kindNewtype, base
		return td, nil

	case def.soleType() == "number":
		td.Kind, td.Base = kindNewtype, "float64"
		return td, nil

	default:
		return nil, fmt.Errorf("%s: unsupported def shape (type=%s)", name, string(def.Type))
	}
}

func classifyUnion(name string, def *rawSchema, variants []*rawSchema, root *rootSchema, preserveNull bool) (*typeDef, error) {
	td, err := classifyUnionShape(name, def, variants, root, preserveNull)
	if err != nil {
		return nil, err
	}
	// Properties declared beside the oneOf/anyOf are part of the wire shape.
	// Fail instead of generating a type that silently drops them.
	for _, p := range sortedProps(def.Properties) {
		if !td.carries(p.key) {
			return nil, fmt.Errorf("%s: property %q declared beside the union is not modeled", name, p.key)
		}
	}
	return td, nil
}

// classifyFlattenedUnion recognizes a struct with an internally tagged union
// flattened into it (a Rust #[serde(flatten)] of a #[serde(tag)] enum), such
// as IdleStateUpdate.stopReason. The def declares the tag property beside its
// anyOf, and every variant constrains only that tag: a const for each known
// variant (optionally merging a payload def via allOf), a plain string for
// the open custom/future variant, and null for "not reported".
//
// The result is a plain struct. The tag becomes an optional field typed by a
// synthesized open string enum, and each payload def's fields are lifted into
// the struct as optional fields. The enum is named after the tag property, or
// prefixed with the def name when a def of that name already exists.
func classifyFlattenedUnion(name string, def *rawSchema, variants []*rawSchema, root *rootSchema, preserveNull bool) (*typeDef, bool, error) {
	tag := ""
	for _, v := range variants {
		if t, ok := unionTagProp(v); ok {
			tag = t
			break
		}
	}
	if tag == "" || def.Properties[tag] == nil {
		return nil, false, nil
	}

	enumName := goNameOf(tag)
	if _, taken := root.Defs[enumName]; taken || enumName == name {
		enumName = rustEnumNames[name+"."+tag]
	}
	if _, taken := root.Defs[enumName]; taken || enumName == "" {
		return nil, true, fmt.Errorf("%s: no free name for the flattened %q enum; add its Rust name to rustEnumNames", name, tag)
	}
	tagDoc := def.Properties[tag].Description
	enum := &typeDef{
		Name: enumName, Kind: kindEnum, Base: "string", OpenEnum: true,
		Doc: fmt.Sprintf("%s is the %q discriminator flattened into %s. Unknown values decode unchanged.\n%s", enumName, tag, name, tagDoc),
	}

	var lifted []field
	open := false
	for _, v := range variants {
		tp := v.Properties[tag]
		if len(v.Properties) != 1 || tp == nil {
			return nil, true, fmt.Errorf("%s: flattened %q variant must constrain only the tag", name, tag)
		}
		cv, isConst := tp.constString()
		switch {
		case isConst:
			enum.Variants = append(enum.Variants, variant{Const: cv, GoName: pascal(cv), Doc: v.Description})
			if len(v.AllOf) == 0 {
				continue
			}
			target := ""
			if len(v.AllOf) == 1 {
				target = v.AllOf[0].typeName()
			}
			payload := root.Defs[target]
			if payload == nil || payload.Properties == nil || len(payload.AnyOf) > 0 || len(payload.OneOf) > 0 {
				return nil, true, fmt.Errorf("%s: flattened %q variant %q payload must be one object def", name, tag, cv)
			}
			for _, p := range sortedProps(payload.Properties) {
				f, err := fieldFrom(p.key, p.value, root, preserveNull)
				if err != nil {
					return nil, true, fmt.Errorf("%s.%s: %w", target, p.key, err)
				}
				// Payload fields exist only alongside their variant, so they
				// are optional on the flattened struct.
				f.Doc = strings.TrimSpace(f.Doc + fmt.Sprintf("\nSent only with %s %q; encoding drops it otherwise.", tag, cv))
				f.Variant = cv
				lifted = append(lifted, f)
			}
		case tp.Const == nil && tp.soleType() == "string" && !tp.isNull():
			if open {
				return nil, true, fmt.Errorf("%s: flattened %q has more than one open variant", name, tag)
			}
			open = true
		case tp.Const == nil && tp.soleType() == "null":
			// "Not reported": the same wire state as an omitted tag.
		default:
			return nil, true, fmt.Errorf("%s: unsupported flattened %q variant", name, tag)
		}
	}

	td := &typeDef{Name: name, Doc: def.Description, Kind: kindStruct, Extra: []*typeDef{enum}, SeqOrder: def.propOrder()}
	td.Flatten = &flattenInfo{TagJSON: tag, TagGo: goNameOf(tag), Enum: enumName, ExtrasGo: goNameOf(tag) + "Fields"}
	for _, v := range enum.Variants {
		td.Flatten.Known = append(td.Flatten.Known, v.Const)
	}
	required := map[string]bool{}
	for _, k := range def.Required {
		required[k] = true
	}
	for _, p := range sortedProps(def.Properties) {
		if p.key == tag {
			// Null selects the "not reported" variant, which omission also
			// selects, so the tag is a plain optional field even where null
			// is otherwise preserved.
			if required[tag] {
				return nil, true, fmt.Errorf("%s: a required flattened %q is not supported", name, tag)
			}
			td.Fields = append(td.Fields, field{
				JSONName: tag, GoName: goNameOf(tag), GoType: enumName,
				Doc: tagDoc, NullOK: true,
			})
			continue
		}
		f, err := fieldFrom(p.key, p.value, root, preserveNull)
		if err != nil {
			return nil, true, fmt.Errorf("%s.%s: %w", name, p.key, err)
		}
		f.Required = required[p.key]
		td.Fields = append(td.Fields, f)
	}
	for _, f := range lifted {
		for _, existing := range td.Fields {
			if existing.JSONName == f.JSONName || existing.GoName == f.GoName || f.GoName == td.Flatten.ExtrasGo {
				return nil, true, fmt.Errorf("%s: flattened variant field %q collides with a struct field", name, f.JSONName)
			}
		}
		td.Fields = append(td.Fields, f)
	}
	sort.SliceStable(td.Fields, func(i, j int) bool { return td.Fields[i].JSONName < td.Fields[j].JSONName })
	return td, true, nil
}

// carries reports whether the generated type encodes the given wire key as a
// field or as its discriminator.
func (td *typeDef) carries(key string) bool {
	if td.Kind == kindTaggedUnion && key == td.TagJSON {
		return true
	}
	for _, f := range td.Fields {
		if f.JSONName == key {
			return true
		}
	}
	return false
}

// sharedFields lifts properties declared beside a oneOf/anyOf (schemars puts
// common fields there) into union struct fields. skip names the tag key,
// which the union derives from the chosen variant instead.
func sharedFields(name string, def *rawSchema, skip string, root *rootSchema, preserveNull bool) ([]field, error) {
	required := map[string]bool{}
	for _, k := range def.Required {
		required[k] = true
	}
	var fields []field
	for _, p := range sortedProps(def.Properties) {
		if p.key == skip {
			continue
		}
		f, err := fieldFrom(p.key, p.value, root, preserveNull)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", name, p.key, err)
		}
		f.Required = required[p.key]
		fields = append(fields, f)
	}
	return fields, nil
}

func classifyUnionShape(name string, def *rawSchema, variants []*rawSchema, root *rootSchema, preserveNull bool) (*typeDef, error) {
	td := &typeDef{Name: name, Doc: def.Description}

	// Enum: every variant is a bare string or integer; known members carry
	// consts, a trailing non-const member makes the enum open.
	if enums, base, ok := enumVariants(variants); ok {
		if base == "integer" {
			formatted := def.Format
			for _, variant := range variants {
				if variant.Format != "" {
					formatted = variant.Format
					break
				}
			}
			var err error
			base, err = integerGoType(&rawSchema{Format: formatted})
			if err != nil {
				return nil, err
			}
		}
		td.Kind, td.Base = kindEnum, base
		for _, e := range enums {
			if e.value == "" {
				td.OpenEnum = true
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
		td.AnySynth = `""`
		for _, v := range variants {
			arm, err := anyArmOf(v, root)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			td.AnyArms = append(td.AnyArms, arm)
		}
		for _, v := range variants {
			if t := v.soleType(); t != "" && t != "null" {
				td.AnySynth = map[string]string{"string": `""`, "integer": "0", "number": "0.5", "boolean": "true", "array": "[]"}[t]
				break
			}
		}
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

	tagged, err := taggedVariants(name, variants, root, preserveNull)
	if err != nil {
		return nil, err
	}
	if tagged != nil {
		td.Kind = kindTaggedUnion
		td.TagJSON = tagged.tag
		td.Variants = tagged.list
		if td.Fields, err = sharedFields(name, def, tagged.tag, root, preserveNull); err != nil {
			return nil, err
		}
		td.FieldOrder = def.propOrder()
		return td, nil
	}

	// Untagged composite union: probe-decoded sum struct. Variants are direct
	// $refs, arrays of a $ref (e.g. flat vs grouped select options), or inline
	// object variants distinguished by their required carrier keys (e.g. a
	// result branch versus an error branch). Shared properties (e.g. the
	// elicitation form schema beside its session/request scope) become
	// common fields.
	td.Kind = kindUntaggedUnion
	if td.Fields, err = sharedFields(name, def, "", root, preserveNull); err != nil {
		return nil, err
	}
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
		nv := variant{GoName: goNameOf(v.Title), Payload: payload, Doc: v.Description}
		if payload == "" && v.isEmpty() {
			// An unconstrained arm (e.g. ErrorData's Other) accepts any
			// value no other arm matches and preserves it verbatim. It must
			// come last because it matches everything.
			if v != variants[len(variants)-1] {
				return nil, fmt.Errorf("%s: unconstrained variant must be the last one", name)
			}
			if nv.GoName != "Other" {
				return nil, fmt.Errorf("%s: unconstrained variant is titled %q, want Other", name, v.Title)
			}
			if len(td.Fields) > 0 {
				return nil, fmt.Errorf("%s: unconstrained variant beside shared fields is unsupported", name)
			}
			nv.Open = true
			td.Variants = append(td.Variants, nv)
			continue
		}
		if payload == "" {
			if v.Properties == nil && v.soleType() != "object" {
				return nil, fmt.Errorf("%s: untagged variant without $ref or object payload is unsupported", name)
			}
			if nv.GoName == "" {
				return nil, fmt.Errorf("%s: untagged inline variant is missing a title", name)
			}
			required := map[string]bool{}
			for _, k := range v.Required {
				required[k] = true
			}
			for _, p := range sortedProps(v.Properties) {
				f, err := fieldFrom(p.key, p.value, root, preserveNull)
				if err != nil {
					return nil, fmt.Errorf("%s %s variant: %w", name, nv.GoName, err)
				}
				f.Required = required[p.key]
				nv.Inline = append(nv.Inline, f)
			}
			nv.SeqOrder = withoutName(v.propOrder(), "")
		}
		td.Variants = append(td.Variants, nv)
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
		if v.isEmpty() {
			continue // an unconstrained arm accepts any JSON value
		}
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
func taggedVariants(name string, variants []*rawSchema, root *rootSchema, preserveNull bool) (*taggedResult, error) {
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
		nv := variant{GoName: goNameOf(title), Default: true, Rank: i, Doc: v.Description}
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
				f, err := fieldFrom(p.key, p.value, root, preserveNull)
				if err != nil {
					return nil, fmt.Errorf("%s default variant: %w", name, err)
				}
				f.Required = required[p.key]
				nv.Inline = append(nv.Inline, f)
			}
			nv.SeqOrder = withoutName(v.propOrder(), "")
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
			open := variant{GoName: "Other", Open: true, Rank: i, Doc: v.Description}
			// The open arm's other properties are checked, as Rust's
			// Other types require them, and the value is kept verbatim.
			required := map[string]bool{}
			for _, k := range v.Required {
				required[k] = true
			}
			for _, p := range sortedProps(props) {
				if p.key == tag {
					continue
				}
				f, err := fieldFrom(p.key, p.value, root, preserveNull)
				if err != nil {
					return nil, fmt.Errorf("%s open variant: %w", name, err)
				}
				f.Required = required[p.key]
				open.Inline = append(open.Inline, f)
			}
			// An anyOf of refs on the open arm (an elicitation scope) must
			// also match one of them.
			arms := v.AnyOf
			if len(v.OneOf) > 0 {
				arms, open.OpenOneOf = v.OneOf, true
			}
			for _, arm := range arms {
				target := arm.typeName()
				if target == "" && len(arm.AllOf) == 1 {
					target = arm.AllOf[0].typeName()
				}
				if target == "" {
					return nil, fmt.Errorf("%s: open variant anyOf arm without $ref", name)
				}
				open.OpenAnyOf = append(open.OpenAnyOf, target)
			}
			list = append(list, open)
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
		nv := variant{GoName: goNameOf(goName), Const: cv, Rank: i, Doc: v.Description}
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
				f, err := fieldFrom(p.key, p.value, root, preserveNull)
				if err != nil {
					return nil, fmt.Errorf("%s inline variant: %w", name, err)
				}
				f.Required = required[p.key]
				nv.Inline = append(nv.Inline, f)
			}
			nv.SeqOrder = withoutName(v.propOrder(), tag)
		}
		list = append(list, nv)
	}
	return &taggedResult{tag: tag, list: list}, nil
}

// withoutName returns names without skip.
func withoutName(names []string, skip string) []string {
	var out []string
	for _, n := range names {
		if n != skip {
			out = append(out, n)
		}
	}
	return out
}

// anyArmOf classifies one arm of a union of primitives.
func anyArmOf(v *rawSchema, root *rootSchema) (anyArm, error) {
	if v.isEmpty() {
		return anyArm{Kind: "any"}, nil
	}
	switch t := v.soleType(); t {
	case "string", "number", "boolean", "null":
		return anyArm{Kind: t}, nil
	case "integer":
		goType, err := integerGoType(v)
		return anyArm{Kind: t, GoType: goType}, err
	case "array":
		if v.Items == nil {
			return anyArm{Kind: "array", GoType: "any", ItemNullOK: true}, nil
		}
		item := v.Items.unwrap()
		arm := anyArm{Kind: "array", ItemNullOK: admitsNull(v.Items, root, map[string]bool{})}
		if target := item.typeName(); target != "" {
			arm.GoType = target
			return arm, nil
		}
		elem, err := primitiveGoType(item)
		arm.GoType = elem
		return arm, err
	}
	return anyArm{}, fmt.Errorf("unsupported arm of a union of primitives (type=%s)", string(v.Type))
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
func fieldFrom(prop string, ps *rawSchema, root *rootSchema, preserveNull bool) (field, error) {
	f, err := fieldType(prop, ps, root, preserveNull)
	if err != nil {
		return f, err
	}
	f.DefaultOnError, f.SkipInvalid, f.DefaultOnNull = ps.DefaultOnError, ps.SkipInvalidItems, ps.DefaultOnNull
	f.Default = string(ps.Default)
	f.NullOK = admitsNull(ps, root, map[string]bool{})
	inner := ps.unwrap()
	switch {
	case strings.HasPrefix(f.GoType, "[]"):
		f.ItemNullOK = admitsNull(inner.Items, root, map[string]bool{})
	case strings.HasPrefix(f.GoType, "map[") || f.GoType == "Meta":
		var val *rawSchema
		if isTrue(inner.Additional) || inner.Additional == nil || json.Unmarshal(inner.Additional, &val) != nil || val == nil {
			f.ItemNullOK = true // arbitrary JSON values
		} else {
			f.ItemNullOK = admitsNull(val, root, map[string]bool{})
		}
	}
	if f.SkipInvalid && !strings.HasPrefix(f.GoType, "[]") {
		return f, fmt.Errorf("x-deserialize-skip-invalid-items on %s, which is not a list", f.GoType)
	}
	return f, nil
}

// admitsNull reports whether the schema accepts JSON null: it lists the null
// type, has a null (or unconstrained) arm, references a def that does, or
// places no constraint at all. Rust decodes such a value with Option or with
// a type that accepts null itself; every other value rejects null.
func admitsNull(s *rawSchema, root *rootSchema, seen map[string]bool) bool {
	if s == nil {
		return false
	}
	if s.isNull() || s.soleType() == "null" || s.isEmpty() {
		return true
	}
	if name := s.typeName(); name != "" {
		if seen[name] {
			return false
		}
		seen[name] = true
		return admitsNull(root.Defs[name], root, seen)
	}
	for _, arm := range append(append([]*rawSchema{}, s.AnyOf...), s.OneOf...) {
		if admitsNull(arm, root, seen) {
			return true
		}
	}
	if len(s.AllOf) == 1 && s.Type == nil && s.Properties == nil {
		return admitsNull(s.AllOf[0], root, seen)
	}
	return false
}

// fieldType maps one object property to a Go field and its type.
func fieldType(prop string, ps *rawSchema, root *rootSchema, preserveNull bool) (field, error) {
	f := field{JSONName: prop, GoName: goNameOf(prop), Doc: ps.Description}
	inner := ps.unwrap()
	f.Nullable = preserveNull && ps.isNull()

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
		base, err := integerGoType(inner)
		if err != nil {
			return f, err
		}
		f.GoType = base

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
		return integerGoType(s)
	case "number":
		return "float64", nil
	case "boolean":
		return "bool", nil
	default:
		return "", fmt.Errorf("unsupported primitive type %q", t)
	}
}

func integerGoType(s *rawSchema) (string, error) {
	switch s.Format {
	case "int8":
		return "int8", nil
	case "int16":
		return "int16", nil
	case "int32":
		return "int32", nil
	case "int64", "":
		return "int64", nil
	case "uint8":
		return "uint8", nil
	case "uint16":
		return "uint16", nil
	case "uint32":
		return "uint32", nil
	case "uint64":
		return "uint64", nil
	default:
		return "", fmt.Errorf("unsupported integer format %q", s.Format)
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
	"string": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint8": true, "uint16": true, "uint32": true, "uint64": true, "float64": true, "bool": true,
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
		}
		if td.Kind == kindTaggedUnion || td.Kind == kindUntaggedUnion {
			for _, v := range td.Variants {
				if !v.Open && v.Payload == "" {
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
		sides                              map[string]bool
		params, notificationParams, result string
		notification                       bool
	}
	entries := map[string]*entry{}
	for _, td := range r.Defs {
		if td.Method == "" {
			continue
		}
		e := entries[td.Method]
		if e == nil {
			e = &entry{sides: map[string]bool{}}
			entries[td.Method] = e
		}
		if td.Side != "" {
			e.sides[td.Side] = true
		}
		switch {
		case strings.HasSuffix(td.Name, "Response"):
			if e.result != "" {
				return fmt.Errorf("method %s has multiple response defs", td.Method)
			}
			e.result = td.Name
		case strings.HasSuffix(td.Name, "Notification"):
			if e.notificationParams != "" {
				return fmt.Errorf("method %s has multiple param defs", td.Method)
			}
			e.notificationParams, e.notification = td.Name, true
		default:
			if e.params != "" {
				return fmt.Errorf("method %s has multiple param defs", td.Method)
			}
			e.params = td.Name
		}
	}

	sideOf := func(method string) (string, error) {
		agentSide, clientSide := false, false
		for _, m := range meta.AgentMethods {
			agentSide = agentSide || m == method
		}
		for _, m := range meta.ClientMethods {
			clientSide = clientSide || m == method
		}
		if agentSide {
			if clientSide {
				return "both", nil
			}
		}
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
		for _, m := range meta.ProxyMethods {
			if m == method {
				return "proxy", nil
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
		if e.params == "" {
			e.params = e.notificationParams
		} else if e.notification && e.notificationParams == "" {
			e.notificationParams = e.params
		}
		defSides := make([]string, 0, len(e.sides))
		for defSide := range e.sides {
			defSides = append(defSides, defSide)
		}
		sort.Strings(defSides)
		for _, defSide := range defSides {
			// A method both peers may call carries per-message x-side
			// annotations (the sender or owner of each payload), so agent and
			// client sides are both consistent with meta.json's "both".
			if defSide == side || (side == "both" && (defSide == "agent" || defSide == "client")) {
				continue
			}
			return fmt.Errorf("method %s: x-side %q contradicts meta.json side %q", m, defSide, side)
		}
		r.Methods = append(r.Methods, methodDesc{
			Name: m, GoName: methodGoName(m), Side: side,
			Params: e.params, NotificationParams: e.notificationParams,
			Result: e.result, Notification: e.notification,
		})
	}
	// Protocol-level notifications (e.g. $/cancel_request) have typed params
	// but no per-side ownership; add any not already covered above.
	for key, m := range meta.ProtocolMethods {
		if _, ok := entries[m]; ok {
			continue
		}
		r.Methods = append(r.Methods, methodDesc{
			Name: m, GoName: methodGoName(key), Side: "protocol",
			NotificationParams: "", Notification: true,
		})
	}
	return nil
}
