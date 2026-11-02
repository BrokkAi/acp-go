package main

import (
	"fmt"
	"sort"
	"strings"
)

// The generated decoders read every value the way the Rust schema crate's
// serde derives and adapters do. The schema records a field's adapters as
// hints:
//
//   - x-deserialize-default-on-error (serde_with::DefaultOnError): a value
//     that does not decode reads as the field's default. For a Nullable patch
//     field (Rust's DefaultOnError<MaybeUndefined<_>>) an explicit null is
//     still kept as null; only a malformed value reads as unset, which is
//     Rust's MaybeUndefined default (Undefined). Elsewhere null reads as the
//     default too.
//   - x-deserialize-skip-invalid-items (serde_with::VecSkipError): list
//     items that do not decode are dropped and the others kept. A value that
//     is not a list still fails, unless default-on-error also applies.
//   - x-deserialize-default-on-null (serde_with::DefaultOnNull): null reads
//     as the field's default.
//
// Every other field decodes strictly, as serde does: a required field must be
// present, null is accepted only where the schema admits it, list items and
// map values must decode too, a closed enum rejects unknown values, and a
// union of primitives accepts only values of its arms. A field's default is
// its Go zero value, except that a required list or map defaults to an empty
// one, which encodes as [] or {} the way Rust encodes Default::default().
//
// Like serde, the decoders also read a struct from a JSON array, positionally
// in Rust field declaration order (unless the Rust struct flattens a field),
// and a tagged union from an array whose first element is the tag. When serde
// decodes from buffered content (inside a default-on-error field or a
// skip-invalid list item, in a tagged union's payload, in a union with
// untagged variants, or in a flattened union) it also reads a non-negative
// integer tag as the index of a tagged variant; the generated decoders track
// that context as the buffered flag.

// decl describes the generated decode methods of a type.
type declKind int

const (
	declNone   declKind = iota // encoding/json decodes it
	declFields                 // struct or tagged union: decodeJSON, decodeFields, maybe decodeSeq
	declJSON                   // untagged union: decodeJSON only
)

// curIR is the IR being emitted, for type lookups while rendering decoders.
var curIR *ir

// resolve follows aliases to the def a Go type name names.
func resolve(name string) *typeDef {
	for i := 0; i < 8; i++ {
		td := curIR.ByName[name]
		if td == nil || td.Kind != kindAlias {
			return td
		}
		name = td.Alias
	}
	return nil
}

// declOf reports which decode methods a Go type has.
func declOf(name string) declKind {
	td := resolve(name)
	switch {
	case td == nil:
		return declNone
	case td.Kind == kindStruct && len(td.Fields) > 0, td.Kind == kindTaggedUnion:
		return declFields
	case td.Kind == kindUntaggedUnion:
		return declJSON
	}
	return declNone
}

// seqCapable reports whether the type decodes from a JSON array.
func seqCapable(name string) bool {
	td := resolve(name)
	switch {
	case td == nil:
		return false
	case td.Kind == kindStruct:
		return len(td.Fields) > 0 && !td.ObjectOnly
	case td.Kind == kindTaggedUnion:
		return len(td.Fields) == 0
	}
	return false
}

// isAnyType reports whether the Go type is a union of primitives, which
// decodes through its generated validator.
func isAnyType(name string) bool {
	td := resolve(name)
	return td != nil && td.Kind == kindAny && len(td.AnyArms) > 0
}

// declType renders the Go type a field is declared with.
func (f field) declType() string {
	switch {
	case f.Nullable:
		return "Nullable[" + f.GoType + "]"
	case f.pointer():
		return "*" + f.GoType
	default:
		return f.GoType
	}
}

// defaultExpr renders the default value a hint assigns to the field.
func (f field) defaultExpr() string {
	switch {
	case f.Nullable:
		return f.declType() + "{}"
	case f.Required && (strings.HasPrefix(f.GoType, "[]") || strings.HasPrefix(f.GoType, "map[") || f.GoType == "Meta"):
		return f.GoType + "{}"
	case f.pointer() || f.isContainer():
		return "nil"
	case f.GoType == "string":
		return `""`
	case f.GoType == "bool":
		return "false"
	}
	return "*new(" + f.declType() + ")"
}

// valueCall renders the call that decodes raw into dst, a pointer to a value
// of Go type typ. itemNullOK applies to list items and map values, skip to
// list items, and cb is the buffered expression for the value.
func valueCall(typ, dst string, nullOK, itemNullOK, skip bool, cb, itemCB string) string {
	switch {
	case strings.HasPrefix(typ, "*"):
		elem := typ[1:]
		switch {
		case declOf(elem) != declNone:
			return fmt.Sprintf("decodePtr(raw, %s, %t, %s)", dst, nullOK, cb)
		case isAnyType(elem):
			return fmt.Sprintf("decodeChecked(raw, %s, %t, valid%s)", dst, nullOK, elem)
		}
		return fmt.Sprintf("decodeValue(raw, %s, %t, %s)", dst, nullOK, cb)
	case strings.HasPrefix(typ, "[]"):
		elem := typ[2:]
		return fmt.Sprintf("decodeList(raw, %s, %t, %t, func(raw json.RawMessage, dst *%s) error {\n\t\treturn %s\n\t})", dst, nullOK, skip, elem, valueCall(elem, "dst", itemNullOK, false, false, itemCB, itemCB))
	case strings.HasPrefix(typ, "map[") && typ != "map[string]any":
		elem := typ[strings.Index(typ, "]")+1:]
		return fmt.Sprintf("decodeMap(raw, %s, %t, func(raw json.RawMessage, dst *%s) error {\n\t\treturn %s\n\t})", dst, nullOK, elem, valueCall(elem, "dst", itemNullOK, false, false, itemCB, itemCB))
	case isAnyType(typ):
		return fmt.Sprintf("decodeChecked(raw, %s, %t, valid%s)", dst, nullOK, typ)
	}
	return fmt.Sprintf("decodeValue(raw, %s, %t, %s)", dst, nullOK, cb)
}

