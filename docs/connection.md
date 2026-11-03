# Connection and wire behavior

`acp.Connection` is the JSON-RPC transport under every role in this module: the
v1 client, both agent runtimes, the routers, and the draft-v2 client (whose
typed facade adds its own rules; see [draft-v2.md](draft-v2.md)). This page
describes how it reads, writes, dispatches, and fails. Throughout, the
reference is the Rust SDK `agent-client-protocol` 5.0.0.

## Lifecycle

```go
connection := acp.Connect(agentStdout, agentStdin, handleRequest, handleNotification)
defer connection.Close()
// ... initialize, create sessions, prompt ...
err = connection.Shutdown(ctx)
```

- `Connect` owns both streams and closes them.
- `Shutdown` is the reference SDK's orderly end of a successful run. It sends
  nothing new, writes every frame already queued, then closes the agent's
  stdin. Its context bounds the drain.
- `Close` aborts the connection: it drops frames not yet written, cancels the
  running handlers, and waits for them to return.

## Handlers

- **Notifications** run in wire order, before responses. They must return
  promptly and must not call back into the connection.
- **Requests** run concurrently and must honor cancellation.
- Frames have no size limit, and the number of in-flight incoming requests is
  not limited, as in the reference SDK.

## Extensions and cancellation

- `Call` and `Notify` send any method, which is how extensions are reached.
- `CancelRequest` sends a typed `$/cancel_request` for one outstanding
  request, preserving the peer's string or numeric ID.

## Bad input never closes the connection

One bad message or failing handler never closes the connection, as in the Rust
SDK. Only invalid UTF-8, an oversized frame, or a transport failure does.

| Input | Answer |
|---|---|
| A line that is not valid JSON (judged as `serde_json` judges it) | `-32700` with `id: null` and the line as `data` |
| A JSON-RPC batch (any JSON array, including `[]`) | One `-32600` (`Invalid request`) with `id: null`; none of its entries run |
| Valid JSON that is not a JSON-RPC message | `-32600` with `id: null` |
| Something shaped like a response (no `method`, with `result` or `error`) that matches no call | Dropped, without completing any call |
| A request whose handler panics | `-32603`, or `-32800` if the request was cancelled |
| A request with a null `id` | Runs on the version-neutral connection and on v1; with `acp.WithProtocolVersion(2)` (set by `v2.Connect` and the draft-v2 agent runtime) it gets `-32600` with `id: null` |

Batches are rejected on both protocol versions, matching the ACP v2
transports rule and the reference SDK. Send each message individually.

A notification handler's error is not fatal. Handler errors and recovered
panics go to the `acp.WithErrorHandler` hook; without one, panics are logged.

## Frame encoding

Every frame the connection writes is serialized as the reference SDK's
`serde_json` writes it, the same encoding the HTTP transport uses. This covers
params, results, error data, and forwarded `json.RawMessage` values alike.

- `<`, `>`, and `&` are not escaped; U+2028 and U+2029 are written raw.
- Other escapes in forwarded JSON (such as `a` or `\/`) are decoded.
- Numbers are formatted as `serde_json` formats them.
- A repeated object member keeps its first position with its last value.
- Object members keep the order `encoding/json` gives them.

JSON the reference SDK cannot hold, such as invalid UTF-8 or a lone surrogate
escape in a `json.RawMessage`, is never written: `Call` fails, `Notify` drops
it and reports it, and a handler result is answered with `-32603`.

## `*acp.RPCError`

`*acp.RPCError` is the JSON-RPC error object as it appears on the wire, kept
losslessly like Rust's raw JSON-RPC error. `Call` returns it for an error
response, and a handler returns one (directly or wrapped) to choose its error
response.

- `Data` distinguishes an omitted `data` (nil), `"data": null`, and a value.
- `Extra` keeps every other member, such as extension fields, so a relay can
  re-emit the object unchanged. A handler that returns a `*RPCError` obtained
  from `Call` therefore forwards the peer's `Extra` members too.
- Members after `code`, `message`, and `data` are re-emitted in the order they
  arrived in, as Rust's relays write them. Members of an `Extra` map built in
  Go are written in sorted key order.
