package main

import (
	"fmt"
	"strings"
)

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

// emitLenientStruct writes the codec of a struct whose malformed optional
// fields read as not reported, and, for a flattened tagged union, the codec
// that keeps the union's wire rules:
//
//   - a discriminator that is not a string reads as not reported;
//   - a lifted variant payload field is decoded and encoded only alongside
//     its own tag value;
//   - a custom or future tag keeps every other field it arrived with.
func emitLenientStruct(b *strings.Builder, td *typeDef) {
	if !td.Lenient {
		return
	}
	fl := td.Flatten
	lenientField := false
	for _, f := range td.Fields {
		lenientField = lenientField || f.Lenient
	}
	if fl == nil && !lenientField {
		return
	}
	shadow := unexport(td.Name) + "Shadow"
	if fl != nil {
		fmt.Fprintf(b, "// %s mirrors %s without its codec methods.\ntype %s %s\n\n", shadow, td.Name, shadow, td.Name)
		emitFlattenedMarshal(b, td, shadow)
	}

	fmt.Fprintf(b, "func (v *%s) UnmarshalJSON(data []byte) error {\n", td.Name)
	fmt.Fprintf(b, "\tvar raws map[string]json.RawMessage\n\tif err := json.Unmarshal(data, &raws); err != nil {\n\t\treturn err\n\t}\n\t*v = %s{}\n", td.Name)
	for _, f := range td.Fields {
		if f.Variant != "" || (fl != nil && f.JSONName == fl.TagJSON) {
			continue
		}
		emitFieldDecode(b, td, f)
	}
	if fl != nil {
		var tagField field
		for _, f := range td.Fields {
			if f.JSONName == fl.TagJSON {
				tagField = f
			}
		}
		fmt.Fprintf(b, "\tif raw, ok := raws[%q]; ok {\n\t\tvar tag *string\n", fl.TagJSON)
		if tagField.Lenient {
			fmt.Fprintf(b, "\t\tif json.Unmarshal(raw, &tag) == nil && tag != nil {\n\t\t\tv.%s = (*%s)(tag)\n\t\t}\n\t}\n", fl.TagGo, fl.Enum)
		} else {
			fmt.Fprintf(b, "\t\tif err := json.Unmarshal(raw, &tag); err != nil {\n\t\t\treturn fmt.Errorf(\"%s.%s: %%w\", err)\n\t\t}\n\t\tv.%s = (*%s)(tag)\n\t}\n", td.Name, fl.TagJSON, fl.TagGo, fl.Enum)
		}
		fmt.Fprintf(b, "\tif v.%s == nil {\n\t\treturn nil\n\t}\n\tswitch *v.%s {\n", fl.TagGo, fl.TagGo)
		for _, known := range fl.Known {
			fmt.Fprintf(b, "\tcase %q:\n", known)
			for _, f := range td.Fields {
				if f.Variant == known {
					emitFieldDecode(b, td, f)
				}
			}
		}
		fmt.Fprintf(b, "\tdefault:\n\t\tfor key, raw := range raws {\n\t\t\tswitch key {\n\t\t\tcase %s:\n\t\t\tdefault:\n\t\t\t\tif v.%s == nil {\n\t\t\t\t\tv.%s = map[string]json.RawMessage{}\n\t\t\t\t}\n\t\t\t\tv.%s[key] = raw\n\t\t\t}\n\t\t}\n\t}\n", quoteList(sharedKeys(td)), fl.ExtrasGo, fl.ExtrasGo, fl.ExtrasGo)
	}
	b.WriteString("\treturn nil\n}\n\n")
}

// sharedKeys lists the wire keys a flattened struct owns regardless of its
// tag value: every field except lifted variant payloads.
func sharedKeys(td *typeDef) []string {
	var keys []string
	for _, f := range td.Fields {
		if f.Variant == "" {
			keys = append(keys, f.JSONName)
		}
	}
	return keys
}

func emitFieldDecode(b *strings.Builder, td *typeDef, f field) {
	fmt.Fprintf(b, "\tif raw, ok := raws[%q]; ok {\n\t\tvar value %s\n", f.JSONName, f.declType())
	if f.Lenient {
		// Malformed reads as not reported (x-deserialize-default-on-error).
		fmt.Fprintf(b, "\t\tif json.Unmarshal(raw, &value) == nil {\n\t\t\tv.%s = value\n\t\t}\n\t}\n", f.GoName)
		return
	}
	fmt.Fprintf(b, "\t\tif err := json.Unmarshal(raw, &value); err != nil {\n\t\t\treturn fmt.Errorf(\"%s.%s: %%w\", err)\n\t\t}\n\t\tv.%s = value\n\t}\n", td.Name, f.JSONName, f.GoName)
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

var withoutKeyEmitted bool

func emitWithoutKey(b *strings.Builder) {
	if withoutKeyEmitted {
		return
	}
	withoutKeyEmitted = true
	b.WriteString(`// withoutKey drops a union discriminator before its payload is decoded, so
// payloads that keep unknown keys do not keep the discriminator too.
func withoutKey(data []byte, key string) []byte {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil {
		return data
	}
	delete(obj, key)
	out, err := json.Marshal(obj)
	if err != nil {
		return data
	}
	return out
}

`)
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
