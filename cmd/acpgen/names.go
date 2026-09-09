package main

import (
	"strings"
	"unicode"
)

// initialisms are uppercased when they appear as whole words in identifiers.
var initialisms = map[string]bool{
	"ID": true, "URL": true, "URI": true, "HTTP": true, "SSE": true,
	"ACP": true, "MCP": true, "JSON": true, "RPC": true, "UI": true,
	"API": true, "OS": true, "LLM": true,
}

// goNameOf converts a wire property name or variant title to a Go identifier.
func goNameOf(s string) string {
	if s == "" {
		return ""
	}
	words := splitWords(s)
	for i, w := range words {
		if up, ok := initialisms[strings.ToUpper(w)]; ok && up {
			words[i] = strings.ToUpper(w)
			continue
		}
		words[i] = capitalize(w)
	}
	return strings.Join(words, "")
}

// pascal converts a snake_case discriminator value to PascalCase without
// initialism rewriting, so enum constants read exactly like their values.
func pascal(s string) string {
	out := strings.Builder{}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' || r == '.' }) {
		out.WriteString(capitalize(w))
	}
	return out.String()
}

func capitalize(w string) string {
	if w == "" {
		return ""
	}
	r := []rune(w)
	return string(unicode.ToUpper(r[0])) + strings.ToLower(string(r[1:]))
}

// splitWords splits snake_case and lowerCamelCase into lowercase words,
// preserving consecutive capitals as one word (e.g. "Sse" stays "sse").
func splitWords(s string) []string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, string(cur))
			cur = nil
		}
	}
	for _, r := range s {
		switch {
		case r == '_' || r == '-' || r == '.' || r == ' ':
			flush()
		case unicode.IsUpper(r) && len(cur) > 0 && !unicode.IsUpper(cur[len(cur)-1]):
			flush()
			cur = append(cur, unicode.ToLower(r))
		default:
			cur = append(cur, unicode.ToLower(r))
		}
	}
	flush()
	return words
}

// methodGoName turns a method name like "session/new" (or a meta.json key
// like "cancel_request") into a Go identifier such as "SessionNew".
func methodGoName(s string) string {
	s = strings.TrimLeft(s, "$/")
	return pascal(strings.ReplaceAll(s, "/", "_"))
}
