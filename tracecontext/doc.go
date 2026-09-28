/*
Package tracecontext carries W3C Trace Context through the ACP _meta channel.

The ACP extensibility conventions reserve the root-level _meta keys
"traceparent", "tracestate", and "baggage" for W3C Trace Context, so MCP and
OpenTelemetry integrations can correlate ACP requests, responses, and
notifications with the surrounding distributed trace. The helpers here set and
extract those keys on the generated metadata maps (schema.Meta and v2.Meta)
without adding dependencies; the values themselves are opaque strings that the
package never parses except where ValidTraceparent is called.

v1 messages carry _meta directly as schema.Meta, which is assignable to and
from map[string]any:

	meta := tracecontext.IntoMeta(notification.Meta, tracecontext.Context{Traceparent: traceparent})
	notification.Meta = meta

Draft-v2 messages wrap _meta in Nullable[v2.Meta], which distinguishes an
omitted field from an explicit JSON null. Populate the value and mark the field
set:

	notification.Meta = v2.Nullable[v2.Meta]{
		Set:   true,
		Value: tracecontext.IntoMeta(nil, tracecontext.Context{Traceparent: traceparent}),
	}

Reading is symmetric for both versions:

	if tc, ok := tracecontext.FromMeta(request.Meta.Value); ok {
		_ = tc.Traceparent
		_ = tc.Tracestate
		_ = tc.Baggage
	}
*/
package tracecontext
