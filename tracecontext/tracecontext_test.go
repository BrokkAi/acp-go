package tracecontext_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go/schema"
	v2schema "github.com/BrokkAi/acp-go/schema/v2"
	"github.com/BrokkAi/acp-go/tracecontext"
)

const (
	sampleTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	sampleTracestate  = "congo=t61rcWkgMzE"
	sampleBaggage     = "userId=alice"
)

func sampleContext() tracecontext.Context {
	return tracecontext.Context{
		Traceparent: sampleTraceparent,
		Tracestate:  sampleTracestate,
		Baggage:     sampleBaggage,
	}
}

func TestIntoMetaAndFromMetaRoundTrip(t *testing.T) {
	meta := tracecontext.IntoMeta(map[string]any{"unrelated": 1}, sampleContext())
	if meta["unrelated"] != 1 {
		t.Fatalf("unrelated key lost: %#v", meta)
	}
	got, ok := tracecontext.FromMeta(meta)
	if !ok {
		t.Fatal("trace context not detected")
	}
	if got != sampleContext() {
		t.Fatalf("round trip = %#v", got)
	}
}

func TestIntoMetaAllocatesAndSkipsEmptyFields(t *testing.T) {
	meta := tracecontext.IntoMeta(nil, tracecontext.Context{Traceparent: sampleTraceparent})
	if meta[tracecontext.TraceparentKey] != sampleTraceparent {
		t.Fatalf("traceparent = %#v", meta)
	}
	if _, ok := meta[tracecontext.TracestateKey]; ok {
		t.Fatal("empty tracestate should not be written")
	}
	if _, ok := meta[tracecontext.BaggageKey]; ok {
		t.Fatal("empty baggage should not be written")
	}

	untouched := map[string]any{"keep": true}
	if result := tracecontext.IntoMeta(untouched, tracecontext.Context{}); len(result) != 1 {
		t.Fatalf("empty context changed meta: %#v", result)
	}
}

func TestFromMetaIgnoresNonStringAndAbsentKeys(t *testing.T) {
	if _, ok := tracecontext.FromMeta(map[string]any{tracecontext.TraceparentKey: 42}); ok {
		t.Fatal("non-string traceparent treated as present")
	}
	if _, ok := tracecontext.FromMeta(nil); ok {
		t.Fatal("nil meta reported a trace context")
	}
	partial, ok := tracecontext.FromMeta(map[string]any{tracecontext.BaggageKey: sampleBaggage})
	if !ok || partial.Baggage != sampleBaggage || partial.Traceparent != "" {
		t.Fatalf("partial extraction = %#v, %v", partial, ok)
	}
}

func TestValidTraceparent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "w3c example", value: sampleTraceparent, want: true},
		{name: "future version", value: "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", want: true},
		{name: "future version extra field", value: "01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra", want: true},
		{name: "uppercase", value: strings.ToUpper(sampleTraceparent), want: false},
		{name: "reserved version", value: "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", want: false},
		{name: "zero trace id", value: "00-00000000000000000000000000000000-00f067aa0ba902b7-01", want: false},
		{name: "zero span id", value: "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", want: false},
		{name: "non-hex flags", value: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-zz", want: false},
		{name: "version 00 trailing field", value: sampleTraceparent + "-extra", want: false},
		{name: "too short", value: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7", want: false},
		{name: "wrong separator", value: "00_4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", want: false},
		{name: "empty", value: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tracecontext.ValidTraceparent(tc.value); got != tc.want {
				t.Fatalf("ValidTraceparent(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// TestGeneratedMetaTypesRoundTrip proves the helpers operate on the real
// generated v1 Meta and draft-v2 Nullable[Meta] fields across a JSON round trip.
func TestGeneratedMetaTypesRoundTrip(t *testing.T) {
	t.Run("v1", func(t *testing.T) {
		request := schema.AuthenticateRequest{
			MethodID: "api-key",
			Meta:     tracecontext.IntoMeta(nil, sampleContext()),
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"_meta"`) {
			t.Fatalf("_meta missing from frame: %s", encoded)
		}
		var decoded schema.AuthenticateRequest
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		got, ok := tracecontext.FromMeta(decoded.Meta)
		if !ok || got != sampleContext() {
			t.Fatalf("v1 round trip = %#v, %v", got, ok)
		}
	})

	t.Run("v2", func(t *testing.T) {
		omitted, err := json.Marshal(v2schema.NewSessionRequest{Cwd: "/workspace"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(omitted), `"_meta"`) {
			t.Fatalf("unset Nullable[Meta] serialized: %s", omitted)
		}

		request := v2schema.NewSessionRequest{
			Cwd:  "/workspace",
			Meta: v2schema.Nullable[v2schema.Meta]{Set: true, Value: tracecontext.IntoMeta(nil, sampleContext())},
		}
		encoded, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(encoded), `"_meta"`) {
			t.Fatalf("_meta missing from frame: %s", encoded)
		}
		var decoded v2schema.NewSessionRequest
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if !decoded.Meta.Set || decoded.Meta.Null {
			t.Fatalf("v2 Nullable[Meta] not decoded as set: %#v", decoded.Meta)
		}
		got, ok := tracecontext.FromMeta(decoded.Meta.Value)
		if !ok || got != sampleContext() {
			t.Fatalf("v2 round trip = %#v, %v", got, ok)
		}
	})
}
