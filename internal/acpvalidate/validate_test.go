package acpvalidate

import "testing"

func TestAbsolutePath(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "/tmp/workspace", want: true},
		{value: "relative/path", want: false},
		{value: "", want: false},
		{value: "./here", want: false},
	} {
		if err := AbsolutePath("path", tc.value); (err == nil) != tc.want {
			t.Fatalf("AbsolutePath(%q) error = %v, want ok=%v", tc.value, err, tc.want)
		}
	}
}

func TestIdentifier(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "session-1", want: true},
		{value: " config ", want: true},
		{value: "", want: false},
		{value: "   ", want: false},
		{value: "\t\n", want: false},
	} {
		if err := Identifier("identifier", tc.value); (err == nil) != tc.want {
			t.Fatalf("Identifier(%q) error = %v, want ok=%v", tc.value, err, tc.want)
		}
	}
}

func TestMediaType(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "image/png", want: true},
		{value: "text/plain; charset=utf-8", want: true},
		{value: `text/plain; name="a;b"`, want: true},
		{value: "application/vnd.acp+json", want: true},
		{value: "", want: false},
		{value: "image", want: false},
		{value: "/png", want: false},
		{value: "image/", want: false},
		{value: "image/png;", want: false},
		{value: "image/png;charset", want: false},
		{value: "image/png;charset=", want: false},
		{value: "image/p ng", want: false},
		{value: `text/plain; name="unterminated`, want: false},
	} {
		if err := MediaType("media type", tc.value); (err == nil) != tc.want {
			t.Fatalf("MediaType(%q) error = %v, want ok=%v", tc.value, err, tc.want)
		}
	}
}

func TestURI(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{value: "file:///tmp/x", want: true},
		{value: "https://example.com/a?b=c#d", want: true},
		{value: "mailto:user@example.com", want: true},
		{value: "urn:acp:thing", want: true},
		{value: "", want: false},
		{value: "/tmp/x", want: false},
		{value: "relative/path", want: false},
		{value: "1http://example.com", want: false},
		{value: "://example.com", want: false},
		{value: "http//example.com", want: false},
	} {
		if err := URI("URI", tc.value); (err == nil) != tc.want {
			t.Fatalf("URI(%q) error = %v, want ok=%v", tc.value, err, tc.want)
		}
	}
}
