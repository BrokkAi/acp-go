// Package acpvalidate holds the dependency-free validators that the typed v1
// and draft-v2 client facades run before a request reaches the wire.
//
// The generated schema types stay permissive: unknown union tags, extension
// fields, and raw _meta payloads must keep decoding, so the generated code does
// not validate. Rust enforces IDs, absolute paths, media types, and URIs with
// semantic newtypes at the type boundary; Go has no equivalent newtype, so the
// facades that build requests check the same invariants with plain string work
// instead of reflection.
package acpvalidate

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// Identifier requires a non-blank identifier value. kind names the value in
// the error, for example "session ID" or "configuration value ID".
func Identifier(kind, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", kind)
	}
	return nil
}

// AbsolutePath requires the ACP absolute-path newtype. The session paths that
// the facades validate are resolved by the agent, which may run on a different
// platform than the client, so the check accepts a path that is absolute on
// either platform: POSIX-absolute (a leading "/"), Windows drive-absolute
// ("C:\dir" or "C:/dir"), or a Windows UNC path. Relative paths,
// drive-relative paths such as "C:dir", and root-relative paths such as
// "\dir" are rejected. The check is deliberately permissive: the agent stays
// the authority on its own filesystem.
func AbsolutePath(kind, value string) error {
	if !isAbsolutePath(value) {
		return fmt.Errorf("%s must be absolute: %q", kind, value)
	}
	return nil
}

// isAbsolutePath reports whether value is absolute on at least one supported
// platform. filepath.IsAbs answers for the client host and path.IsAbs answers
// for POSIX regardless of host; the Windows forms that filepath recognizes
// only when built for Windows need an explicit check so a POSIX client can
// address a Windows agent.
func isAbsolutePath(value string) bool {
	if filepath.IsAbs(value) || path.IsAbs(value) {
		return true
	}
	if len(value) >= 3 && isASCIILetter(value[0]) && value[1] == ':' && isPathSeparator(value[2]) {
		return true
	}
	return len(value) >= 2 && value[0] == '\\' && value[1] == '\\'
}

func isASCIILetter(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

func isPathSeparator(c byte) bool {
	return c == '/' || c == '\\'
}

// MediaType requires an RFC 6838 "type/subtype" media type. Optional
// parameters after the first ";" are checked for name=value shape but their
// values are otherwise opaque.
func MediaType(kind, value string) error {
	base, parameters, _ := strings.Cut(value, ";")
	if parameters == "" && strings.Contains(value, ";") {
		return fmt.Errorf("%s must be a media type of the form type/subtype: %q", kind, value)
	}
	if parameters != "" && !validParameters(parameters) {
		return fmt.Errorf("%s has malformed media type parameters: %q", kind, value)
	}
	typeName, subType, found := strings.Cut(strings.TrimSpace(base), "/")
	if !found || !isToken(typeName) || !isToken(subType) {
		return fmt.Errorf("%s must be a media type of the form type/subtype: %q", kind, value)
	}
	return nil
}

// URI requires an absolute RFC 3986 URI with a scheme. Hierarchical and opaque
// URIs are both accepted; the scheme decision is the part the ACP schema
// newtype makes.
func URI(kind, value string) error {
	scheme, _, found := strings.Cut(value, ":")
	if !found || !isScheme(scheme) {
		return fmt.Errorf("%s must be an absolute URI with a scheme: %q", kind, value)
	}
	return nil
}

// isToken reports whether value is a non-empty RFC 2045 token.
func isToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// isScheme reports whether value is a non-empty RFC 3986 scheme.
func isScheme(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		case ('0' <= c && c <= '9') || c == '+' || c == '-' || c == '.':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return value != ""
}

// validParameters checks a media type parameter list, respecting quoted values
// so a ";" inside quotes does not split a parameter.
func validParameters(value string) bool {
	quoted := false
	start := 0
	parts := []string{}
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '"':
			quoted = !quoted
		case ';':
			if !quoted {
				parts = append(parts, value[start:i])
				start = i + 1
			}
		}
	}
	if quoted {
		return false
	}
	parts = append(parts, value[start:])
	for _, part := range parts {
		name, rawValue, found := strings.Cut(part, "=")
		if !found || !isToken(strings.TrimSpace(name)) {
			return false
		}
		rawValue = strings.TrimSpace(rawValue)
		if !isToken(rawValue) && !isQuotedString(rawValue) {
			return false
		}
	}
	return true
}

func isQuotedString(value string) bool {
	return len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"'
}