// fieldCall renders the call decoding raw into field f of v. buffered is the
// buffered expression of the enclosing value.
func fieldCall(f field, buffered string) string {
	cb, itemCB := buffered, buffered
	if f.DefaultOnError {
		cb, itemCB = "true", "true"
	}
	if f.SkipInvalid {
		itemCB = "true"
	}
	dst := "&v." + f.GoName
	switch {
	case f.Nullable && f.NonZero:
		return fmt.Sprintf("decodeNullable(raw, %s, func(raw json.RawMessage, dst *%s) error {\n\t\treturn decodeNonZero(raw, dst)\n\t})", dst, f.GoType)
	case f.Nullable:
		return fmt.Sprintf("decodeNullable(raw, %s, func(raw json.RawMessage, dst *%s) error {\n\t\treturn %s\n\t})", dst, f.GoType, valueCall(f.GoType, "dst", false, f.ItemNullOK, f.SkipInvalid, cb, itemCB))
	case f.NonZero && f.pointer():
		return fmt.Sprintf("func() error {\n\t\tif isNull(raw) {\n\t\t\treturn nil\n\t\t}\n\t\tv.%s = new(%s)\n\t\treturn decodeNonZero(raw, v.%s)\n\t}()", f.GoName, f.GoType, f.GoName)
	case f.NonZero:
		return fmt.Sprintf("decodeNonZero(raw, %s)", dst)
	}
	return valueCall(f.declType(), dst, f.NullOK, f.ItemNullOK, f.SkipInvalid, cb, itemCB)
}

// emitFieldValue writes the decoding of raw (in scope) into field f of v.
func emitFieldValue(b *strings.Builder, owner string, f field, buffered string) {
	if f.DefaultOnNull && !f.Nullable {
		fmt.Fprintf(b, "\t\tif isNull(raw) {\n\t\t\tv.%s = %s\n\t\t} else if err := %s; err != nil {\n", f.GoName, f.defaultExpr(), fieldCall(f, buffered))
	} else {
		fmt.Fprintf(b, "\t\tif err := %s; err != nil {\n", fieldCall(f, buffered))
	}
	if f.DefaultOnError {
		fmt.Fprintf(b, "\t\t\tv.%s = %s\n", f.GoName, f.defaultExpr())
	} else {
		fmt.Fprintf(b, "\t\t\treturn fmt.Errorf(\"%s.%s: %%w\", err)\n", owner, f.JSONName)
	}
	b.WriteString("\t\t}\n")
}

// emitFieldDecode writes the decoding of field f from raws into v.
func emitFieldDecode(b *strings.Builder, owner string, f field, buffered string) {
	fmt.Fprintf(b, "\tif raw, ok := raws.get(%q); ok {\n", f.JSONName)
	emitFieldValue(b, owner, f, buffered)
	b.WriteString("\t}")
	if f.Required {
		fmt.Fprintf(b, " else {\n\t\treturn missingField(%q, %q)\n\t}", owner, f.JSONName)
	}
	b.WriteString("\n")
}

// emitDecodeEntry writes UnmarshalJSON and decodeJSON of a struct or tagged
// union, which reads an object, or an array when the type reads from one.
func emitDecodeEntry(b *strings.Builder, typeName string, seq bool) {
	fmt.Fprintf(b, "func (v *%s) UnmarshalJSON(data []byte) error { return v.decodeJSON(data, false) }\n\n", typeName)
	fmt.Fprintf(b, "func (v *%s) decodeJSON(data []byte, buffered bool) error {\n", typeName)
	if seq {
		b.WriteString("\tif isArray(data) {\n\t\titems, err := splitArray(data)\n\t\tif err != nil {\n\t\t\treturn err\n\t\t}\n\t\treturn v.decodeSeq(items, buffered)\n\t}\n")
	}
	b.WriteString("\traws, err := splitObject(data)\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn v.decodeFields(data, raws, buffered)\n}\n\n")
}

// emitStructDecode writes the decoders of a struct (or of an inline union
// variant payload). A struct without fields keeps encoding/json's decoding.
func emitStructDecode(b *strings.Builder, td *typeDef, typeName string, fields []field, seqOrder []string) {
	if len(fields) == 0 {
		return
	}
	var fl *flattenInfo
	if td != nil {
		fl = td.Flatten
	}
	emitDecodeEntry(b, typeName, seqOrder != nil)
	fmt.Fprintf(b, "func (v *%s) decodeFields(data []byte, raws rawObject, buffered bool) error {\n\t*v = %s{}\n", typeName, typeName)
	for _, f := range fields {
		if f.Variant != "" || (fl != nil && f.JSONName == fl.TagJSON) {
			continue
		}
		emitFieldDecode(b, typeName, f, "buffered")
	}
	if fl != nil {
		emitFlattenedDecode(b, td)
	}
	b.WriteString("\treturn nil\n}\n\n")
	if seqOrder != nil {
		emitSeqDecode(b, typeName, fields, seqOrder)
	}
	if fl != nil {
		emitFlattenedVariantDecode(b, td)
	}
}

