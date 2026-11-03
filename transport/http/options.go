package acphttp

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	// DefaultHealthPath is the path of the health endpoint, which answers
	// GET with "ok" unless ServerOptions.DisableHealth is set.
	DefaultHealthPath = "/health"

	// DefaultMaxBodyBytes is the default limit on a POST body, matching the
	// Rust crate's 16 MiB.
	DefaultMaxBodyBytes = 16 << 20
)

// ServerOptions configures a Server. The zero value is the default, matching
// the Rust crate's ServerOptions::default(): the endpoint at /acp, CORS
// disabled, the /health endpoint enabled, and a 16 MiB POST body limit.
type ServerOptions struct {
	// Path is the ACP endpoint path. Empty means DefaultPath. It must start
	// with "/" and may only be DefaultHealthPath when DisableHealth is set.
	Path string
	// CORS is the browser origin policy. The zero value is CORSDisabled.
	CORS CORSOptions
	// DisableHealth turns off the GET /health endpoint.
	DisableHealth bool
	// MaxBodyBytes bounds a POST body. Zero or less means DefaultMaxBodyBytes.
	// A larger body, or one whose Content-Length declares more, gets 413.
	MaxBodyBytes int64
}

// Validate reports a configuration the Rust crate refuses to build a router
// for: a path that is not absolute, or one that collides with the health
// endpoint.
func (o ServerOptions) Validate() error {
	path := o.path()
	if !strings.HasPrefix(path, "/") {
		return fmt.Errorf("ACP HTTP path %q must start with /", path)
	}
	if path == DefaultHealthPath && !o.DisableHealth {
		return fmt.Errorf("ACP HTTP path %q collides with the health endpoint; set DisableHealth to use it", path)
	}
	return nil
}

func (o ServerOptions) path() string {
	if o.Path == "" {
		return DefaultPath
	}
	return o.Path
}

func (o ServerOptions) maxBodyBytes() int64 {
	if o.MaxBodyBytes <= 0 {
		return DefaultMaxBodyBytes
	}
	return o.MaxBodyBytes
}

// CORSOptions is the browser origin policy for HTTP CORS and, with the
// WebSocket binding, for upgrade Origin checks. It is not authentication:
// requests without an Origin header pass the origin check.
//
// Build one with CORSDisabled (the zero value), AllowOrigins, or
// AllowAnyOrigin. The built-in policy allows the ACP transport headers but not
// Authorization, and never enables credentialed CORS. Hosts that need a
// credential policy keep CORS disabled here and wrap the Server with their own.
type CORSOptions struct {
	mode    corsMode
	origins []string
}

type corsMode int

const (
	corsDisabled corsMode = iota
	corsAllowOrigins
	corsAllowAnyOrigin
)

// CORSDisabled disables cross-origin HTTP access. Requests carrying an Origin
// header fail the origin check.
func CORSDisabled() CORSOptions { return CORSOptions{} }

// AllowAnyOrigin allows any browser origin with a wildcard
// Access-Control-Allow-Origin, without enabling credentialed CORS.
func AllowAnyOrigin() CORSOptions { return CORSOptions{mode: corsAllowAnyOrigin} }

// AllowOrigins allows only the given browser origins, compared byte for byte
// with the request's Origin header. It returns an *InvalidOriginError naming
// the first origin that is not a valid HTTP header value.
func AllowOrigins(origins ...string) (CORSOptions, error) {
	allowed := make([]string, 0, len(origins))
	for _, origin := range origins {
		if !validHeaderValue(origin) {
			return CORSOptions{}, &InvalidOriginError{Origin: origin}
		}
		allowed = append(allowed, origin)
	}
	return CORSOptions{mode: corsAllowOrigins, origins: allowed}, nil
}

// Enabled reports whether the policy answers CORS requests at all.
func (c CORSOptions) Enabled() bool { return c.mode != corsDisabled }

// Origins returns a copy of the allowlist, or nil unless the policy was built
// with AllowOrigins.
func (c CORSOptions) Origins() []string {
	if c.mode != corsAllowOrigins {
		return nil
	}
	return append([]string{}, c.origins...)
}

// AllowsAnyOrigin reports whether the policy was built with AllowAnyOrigin.
func (c CORSOptions) AllowsAnyOrigin() bool { return c.mode == corsAllowAnyOrigin }

// allowsOrigin is the origin check shared by CORS and the WebSocket upgrade:
// a request without an Origin header passes, a disabled policy rejects any
// Origin, and an allowlist matches it exactly.
func (c CORSOptions) allowsOrigin(header http.Header) bool {
	values := header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	return c.allows(values[0])
}

func (c CORSOptions) allows(origin string) bool {
	switch c.mode {
	case corsAllowAnyOrigin:
		return true
	case corsAllowOrigins:
		for _, allowed := range c.origins {
			if allowed == origin {
				return true
			}
		}
	}
	return false
}

// InvalidOriginError reports an origin passed to AllowOrigins that is not a
// valid HTTP header value.
type InvalidOriginError struct {
	Origin string
}

func (e *InvalidOriginError) Error() string {
	return fmt.Sprintf("invalid CORS origin %q", e.Origin)
}

// Unwrap returns ErrInvalidHeaderValue.
func (e *InvalidOriginError) Unwrap() error { return ErrInvalidHeaderValue }

// ErrInvalidHeaderValue is the cause of an InvalidOriginError: the value holds
// a control character other than a horizontal tab.
var ErrInvalidHeaderValue = errors.New("invalid HTTP header value")

// validHeaderValue follows the http crate's HeaderValue::from_str: every byte
// is a horizontal tab or at least 0x20 and not DEL.
func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if b := value[i]; (b < 0x20 && b != '\t') || b == 0x7f {
			return false
		}
	}
	return true
}

// The tower-http CorsLayer the Rust crate configures, as constants.
const (
	corsAllowMethods  = "GET,POST,DELETE,OPTIONS"
	corsAllowHeaders  = "content-type,accept,acp-connection-id,acp-session-id,sec-websocket-version,sec-websocket-key,connection,upgrade"
	corsExposeHeaders = "acp-connection-id,acp-session-id"
)

// applyCORS writes the policy's headers the way the Rust crate's tower-http
// CorsLayer does, and reports whether the request was a preflight it has
// answered. Every OPTIONS request is treated as a preflight, at any path.
func (c CORSOptions) applyCORS(w http.ResponseWriter, r *http.Request) bool {
	if !c.Enabled() {
		return false
	}
	header := w.Header()
	switch c.mode {
	case corsAllowAnyOrigin:
		header.Set("Access-Control-Allow-Origin", "*")
	case corsAllowOrigins:
		header.Add("Vary", "origin")
		if values := r.Header.Values("Origin"); len(values) > 0 && c.allows(values[0]) {
			header.Set("Access-Control-Allow-Origin", values[0])
		}
	}
	if r.Method == http.MethodOptions {
		header.Set("Access-Control-Allow-Methods", corsAllowMethods)
		header.Set("Access-Control-Allow-Headers", corsAllowHeaders)
		w.WriteHeader(http.StatusOK)
		return true
	}
	header.Set("Access-Control-Expose-Headers", corsExposeHeaders)
	return false
}