- `code`, `message`, and `data` always come from the typed fields, never from
  `Extra`, so no member is ever encoded twice.

As in Rust, decoding rejects an error object whose `code` or `message` is
missing, null, or mistyped, whose `code` is not an `int32` integer, or that
repeats `code`, `message`, or `data`. A handler error whose `Code` does not
fit `int32` is answered with `-32603`.

`acp.IsAuthRequired` and `acp.IsResourceNotFound` classify by code alone.
`RPCError.SchemaError` is the explicit conversion to the typed v1
`schema.Error`, and drops `Extra`, as Rust's `into_acp_error` does.

## `_meta` and untyped JSON

`_meta` is a `schema.Meta`, a `jsonvalue.Object` from
`github.com/BrokkAi/acp-go/jsonvalue`, which holds untyped JSON exactly as the
reference SDK's `serde_json::Value` does:

- An object keeps its members in wire order (a repeated key once, at its first
  position, with its last value).
- Integers that fit `i64` or `u64` stay exact `int64` or `uint64` values.
- Other numbers are `float64` values written in serde_json's form (`1.0` stays
  `1.0`).

Decoding a `_meta` and encoding it again writes exactly what Rust writes.
Build one with `schema.NewMeta`, which sorts a map's keys, or as a literal to
choose the order, and read it with `Get`:

```go
meta := schema.Meta{{Key: "traceparent", Value: traceparent}}
meta.Set("acme.dev/attempt", 2)
if attempt, ok := request.Meta.Get("acme.dev/attempt"); ok {
	_ = attempt.(int64) // decoded integers are int64, or uint64 above i64
}
```

## Trace context

`github.com/BrokkAi/acp-go/tracecontext` sets and extracts the root-level
`_meta` keys `traceparent`, `tracestate`, and `baggage` that the ACP
extensibility conventions reserve for W3C Trace Context, so MCP and
OpenTelemetry integrations can correlate ACP requests, responses, and
notifications with the surrounding trace.

- `IntoMeta` and `FromMeta` operate on the generated `schema.Meta` and
  `v2.Meta` objects, and on the `Nullable[Meta]` fields that wrap them in draft
  v2 and in v1 `compaction_update`.
- `ValidTraceparent` checks the fixed W3C grammar without parsing the opaque
  `tracestate` or `baggage` values.

The package is standard-library only.

## Generated schema types

`github.com/BrokkAi/acp-go/schema` provides typed constants, request and
response structs, notifications, and a method registry for every message in
the pinned [ACP JSON Schema release](https://github.com/agentclientprotocol/agent-client-protocol/releases)
(`schema-v1.29.1` at the time of writing), including session updates, content
blocks, tool calls and their programmatic names, permissions, terminals,
elicitation, config options, session notices, and compaction. The pins are the
exact artifacts of Rust schema crate 1.15.1, which the reference SDK pins.

The package is generated by `cmd/acpgen` and guarded by round-trip parity
tests, so tracking a new schema release is a reviewed regeneration rather than
hand transcription. See [schema-pin-workflow.md](schema-pin-workflow.md) for
the update workflow.

All four generated packages (`schema`, `schema/v2`, and their `unstable`
counterparts) decode as the Rust schema crate does, following the schema's
`x-deserialize-*` hints:

- A malformed optional field reads as not reported. A malformed `Nullable`
  patch reads as unset, while an explicit `null` stays null.
- List items that do not decode are skipped where the schema says so.
- A missing required field or union tag, an unknown value of a closed v1 enum,
  or a value outside a union's arms fails.
- Protocol versions and error codes use the schema's exact integer widths. In
  particular, `ProtocolVersion` is a `uint16`, so strings and values above
  65535 fail to decode.

See [Decode rules](schema-pin-workflow.md#decode-rules) for the full list.

`Nullable[T]` distinguishes an omitted field from an explicit JSON `null`. A
`Nullable` is sent only when `Set` is true, for a value and for `null` alike.
The stable v1 types use it only for the patch fields Rust models with
`MaybeUndefined`: `compaction_update`'s `summary`, `error`, and `_meta`, and
`session_info_update`'s `title` and `updatedAt`. The draft-v2 types use it more
widely; see [draft-v2.md](draft-v2.md#generated-types-and-patches).
