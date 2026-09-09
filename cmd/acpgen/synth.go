package main

import (
	"fmt"
	"strings"
)

// synthWire deterministically synthesizes a JSON document exercising the
// given def: required fields only, or every field when full is set. The
// documents are committed as parity fixtures; decoding and re-encoding one
// must reproduce it exactly.
func synthWire(r *ir, name string, full bool, depth map[string]int) (string, error) {
	if depth[name] > 3 {
		return "", fmt.Errorf("%s: schema reference cycle at depth %d", name, depth[name])
	}
	td, ok := r.ByName[name]
	if !ok {
		return "", fmt.Errorf("unknown def %q", name)
	}
	switch td.Kind {
	case kindAlias:
		return synthWire(r, td.Alias, full, withDepth(depth, name))

	case kindNewtype, kindEnum:
		if td.Base == "string" {
			if td.Kind == kindEnum && len(td.Variants) > 0 {
				return fmt.Sprintf("%q", td.Variants[0].Const), nil
			}
			return `""`, nil
		}
		if td.Base == "float64" {
			return "0.5", nil
		}
		return "0", nil

	case kindAny:
		return `""`, nil

	case kindStruct:
		return synthFields(r, td.Fields, full, withDepth(depth, name))

	case kindTaggedUnion, kindUntaggedUnion:
		var chosen *variant
		if td.Kind == kindTaggedUnion {
			for i := range td.Variants {
				v := &td.Variants[i]
				if v.Open {
					continue
				}
				chosen = v
				if v.Const != "" {
					break // prefer a tagged variant over the wire default
				}
			}
		} else if len(td.Variants) > 0 {
			chosen = &td.Variants[0]
		}
		if chosen == nil {
			return "null", nil
		}
		parts, err := synthFieldParts(r, td.Fields, full, withDepth(depth, name))
		if err != nil {
			return "", err
		}
		if chosen.Const != "" {
			parts = append(parts, fmt.Sprintf("%q: %q", td.TagJSON, chosen.Const))
		}
		switch {
		case chosen.Open:
			return "null", nil
		case strings.HasPrefix(chosen.Payload, "[]"):
			item, err := synthWire(r, strings.TrimPrefix(chosen.Payload, "[]"), full, withDepth(depth, name))
			if err != nil {
				return "", err
			}
			return "[" + item + "]", nil
		case chosen.Payload != "":
			payload, err := synthWire(r, chosen.Payload, full, withDepth(depth, name))
			if err != nil {
				return "", err
			}
			parts = append(parts, objectParts(payload)...)
		default:
			inline, err := synthFieldParts(r, chosen.Inline, full, withDepth(depth, name))
			if err != nil {
				return "", err
			}
			parts = append(parts, inline...)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil

	default:
		return "", fmt.Errorf("%s: cannot synthesize kind %d", name, td.Kind)
	}
}

// synthFields renders an object with required fields (or all fields when
// full), recursing through referenced defs.
func synthFields(r *ir, fields []field, full bool, depth map[string]int) (string, error) {
	parts, err := synthFieldParts(r, fields, full, depth)
	if err != nil {
		return "", err
	}
	return "{" + strings.Join(parts, ", ") + "}", nil
}

func synthFieldParts(r *ir, fields []field, full bool, depth map[string]int) ([]string, error) {
	var parts []string
	for _, f := range fields {
		if !f.Required && !full {
			continue
		}
		value, err := synthValue(r, f, full, depth)
		if err != nil {
			return nil, err
		}
		parts = append(parts, fmt.Sprintf("%q: %s", f.JSONName, value))
	}
	return parts, nil
}

// objectParts splits a synthesized JSON object into its top-level members.
func objectParts(obj string) []string {
	obj = strings.TrimSpace(obj)
	obj = strings.TrimPrefix(obj, "{")
	obj = strings.TrimSuffix(obj, "}")
	obj = strings.TrimSpace(obj)
	if obj == "" {
		return nil
	}
	return []string{obj}
}

func synthValue(r *ir, f field, full bool, depth map[string]int) (string, error) {
	typ := f.GoType
	optional := !f.Required
	if optional && !f.pointer() {
		optional = false // containers stay inline; omit when not full instead
	}
	_ = optional

	switch {
	case typ == "string":
		return `""`, nil
	case typ == "int64" || typ == "uint64":
		return "0", nil
	case typ == "float64":
		return "0.5", nil
	case typ == "bool":
		if full {
			return "true", nil
		}
		return "false", nil
	case typ == "Meta" || typ == "map[string]any":
		return `{"k": "v"}`, nil
	case typ == "map[string]string":
		return `{"k": ""}`, nil
	case typ == "json.RawMessage" || typ == "any":
		return "null", nil
	case strings.HasPrefix(typ, "[]"):
		elem := strings.TrimPrefix(typ, "[]")
		item, err := synthElem(r, elem, depth)
		if err != nil {
			return "", err
		}
		return "[" + item + "]", nil
	case strings.HasPrefix(typ, "map["):
		// One non-empty entry: optional maps with omitempty would
		// otherwise vanish on re-encode.
		val := strings.TrimPrefix(typ, "map[string]")
		item, err := synthElem(r, val, depth)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(item, "{") {
			return fmt.Sprintf("{\"k\": %s}", item), nil
		}
		return fmt.Sprintf("{\"k\": %s}", item), nil
	default:
		return synthWire(r, typ, full, depth)
	}
}

func synthElem(r *ir, elem string, depth map[string]int) (string, error) {
	switch elem {
	case "string":
		return `""`, nil
	case "int64", "uint64":
		return "0", nil
	case "float64":
		return "0.5", nil
	case "bool":
		return "true", nil
	case "any":
		return `""`, nil
	default:
		return synthWire(r, elem, true, depth)
	}
}

func withDepth(depth map[string]int, name string) map[string]int {
	next := make(map[string]int, len(depth)+1)
	for k, v := range depth {
		next[k] = v
	}
	next[name]++
	return next
}
