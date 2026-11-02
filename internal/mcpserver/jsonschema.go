package mcpserver

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"sort"
	"strings"
)

// validateSchema checks instance against a JSON Schema. It implements the
// structural keywords tool schemas rely on: type, enum, const, properties,
// required, additionalProperties, items, allOf, anyOf, and oneOf. Every other
// keyword, including $ref and string or numeric bounds, is ignored and never
// rejects a value.
func validateSchema(schema, instance json.RawMessage) error {
	var root any
	if err := decodeNumbers(schema, &root); err != nil {
		return fmt.Errorf("invalid input schema: %w", err)
	}
	var value any
	if err := decodeNumbers(instance, &value); err != nil {
		return fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	return validateValue(root, value, "arguments")
}

func validateValue(schema, value any, path string) error {
	switch s := schema.(type) {
	case bool:
		if !s {
			return fmt.Errorf("%s is not allowed", path)
		}
		return nil
	case map[string]any:
		return validateObjectSchema(s, value, path)
	default:
		return nil
	}
}

func validateObjectSchema(schema map[string]any, value any, path string) error {
	if kinds, ok := schemaTypes(schema["type"]); ok && !slices.ContainsFunc(kinds, func(kind string) bool { return hasType(value, kind) }) {
		return fmt.Errorf("%s must be of type %s, got %s", path, strings.Join(kinds, " or "), typeName(value))
	}
	if enum, ok := schema["enum"].([]any); ok && !slices.ContainsFunc(enum, func(candidate any) bool { return jsonEqual(candidate, value) }) {
		return fmt.Errorf("%s must be one of the allowed values", path)
	}
	if constant, ok := schema["const"]; ok && !jsonEqual(constant, value) {
		return fmt.Errorf("%s must equal the constant value", path)
	}
	if object, ok := value.(map[string]any); ok {
		if err := validateProperties(schema, object, path); err != nil {
			return err
		}
	}
	if array, ok := value.([]any); ok {
		if items, present := schema["items"]; present {
			for i, item := range array {
				if err := validateValue(items, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	if all, ok := schema["allOf"].([]any); ok {
		for _, sub := range all {
			if err := validateValue(sub, value, path); err != nil {
				return err
			}
		}
	}
	if any_, ok := schema["anyOf"].([]any); ok {
		if !slices.ContainsFunc(any_, func(sub any) bool { return validateValue(sub, value, path) == nil }) {
			return fmt.Errorf("%s does not match any allowed schema", path)
		}
	}
	if one, ok := schema["oneOf"].([]any); ok {
		matches := 0
		for _, sub := range one {
			if validateValue(sub, value, path) == nil {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("%s must match exactly one allowed schema, matched %d", path, matches)
		}
	}
	return nil
}

func validateProperties(schema map[string]any, object map[string]any, path string) error {
	if required, ok := schema["required"].([]any); ok {
		for _, name := range required {
			key, isString := name.(string)
			if !isString {
				continue
			}
			if _, present := object[key]; !present {
				return fmt.Errorf("%s is missing required property %q", path, key)
			}
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		child := path + "." + key
		if sub, declared := properties[key]; declared {
			if err := validateValue(sub, object[key], child); err != nil {
				return err
			}
			continue
		}
		if additional, present := schema["additionalProperties"]; present {
			if allowed, isBool := additional.(bool); isBool && !allowed {
				return fmt.Errorf("%s has unexpected property %q", path, key)
			}
			if err := validateValue(additional, object[key], child); err != nil {
				return err
			}
		}
	}
	return nil
}

func schemaTypes(raw any) ([]string, bool) {
	switch t := raw.(type) {
	case string:
		return []string{t}, true
	case []any:
		kinds := make([]string, 0, len(t))
		for _, kind := range t {
			if s, ok := kind.(string); ok {
				kinds = append(kinds, s)
			}
		}
		return kinds, len(kinds) > 0
	default:
		return nil, false
	}
}

func hasType(value any, kind string) bool {
	switch kind {
	case "null":
		return value == nil
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "number":
		_, ok := value.(json.Number)
		return ok
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		parsed, ok := parseNumber(number)
		return ok && parsed.IsInt()
	default:
		// Unknown type names do not constrain the value.
		return true
	}
}

func typeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case json.Number:
		return "number"
	default:
		return "unknown"
	}
}

// jsonEqual compares decoded JSON values, treating numbers by value.
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		left, leftOK := parseNumber(x)
		right, rightOK := parseNumber(y)
		if !leftOK || !rightOK {
			return x == y
		}
		return left.Cmp(right) == 0
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for key, value := range x {
			other, present := y[key]
			if !present || !jsonEqual(value, other) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}

// parseNumber parses a JSON number without letting a huge exponent allocate
// a huge value.
func parseNumber(number json.Number) (*big.Float, bool) {
	parsed, _, err := big.ParseFloat(number.String(), 10, 512, big.ToNearestEven)
	return parsed, err == nil
}