// emitSeqDecode writes decodeSeq, serde's visit_seq: element i is the i-th
// field in Rust declaration order. A missing element leaves an optional
// field unset and fails a required one; extra elements fail.
func emitSeqDecode(b *strings.Builder, typeName string, fields []field, order []string) {
	byName := map[string]field{}
	for _, f := range fields {
		byName[f.JSONName] = f
	}
	fmt.Fprintf(b, "func (v *%s) decodeSeq(items []json.RawMessage, buffered bool) error {\n\t*v = %s{}\n", typeName, typeName)
	for i, key := range order {
		f, ok := byName[key]
		if !ok {
			panic(fmt.Sprintf("%s: positional field %q is not a field", typeName, key))
		}
		fmt.Fprintf(b, "\tif len(items) > %d {\n\t\traw := items[%d]\n", i, i)
		emitFieldValue(b, typeName, f, "buffered")
		b.WriteString("\t}")
		if f.Required {
			fmt.Fprintf(b, " else {\n\t\treturn seqTooShort(%q, %d, %d)\n\t}", typeName, i, len(order))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(b, "\tif len(items) > %d {\n\t\treturn seqTooLong(len(items))\n\t}\n\treturn nil\n}\n\n", len(order))
}

// tagsVar names the package variable listing a union's tag values in Rust
// declaration order, which serde maps integer tags onto.
func tagsVar(name string) string { return unexport(name) + "Tags" }

// emitFlattenedDecode decodes a tagged union flattened into its parent, a
// Rust #[serde(flatten)] Option of an internally tagged enum. serde reads it
// from buffered content and reads the Option as None whenever the enum does
// not decode: a missing or invalid tag, or a known tag whose payload fields
// do not decode. A custom or future tag keeps every other field it arrived
// with.
func emitFlattenedDecode(b *strings.Builder, td *typeDef) {
	fl := td.Flatten
	fmt.Fprintf(b, "\tif raw, ok := raws.get(%q); ok {\n\t\tif tag, ok := decodeTag(raw, true, %s); ok {\n", fl.TagJSON, tagsVar(td.Name))
	fmt.Fprintf(b, "\t\t\tvalue := %s(tag)\n\t\t\tv.%s = &value\n\t\t\tif v.decode%sVariant(raws) != nil {\n", fl.Enum, fl.TagGo, fl.TagGo)
	b.WriteString("\t\t\t\t// The flattened Option reads a variant that does not decode as not reported.\n")
	fmt.Fprintf(b, "\t\t\t\tv.%s = nil\n", fl.TagGo)
	for _, f := range td.Fields {
		if f.Variant != "" {
			fmt.Fprintf(b, "\t\t\t\tv.%s = %s\n", f.GoName, f.defaultExpr())
		}
	}
	fmt.Fprintf(b, "\t\t\t\tv.%s = nil\n\t\t\t}\n\t\t}\n\t}\n", fl.ExtrasGo)
}

// emitFlattenedVariantDecode writes the helper decoding the fields of the
// flattened variant the tag selects, and the tag list.
func emitFlattenedVariantDecode(b *strings.Builder, td *typeDef) {
	fl := td.Flatten
	fmt.Fprintf(b, "var %s = []string{%s}\n\n", tagsVar(td.Name), quoteList(fl.Known))
	fmt.Fprintf(b, "// decode%sVariant decodes the fields of the flattened %s variant.\n", fl.TagGo, fl.TagJSON)
	fmt.Fprintf(b, "func (v *%s) decode%sVariant(raws rawObject) error {\n\tswitch *v.%s {\n", td.Name, fl.TagGo, fl.TagGo)
	for _, known := range fl.Known {
		fmt.Fprintf(b, "\tcase %q:\n", known)
		for _, f := range td.Fields {
			if f.Variant == known {
				emitFieldDecode(b, td.Name, f, "true")
			}
		}
	}
	fmt.Fprintf(b, "\tdefault:\n\t\tfor _, m := range raws {\n\t\t\tswitch key, raw := string(m.key), m.raw; key {\n\t\t\tcase %s:\n\t\t\tdefault:\n\t\t\t\tif v.%s == nil {\n\t\t\t\t\tv.%s = map[string]json.RawMessage{}\n\t\t\t\t}\n\t\t\t\tv.%s[key] = raw\n\t\t\t}\n\t\t}\n\t}\n", quoteList(sharedKeys(td)), fl.ExtrasGo, fl.ExtrasGo, fl.ExtrasGo)
	b.WriteString("\treturn nil\n}\n\n")
}

// payloadFieldsCall renders the decoding of a union variant payload from an
// object already split into raws.
func payloadFieldsCall(v variant, payloadType, payload, data, raws string) string {
	if v.Payload == "" {
		if len(v.Inline) == 0 {
			return "nil" // a payload without fields takes any other members
		}
		return fmt.Sprintf("%s.decodeFields(%s, %s, true)", payload, data, raws)
	}
	switch declOf(payloadType) {
	case declFields:
		return fmt.Sprintf("%s.decodeFields(%s, %s, true)", payload, data, raws)
	case declJSON:
		return fmt.Sprintf("%s.decodeJSON(objectData(%s, %s), true)", payload, data, raws)
	}
	return fmt.Sprintf("json.Unmarshal(objectData(%s, %s), %s)", data, raws, payload)
}

// payloadSeqCall renders the decoding of a union variant payload from the
// elements of an array.
func payloadSeqCall(v variant, payloadType, payload, items string) string {
	if v.Payload == "" {
		if len(v.Inline) == 0 {
			return fmt.Sprintf("seqEmpty(%s)", items)
		}
		return fmt.Sprintf("%s.decodeSeq(%s, true)", payload, items)
	}
	if td := resolve(payloadType); td != nil && td.Kind == kindStruct && len(td.Fields) == 0 {
		return fmt.Sprintf("seqEmpty(%s)", items)
	}
	switch {
	case seqCapable(payloadType):
		return fmt.Sprintf("%s.decodeSeq(%s, true)", payload, items)
	case declOf(payloadType) == declJSON:
		return fmt.Sprintf("%s.decodeJSON(arrayData(%s), true)", payload, items)
	}
	return "errSeq"
}

// emitTaggedUnionDecode writes the decoders of a tagged union the way serde
// decodes an internally tagged enum whose catch-all variants are untagged: a
// tag naming a variant selects it, and whatever it does not decode (a
// missing, invalid, or unknown tag, or a payload that does not decode) is
// tried against the untagged variants in schema order. The open variant
// takes a string tag that names no variant (only from an object); a variant
// without a tag takes any value its payload decodes. A union with untagged
// variants is decoded from buffered content as a whole; a variant payload
// always is.
func emitTaggedUnionDecode(b *strings.Builder, r *ir, td *typeDef) {
	kindType := td.Name + "Kind"
	var untagged []variant
	var known []string
	for _, v := range td.Variants {
		if v.Open || v.Default {
			untagged = append(untagged, v)
		}
	}
	consts := append([]variant(nil), td.Variants...)
	sort.SliceStable(consts, func(i, j int) bool { return consts[i].Rank < consts[j].Rank })
	for _, v := range consts {
		if v.Const != "" {
			known = append(known, v.Const)
		}
	}
	sort.SliceStable(untagged, func(i, j int) bool { return untagged[i].Rank < untagged[j].Rank })
	seq := len(td.Fields) == 0
	fmt.Fprintf(b, "var %s = []string{%s}\n\n", tagsVar(td.Name), quoteList(known))
	emitDecodeEntry(b, td.Name, seq)

	for _, form := range []string{"fields", "seq"} {
		if form == "seq" && !seq {
			continue
		}
		if form == "fields" {
			fmt.Fprintf(b, "func (v *%s) decodeFields(data []byte, raws rawObject, buffered bool) error {\n\t*v = %s{}\n", td.Name, td.Name)
		} else {
			fmt.Fprintf(b, "func (v *%s) decodeSeq(items []json.RawMessage, buffered bool) error {\n\t*v = %s{}\n", td.Name, td.Name)
		}
		if len(untagged) > 0 && len(td.Fields) == 0 {
			b.WriteString("\tbuffered = true\n")
		}
		if form == "fields" {
			for _, f := range td.Fields {
				emitFieldDecode(b, td.Name, f, "buffered")
			}
			if len(td.Fields) > 0 {
				// Rust flattens the enum beside the shared fields, so it
				// decodes from buffered content.
				b.WriteString("\tbuffered = true\n")
			}
			fmt.Fprintf(b, "\traw, present := raws.get(%q)\n", td.TagJSON)
		} else {
			b.WriteString("\tpresent := len(items) > 0\n\tvar raw json.RawMessage\n\tif present {\n\t\traw = items[0]\n\t}\n")
		}
		if len(untagged) == 0 {
			fmt.Fprintf(b, "\tif !present {\n\t\treturn missingField(%q, %q)\n\t}\n", td.Name, td.TagJSON)
		}
		tagBuffered := "buffered"
		if td.StringTag {
			tagBuffered = "false"
		}
		fmt.Fprintf(b, "\ttag, tagged := \"\", false\n\tif present {\n\t\ttag, tagged = decodeTag(raw, %s, %s)\n\t}\n", tagBuffered, tagsVar(td.Name))
		if len(untagged) == 0 {
			fmt.Fprintf(b, "\tif !tagged {\n\t\treturn fmt.Errorf(\"%s: invalid %s tag %%s\", raw)\n\t}\n", td.Name, td.TagJSON)
		}
		b.WriteString("\tvar err error\n\tif tagged {\n\t\tswitch tag {\n")
		for _, v := range consts {
			if v.Const == "" {
				continue
			}
			pt := payloadType(r, td, v)
			var call string
			if form == "fields" {
				data, raws := "data", "raws"
				if v.Payload != "" && capturesExtras(r, v.Payload, map[string]bool{}) {
					data, raws = "nil", fmt.Sprintf("withoutKey(raws, %q)", td.TagJSON)
				}
				call = payloadFieldsCall(v, pt, "payload", data, raws)
			} else {
				call = payloadSeqCall(v, pt, "payload", "items[1:]")
			}
			fmt.Fprintf(b, "\t\tcase %q:\n\t\t\tpayload := new(%s)\n\t\t\tif err = %s; err == nil {\n\t\t\t\tv.Kind, v.%s = %s%s, payload\n\t\t\t\treturn nil\n\t\t\t}\n", v.Const, pt, call, v.GoName, kindType, pascal(v.Const))
		}
		if len(untagged) == 0 {
			fmt.Fprintf(b, "\t\tdefault:\n\t\t\treturn fmt.Errorf(\"%s: unknown %s tag %%q\", tag)\n\t\t}\n\t}\n\treturn err\n}\n\n", td.Name, td.TagJSON)
			continue
		}
		b.WriteString("\t\t}\n\t}\n")
		for _, v := range untagged {
			if v.Open {
				if form == "seq" {
					continue // the open variant reads only an object
				}
				b.WriteString("\tif tagged")
				for _, k := range known {
					fmt.Fprintf(b, " && tag != %q", k)
				}
				b.WriteString(" {\n")
				if len(v.Inline) > 0 {
					fmt.Fprintf(b, "\t\tif e := new(%s).decodeFields(data, raws, true); e != nil {\n\t\t\treturn fmt.Errorf(\"%s: data matches no variant: %%w\", e)\n\t\t}\n", otherFieldsType(td), td.Name)
				}
				if len(v.OpenAnyOf) > 0 {
					// The open value must also match one of the arms (exactly
					// one for a oneOf).
					b.WriteString("\t\tmatched := 0\n")
					for _, ref := range v.OpenAnyOf {
						fmt.Fprintf(b, "\t\tif %s == nil {\n\t\t\tmatched++\n\t\t}\n", payloadFieldsCall(variant{Payload: ref}, ref, "new("+ref+")", "data", "raws"))
					}
					cond := "matched == 0"
					if v.OpenOneOf {
						cond = "matched != 1"
					}
					fmt.Fprintf(b, "\t\tif %s {\n\t\t\treturn fmt.Errorf(\"%s: data matches no variant\")\n\t\t}\n", cond, td.Name)
				}
				fmt.Fprintf(b, "\t\tv.Kind = %s(tag)\n\t\tv.Other = &%sOther{Raw: objectData(data, raws)}\n\t\treturn nil\n\t}\n", kindType, td.Name)
				continue
			}
			pt := payloadType(r, td, v)
			call := payloadFieldsCall(v, pt, "payload"+v.GoName, "data", "raws")
			if form == "seq" {
				call = payloadSeqCall(v, pt, "payload"+v.GoName, "items")
			}
			fmt.Fprintf(b, "\tpayload%s := new(%s)\n\tif e := %s; e == nil {\n\t\tv.%s = payload%s\n\t\treturn nil\n\t} else if err == nil {\n\t\terr = e\n\t}\n", v.GoName, pt, call, v.GoName, v.GoName)
		}
		fmt.Fprintf(b, "\tif err != nil {\n\t\treturn fmt.Errorf(\"%s: data matches no variant: %%w\", err)\n\t}\n", td.Name)
		fmt.Fprintf(b, "\tif !present {\n\t\treturn missingField(%q, %q)\n\t}\n", td.Name, td.TagJSON)
		fmt.Fprintf(b, "\treturn fmt.Errorf(\"%s: unknown %s tag %%s\", raw)\n}\n\n", td.Name, td.TagJSON)
	}
}

// otherFieldsType names the unexported struct that checks the fields of a
// tagged union's open variant.
func otherFieldsType(td *typeDef) string { return unexport(td.Name) + "OtherFields" }

// emitOtherFields writes the struct that checks the fields of a tagged
// union's open variant, which Rust's Other type requires.
func emitOtherFields(b *strings.Builder, td *typeDef) {
	for _, v := range td.Variants {
		if !v.Open || len(v.Inline) == 0 {
			continue
		}
		name := otherFieldsType(td)
		fmt.Fprintf(b, "// %s checks the fields an open %s variant must carry.\ntype %s struct {\n", name, td.Name, name)
		for _, f := range v.Inline {
			fmt.Fprintf(b, "\t%s\n", f.goDecl())
		}
		b.WriteString("}\n\n")
		emitStructDecode(b, nil, name, v.Inline, nil)
	}
}

// emitUntaggedUnionDecode writes the decoder of an untagged union, which
// serde decodes from buffered content, trying each variant in order.
func emitUntaggedUnionDecode(b *strings.Builder, r *ir, td *typeDef) {
	fmt.Fprintf(b, "func (v *%s) UnmarshalJSON(data []byte) error { return v.decodeJSON(data, false) }\n\n", td.Name)
	fmt.Fprintf(b, "func (v *%s) decodeJSON(data []byte, buffered bool) error {\n\t*v = %s{}\n", td.Name, td.Name)
	b.WriteString("\traws, err := splitObject(data)\n\tobject := err == nil && raws != nil\n")
	if len(td.Fields) > 0 {
		fmt.Fprintf(b, "\tif !object {\n\t\treturn fmt.Errorf(\"%s: expected an object\")\n\t}\n", td.Name)
		for _, f := range td.Fields {
			emitFieldDecode(b, td.Name, f, "buffered")
		}
	}
	b.WriteString("\terr = nil\n")
	open := hasOpenVariant(td)
	for _, v := range td.Variants {
		if v.Open {
			continue // classification keeps the open variant last
		}
		pt := payloadType(r, td, v)
		var call string
		switch {
		case strings.HasPrefix(v.Payload, "[]"):
			elem := strings.TrimPrefix(v.Payload, "[]")
			call = fmt.Sprintf("decodeList(data, payload, false, false, func(raw json.RawMessage, dst *%s) error {\n\t\treturn %s\n\t})", elem, valueCall(elem, "dst", false, false, false, "true", "true"))
		case declOf(pt) == declFields:
			call = fmt.Sprintf("func() error {\n\t\tif object {\n\t\t\treturn payload.decodeFields(data, raws, true)\n\t\t}\n\t\treturn payload.decodeJSON(data, true)\n\t}()")
		case declOf(pt) == declJSON:
			call = "payload.decodeJSON(data, true)"
		default:
			call = "json.Unmarshal(data, payload)"
		}
		probe := untaggedProbe(r, v)
		guard := "true"
		if probe != "true" && !strings.HasPrefix(v.Payload, "[]") {
			guard = "!object || " + probe
		}
		fmt.Fprintf(b, "\tif %s {\n\t\tpayload := new(%s)\n\t\tif err = %s; err == nil {\n\t\t\tv.%s = payload\n", guard, pt, call, v.GoName)
		if open {
			fmt.Fprintf(b, "\t\t\tv.raw, v.rawVariant = append(json.RawMessage(nil), data...), %q\n", v.GoName)
		}
		b.WriteString("\t\t\treturn nil\n\t\t}\n\t}\n")
	}
	if open {
		fmt.Fprintf(b, "\tv.Other = &%sOther{Raw: append(json.RawMessage(nil), data...)}\n\treturn nil\n}\n\n", td.Name)
		return
	}
	fmt.Fprintf(b, "\tif err != nil {\n\t\treturn fmt.Errorf(\"%s: payload matches no variant: %%w\", err)\n\t}\n\treturn fmt.Errorf(\"%s: payload matches no variant\")\n}\n\n", td.Name, td.Name)
}

// untaggedProbe renders the shape test guarding one untagged variant read
// from an object: its required keys. A variant without them cannot decode,
// so the probe only saves a decode attempt.
func untaggedProbe(r *ir, v variant) string {
	var keys []string
	if v.Payload != "" {
		if td := resolve(v.Payload); td != nil && td.Kind == kindStruct {
			for _, f := range td.Fields {
				if f.Required {
					keys = append(keys, f.JSONName)
				}
			}
		}
	} else {
		for _, f := range v.Inline {
			if f.Required {
				keys = append(keys, f.JSONName)
			}
		}
	}
	if len(keys) == 0 {
		return "true"
	}
	return fmt.Sprintf("hasKeys(raws, %s)", quoteList(keys))
}

// emitEnumDecode writes the decoder of a string enum, as serde decodes a
// unit-variant enum: a string, or the one-key map form {"variant": null}.
// A closed enum rejects a value it does not name, unless it has a
// #[serde(other)] member; an open enum (Rust's untagged Known | Other(String))
// keeps any string, but the map form only for a known variant.
func emitEnumDecode(b *strings.Builder, td *typeDef) {
	if td.Base != "string" || len(td.Variants) == 0 {
		return
	}
	cases := make([]string, 0, len(td.Variants))
	for _, v := range td.Variants {
		cases = append(cases, td.Name+v.GoName)
	}
	switch {
	case td.OpenEnum:
		b.WriteString("// UnmarshalJSON keeps any string, as Rust's open enum does, and reads the\n// map form {\"variant\": null} of a known variant.\n")
	case td.Fallback != "":
		fmt.Fprintf(b, "// UnmarshalJSON reads a member as a string or in the map form\n// {\"variant\": null}, as Rust's closed enum does; an unknown value reads as\n// %q (#[serde(other)]).\n", td.Fallback)
	default:
		b.WriteString("// UnmarshalJSON reads a member as a string or in the map form\n// {\"variant\": null}, and rejects any other value, as Rust's closed enum does.\n")
	}
	fmt.Fprintf(b, "func (v *%s) UnmarshalJSON(data []byte) error {\n\ts, mapForm, err := enumValue(data)\n\tif err != nil {\n\t\treturn fmt.Errorf(\"%s: %%w\", err)\n\t}\n", td.Name, td.Name)
	fmt.Fprintf(b, "\tswitch %s(s) {\n\tcase %s:\n\t\t*v = %s(s)\n\tdefault:\n", td.Name, strings.Join(cases, ", "), td.Name)
	switch {
	case td.Fallback != "":
		fmt.Fprintf(b, "\t\t*v = %q\n", td.Fallback)
	case td.OpenEnum:
		fmt.Fprintf(b, "\t\tif mapForm {\n\t\t\treturn fmt.Errorf(\"%s: unknown variant %%q\", s)\n\t\t}\n\t\t*v = %s(s)\n", td.Name, td.Name)
	default:
		fmt.Fprintf(b, "\t\treturn fmt.Errorf(\"%s: unknown variant %%q\", s)\n", td.Name)
	}
	b.WriteString("\t}\n")
	if !td.OpenEnum {
		b.WriteString("\t_ = mapForm\n")
	}
	b.WriteString("\treturn nil\n}\n\n")
	fmt.Fprintf(b, "func (v *%s) decodeJSON(data []byte, _ bool) error { return v.UnmarshalJSON(data) }\n\n", td.Name)
}

// emitAnyValidator writes the validator of a union of primitives: a value
// must match one of its arms, as Rust's untagged enum requires.
func emitAnyValidator(b *strings.Builder, td *typeDef) {
	if len(td.AnyArms) == 0 {
		return
	}
	fmt.Fprintf(b, "// valid%s reports whether raw matches an arm of %s.\nfunc valid%s(raw json.RawMessage) error {\n", td.Name, td.Name, td.Name)
	for _, arm := range td.AnyArms {
		var cond string
		switch arm.Kind {
		case "any":
			cond = "true"
		case "null":
			cond = "isNull(raw)"
		case "string":
			cond = `jsonKind(raw) == '"'`
		case "boolean":
			cond = `string(raw) == "true" || string(raw) == "false"`
		case "number":
			cond = `jsonKind(raw) == '-' || (jsonKind(raw) >= '0' && jsonKind(raw) <= '9')`
		case "integer":
			cond = fmt.Sprintf("!isNull(raw) && json.Unmarshal(raw, new(%s)) == nil", arm.GoType)
		case "array":
			cond = fmt.Sprintf("decodeList(raw, new([]%s), false, false, func(raw json.RawMessage, dst *%s) error {\n\t\treturn %s\n\t}) == nil", arm.GoType, arm.GoType, valueCall(arm.GoType, "dst", arm.ItemNullOK, false, false, "true", "true"))
		}
		fmt.Fprintf(b, "\tif %s {\n\t\treturn nil\n\t}\n", cond)
	}
	fmt.Fprintf(b, "\treturn fmt.Errorf(\"%s: data did not match any variant\")\n}\n\n", td.Name)
}

// emitFlattenedCodec writes the encoder of a struct carrying a flattened
// tagged union: a lifted variant payload field is encoded only alongside its
// own tag value, and a custom or future tag re-encodes the other fields it
// arrived with.
func emitFlattenedCodec(b *strings.Builder, td *typeDef) {
	if td.Flatten == nil {
		return
	}
	shadow := unexport(td.Name) + "Shadow"
	fmt.Fprintf(b, "// %s mirrors %s without its codec methods.\ntype %s %s\n\n", shadow, td.Name, shadow, td.Name)
	emitFlattenedMarshal(b, td, shadow)
}

// sharedKeys lists the wire keys a flattened struct owns regardless of its
// tag value: every field except lifted variant payloads, and the keys Rust
// reserves for it (flattenReservedKeys).
func sharedKeys(td *typeDef) []string {
	var keys []string
	seen := map[string]bool{}
	for _, f := range td.Fields {
		if f.Variant == "" {
			keys, seen[f.JSONName] = append(keys, f.JSONName), true
		}
	}
	for _, key := range flattenReservedKeys[td.Name] {
		if !seen[key] {
			keys, seen[key] = append(keys, key), true
		}
	}
	sort.Strings(keys)
	return keys
}

// hasNonZero reports whether any generated field is a NonZero integer.
func (r *ir) hasNonZero() bool {
	for _, td := range r.Defs {
		for _, f := range td.Fields {
			if f.NonZero {
				return true
			}
		}
	}
	return false
}

func emitFlattenedMarshal(b *strings.Builder, td *typeDef, shadow string) {
	fl := td.Flatten
	fmt.Fprintf(b, "func (v %s) MarshalJSON() ([]byte, error) {\n", td.Name)
	fmt.Fprintf(b, "\tencoded, err := json.Marshal(%s(v))\n\tif err != nil {\n\t\treturn nil, err\n\t}\n", shadow)
	fmt.Fprintf(b, "\tvar obj map[string]json.RawMessage\n\tif err := json.Unmarshal(encoded, &obj); err != nil {\n\t\treturn nil, err\n\t}\n")
	fmt.Fprintf(b, "\tvar tag %s\n\tif v.%s != nil {\n\t\ttag = *v.%s\n\t}\n", fl.Enum, fl.TagGo, fl.TagGo)
	for _, f := range td.Fields {
		if f.Variant != "" {
			fmt.Fprintf(b, "\tif tag != %q {\n\t\tdelete(obj, %q)\n\t}\n", f.Variant, f.JSONName)
		}
	}
	cases := make([]string, 0, len(fl.Known)+1)
	cases = append(cases, `""`)
	for _, known := range fl.Known {
		cases = append(cases, fmt.Sprintf("%q", known))
	}
	fmt.Fprintf(b, "\tswitch tag {\n\tcase %s:\n\tdefault:\n\t\tfor key, raw := range v.%s {\n\t\t\tif _, owned := obj[key]; !owned {\n\t\t\t\tobj[key] = raw\n\t\t\t}\n\t\t}\n\t}\n", strings.Join(cases, ", "), fl.ExtrasGo)
	b.WriteString("\treturn json.Marshal(obj)\n}\n\n")
}

// emitDecodeHelpers writes the helpers the generated decoders share.
func emitDecodeHelpers(b *strings.Builder, nullable, nonZero bool) {
	b.WriteString(`// decoder is implemented by the generated structs and unions. buffered is
// set where serde decodes from buffered content, which reads a non-negative
// integer union tag as a variant index.
type decoder interface {
	decodeJSON(data []byte, buffered bool) error
}

// errNull reports a null where the schema does not admit one.
var errNull = fmt.Errorf("null is not allowed here")

// errSeq reports an array where only an object decodes.
var errSeq = fmt.Errorf("invalid type: sequence, expected an object")

func isNull(raw json.RawMessage) bool { return string(raw) == "null" }

// jsonKind returns the first byte of a JSON value.
func jsonKind(raw []byte) byte {
	for _, c := range raw {
		switch c {
		case ' ', '\t', '\n', '\r':
		default:
			return c
		}
	}
	return 0
}

func isArray(data []byte) bool { return jsonKind(data) == '[' }

func missingField(owner, key string) error {
	return fmt.Errorf("%s: missing field %q", owner, key)
}

func seqTooShort(owner string, n, want int) error {
	return fmt.Errorf("invalid length %d, expected struct %s with %d elements", n, owner, want)
}

// seqEmpty fails when a variant without fields gets elements.
func seqEmpty(items []json.RawMessage) error {
	if len(items) > 0 {
		return fmt.Errorf("invalid length %d, expected 0 elements in sequence", len(items))
	}
	return nil
}

func seqTooLong(n int) error {
	return fmt.Errorf("invalid length %d, expected fewer elements in array", n)
}

// rawObject holds the members of a JSON object in wire order. The values are
// slices of the decoded document, not copies.
type rawObject []rawMember

type rawMember struct {
	key []byte
	raw json.RawMessage
}

// get returns the value of key; a duplicate key keeps its last value, as
// the Rust SDK's serde_json::Value does.
func (o rawObject) get(key string) (json.RawMessage, bool) {
	for i := len(o) - 1; i >= 0; i-- {
		if string(o[i].key) == key {
			return o[i].raw, true
		}
	}
	return nil, false
}

// hasKeys reports whether every key is present.
func hasKeys(raws rawObject, keys ...string) bool {
	for _, k := range keys {
		if _, ok := raws.get(k); !ok {
			return false
		}
	}
	return true
}

// withoutKey returns raws without key, leaving raws unchanged.
func withoutKey(raws rawObject, key string) rawObject {
	out := make(rawObject, 0, len(raws))
	for _, m := range raws {
		if string(m.key) != key {
			out = append(out, m)
		}
	}
	return out
}

// objectData returns the wire bytes of an object, re-encoding raws when the
// original bytes are not at hand.
func objectData(data []byte, raws rawObject) json.RawMessage {
	if data != nil {
		return append(json.RawMessage(nil), data...)
	}
	obj := make(map[string]json.RawMessage, len(raws))
	for _, m := range raws {
		obj[string(m.key)] = m.raw
	}
	out, _ := json.Marshal(obj)
	return out
}

// splitObject splits a JSON object into its members without copying them.
// The document has already been validated by encoding/json (the decoders
// only see values it handed to UnmarshalJSON, or slices of them), so the
// scan only finds value boundaries; anything unexpected defers to
// encoding/json for its error. null reads as no object, as in encoding/json.
func splitObject(data []byte) (rawObject, error) {
	i := skipSpace(data, 0)
	if i < len(data) && data[i] == 'n' {
		return nil, nil
	}
	if i >= len(data) || data[i] != '{' {
		return nil, fmt.Errorf("invalid type: %s, expected a map", serdeKind(data))
	}
	var out rawObject
	i = skipSpace(data, i+1)
	if i < len(data) && data[i] == '}' {
		return rawObject{}, nil
	}
	for i < len(data) {
		end, ok := scanString(data, i)
		if !ok {
			break
		}
		key := data[i+1 : end-1]
		if bytesIndex(key, '\\') >= 0 {
			var unescaped string
			if err := json.Unmarshal(data[i:end], &unescaped); err != nil {
				return nil, err
			}
			key = []byte(unescaped)
		}
		i = skipSpace(data, end)
		if i >= len(data) || data[i] != ':' {
			break
		}
		start := skipSpace(data, i+1)
		end, ok = scanValue(data, start)
		if !ok {
			break
		}
		out = append(out, rawMember{key: key, raw: data[start:end:end]})
		i = skipSpace(data, end)
		if i < len(data) && data[i] == ',' {
			i = skipSpace(data, i+1)
			continue
		}
		if i < len(data) && data[i] == '}' {
			return out, nil
		}
		break
	}
	return nil, jsonError(data, new(map[string]json.RawMessage))
}

// splitArray splits a JSON array into its elements without copying them.
func splitArray(data []byte) ([]json.RawMessage, error) {
	i := skipSpace(data, 0)
	if i < len(data) && data[i] == 'n' {
		return nil, nil
	}
	if i >= len(data) || data[i] != '[' {
		return nil, fmt.Errorf("invalid type: %s, expected a sequence", serdeKind(data))
	}
	out := []json.RawMessage{}
	i = skipSpace(data, i+1)
	if i < len(data) && data[i] == ']' {
		return out, nil
	}
	for i < len(data) {
		end, ok := scanValue(data, i)
		if !ok {
			break
		}
		out = append(out, data[i:end:end])
		i = skipSpace(data, end)
		if i < len(data) && data[i] == ',' {
			i = skipSpace(data, i+1)
			continue
		}
		if i < len(data) && data[i] == ']' {
			return out, nil
		}
		break
	}
	return nil, jsonError(data, new([]json.RawMessage))
}

// jsonError reports a document the fast scan could not split; encoding/json
// has validated it, so this means a bug in the scan.
func jsonError(data []byte, _ any) error {
	return fmt.Errorf("invalid JSON value %.40q", data)
}

func skipSpace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func bytesIndex(data []byte, c byte) int {
	for i, d := range data {
		if d == c {
			return i
		}
	}
	return -1
}

// scanString returns the end of the string starting at data[i].
func scanString(data []byte, i int) (int, bool) {
	if i >= len(data) || data[i] != '"' {
		return 0, false
	}
	for j := i + 1; j < len(data); j++ {
		switch data[j] {
		case '\\':
			j++
		case '"':
			return j + 1, true
		}
	}
	return 0, false
}

// scanValue returns the end of the value starting at data[i].
func scanValue(data []byte, i int) (int, bool) {
	if i >= len(data) {
		return 0, false
	}
	switch data[i] {
	case '"':
		return scanString(data, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(data); j++ {
			switch data[j] {
			case '"':
				end, ok := scanString(data, j)
				if !ok {
					return 0, false
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, true
				}
			}
		}
		return 0, false
	}
	j := i
	for j < len(data) {
		switch data[j] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return j, j > i
		}
		j++
	}
	return j, j > i
}

// arrayData re-encodes array elements.
func arrayData(items []json.RawMessage) json.RawMessage {
	if items == nil {
		items = []json.RawMessage{}
	}
	out, _ := json.Marshal(items)
	return out
}

// enumValue reads a unit-variant enum value: a string, or the one-key map
// form {"variant": null} that serde also accepts.
func enumValue(data []byte) (string, bool, error) {
	switch jsonKind(data) {
	case '"':
		if end := len(data) - 1; end > 0 && data[end] == '"' && bytesIndex(data[1:end], '\\') < 0 {
			return string(data[1:end]), false, nil
		}
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return "", false, err
		}
		return s, false, nil
	case '{':
		members, err := splitObject(data)
		if err != nil {
			return "", false, err
		}
		if len(members) != 1 {
			return "", false, fmt.Errorf("invalid value: map, expected map with a single key")
		}
		if !isNull(members[0].raw) {
			return "", false, fmt.Errorf("invalid type: %s, expected unit", serdeKind(members[0].raw))
		}
		return string(members[0].key), true, nil
	}
	return "", false, fmt.Errorf("invalid type: %s, expected a variant", serdeKind(data))
}

// serdeKind names the kind of a JSON value the way serde's errors do.
func serdeKind(data []byte) string {
	switch c := jsonKind(data); {
	case c == '{':
		return "map"
	case c == '[':
		return "sequence"
	case c == '"':
		return "string"
	case c == 't' || c == 'f':
		return "boolean"
	case c == 'n':
		return "null"
	case c == '-' || (c >= '0' && c <= '9'):
		return "number"
	}
	return "value"
}

// decodeTag reads a union tag: a string or, from buffered content, a
// non-negative integer naming the tagged variant at that index.
func decodeTag(raw json.RawMessage, buffered bool, tags []string) (string, bool) {
	switch c := jsonKind(raw); {
	case c == '"':
		if end := len(raw) - 1; end > 0 && raw[end] == '"' && bytesIndex(raw[1:end], '\\') < 0 {
			return string(raw[1:end]), true
		}
		var tag string
		return tag, json.Unmarshal(raw, &tag) == nil
	case buffered && c >= '0' && c <= '9':
		var index uint64
		for _, d := range []byte(raw) {
			if d < '0' || d > '9' {
				return "", false
			}
			next := index*10 + uint64(d-'0')
			if next/10 != index {
				return "", false
			}
			index = next
		}
		if index < uint64(len(tags)) {
			return tags[index], true
		}
	}
	return "", false
}

// decodeValue decodes one value, failing on null unless the schema admits it.
func decodeValue[T any](raw json.RawMessage, dst *T, nullOK, buffered bool) error {
	if !nullOK && isNull(raw) {
		return errNull
	}
	if d, ok := any(dst).(decoder); ok {
		return d.decodeJSON(raw, buffered)
	}
	return typeError(json.Unmarshal(raw, dst), raw)
}

// typeError rewrites encoding/json's type errors the way serde words them,
// without Go type names.
func typeError(err error, raw []byte) error {
	if err == nil {
		return nil
	}
	var te *json.UnmarshalTypeError
	if !errors.As(err, &te) {
		return err
	}
	expected := "a value"
	switch te.Type.Kind() {
	case reflect.String:
		expected = "a string"
	case reflect.Bool:
		expected = "a boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		expected = "an integer"
	case reflect.Float32, reflect.Float64:
		expected = "a number"
	case reflect.Map, reflect.Struct:
		expected = "a map"
	case reflect.Slice, reflect.Array:
		expected = "a sequence"
	}
	kind := serdeKind(raw)
	if kind == "number" && strings.Contains(expected, "integer") {
		return fmt.Errorf("invalid value: %s, expected %s", raw, expected)
	}
	return fmt.Errorf("invalid type: %s, expected %s", kind, expected)
}

// decodePtr decodes an optional struct or union.
func decodePtr[T any, P interface {
	*T
	decoder
}](raw json.RawMessage, dst **T, nullOK, buffered bool) error {
	if isNull(raw) {
		if !nullOK {
			return errNull
		}
		*dst = nil
		return nil
	}
	value := P(new(T))
	if err := value.decodeJSON(raw, buffered); err != nil {
		return err
	}
	*dst = (*T)(value)
	return nil
}

// decodeChecked decodes a union of primitives after check accepts it.
func decodeChecked[T any](raw json.RawMessage, dst *T, nullOK bool, check func(json.RawMessage) error) error {
	if isNull(raw) && !nullOK {
		return errNull
	}
	if !isNull(raw) {
		if err := check(raw); err != nil {
			return err
		}
	}
	return typeError(json.Unmarshal(raw, dst), raw)
}

// decodeList decodes a list item by item. With skip set
// (x-deserialize-skip-invalid-items), items that do not decode are dropped
// and the others kept.
func decodeList[T any](raw json.RawMessage, dst *[]T, nullOK, skip bool, item func(json.RawMessage, *T) error) error {
	if isNull(raw) {
		if !nullOK {
			return errNull
		}
		*dst = nil
		return nil
	}
	items, err := splitArray(raw)
	if err != nil {
		return err
	}
	out := make([]T, 0, len(items))
	for i, raw := range items {
		var value T
		if err := item(raw, &value); err != nil {
			if skip {
				continue
			}
			return fmt.Errorf("item %d: %w", i, err)
		}
		out = append(out, value)
	}
	*dst = out
	return nil
}

// decodeMap decodes an object whose values must each decode.
func decodeMap[M ~map[string]V, V any](raw json.RawMessage, dst *M, nullOK bool, value func(json.RawMessage, *V) error) error {
	if isNull(raw) {
		if !nullOK {
			return errNull
		}
		*dst = nil
		return nil
	}
	entries, err := splitObject(raw)
	if err != nil {
		return err
	}
	out := make(M, len(entries))
	for _, entry := range entries {
		var v V
		if err := value(entry.raw, &v); err != nil {
			return fmt.Errorf("%q: %w", entry.key, err)
		}
		out[string(entry.key)] = v
	}
	*dst = out
	return nil
}

`)
	if nullable {
		b.WriteString(`// decodeNullable decodes a field that keeps null distinct from omission.
func decodeNullable[T any](raw json.RawMessage, dst *Nullable[T], decode func(json.RawMessage, *T) error) error {
	if isNull(raw) {
		*dst = Nullable[T]{Set: true, Null: true}
		return nil
	}
	var value T
	if err := decode(raw, &value); err != nil {
		return err
	}
	*dst = Nullable[T]{Set: true, Value: value}
	return nil
}

`)
	}
	if nonZero {
		b.WriteString(`// decodeNonZero decodes an integer Rust reads as NonZero, which rejects 0.
func decodeNonZero[T ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~int8 | ~int16 | ~int32 | ~int64](raw json.RawMessage, dst *T) error {
	if err := decodeValue(raw, dst, false, false); err != nil {
		return err
	}
	if *dst == 0 {
		return fmt.Errorf("invalid value: 0, expected a nonzero integer")
	}
	return nil
}

`)
	}
}

// capturesExtras reports whether decoding the def keeps wire keys it does not
// name, so a union routing to it must strip its own discriminator first.
func capturesExtras(r *ir, name string, seen map[string]bool) bool {
	td := r.ByName[name]
	if td == nil || seen[name] {
		return false
	}
	seen[name] = true
	switch td.Kind {
	case kindStruct:
		return td.Flatten != nil
	case kindAlias:
		return capturesExtras(r, td.Alias, seen)
	case kindTaggedUnion:
		for _, v := range td.Variants {
			if !v.Open && v.Payload != "" && capturesExtras(r, v.Payload, seen) {
				return true
			}
		}
	}
	return false
}

var unchangedEmitted bool

func emitUnchanged(b *strings.Builder) {
	if unchangedEmitted {
		return
	}
	unchangedEmitted = true
	b.WriteString(`// unchangedSince reports whether current still encodes exactly like a
// fresh decode of raw, so raw can be re-sent verbatim with its unknown keys.
func unchangedSince[T any](raw json.RawMessage, current *T) bool {
	var fresh T
	if json.Unmarshal(raw, &fresh) != nil {
		return false
	}
	a, err := json.Marshal(&fresh)
	if err != nil {
		return false
	}
	c, err := json.Marshal(current)
	return err == nil && string(a) == string(c)
}

`)
}
