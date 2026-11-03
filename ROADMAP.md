# acp-go roadmap

Goal: make acp-go the canonical standard-library-only Go SDK for Agent Client
Protocol, with v1 and draft-v2 behavior aligned to the
[reference Rust SDK](https://github.com/agentclientprotocol/rust-sdk).

## Reference baseline

The behavioral reference is Rust `agent-client-protocol` **5.0.0**, which pins
`agent-client-protocol-schema` **`=1.15.1`**. That schema crate publishes ACP
v1 **`schema-v1.29.1`** and draft ACP v2 **`schema-v2.0.0-alpha.13`**, and our
pinned stable and unstable artifacts are byte-for-byte identical to it. The
generated types cover the whole 1.15.1 surface; the 5.0.0 behavior built on
the newer types is tracked under "Schema crate 1.15.1 follow-ups" below.

## Status

| Area | Status |
|---|---|
| Schema generator, parity tests, exact Rust artifact pins | Done |
| v1 client surface | Done |
| v1 agent runtime and reference client host | Done |
| Fuzzing, licenses, release discipline, optional real-agent CI | Done |
| Draft-v2 schema, client, agent runtime, host adapters, one-shot runner | Done |
| Rust-parity hardening | In progress |

Completed roadmap phases are retained in `CHANGELOG.md`; this file now tracks
only remaining work.

## Rust-parity work already closed

- Separate v1 and v2 generated schema packages with no implicit conversion.
- v1 client and agent runtimes.
- Draft-v2 client and baseline agent runtime.
- Typed permission and elicitation host adapters.
- One-shot v2 runner with the reference update projection:
  ignore until an active state (`running`, `requires_action`, or an unknown
  state), apply message chunks and patch snapshots, complete at the next
  `idle`, and report the `error` stop reason as a typed failure.
- Explicit MCP-over-ACP opt-in through `github.com/BrokkAi/acp-go/mcp` and
  `github.com/BrokkAi/acp-go/v2/mcp`, mirroring Rust's separate
  `unstable_mcp_over_acp` feature boundary, including the request-scoped
  `mcp/message` mover.
- Exact JSON Schema integer formats, notably Rust-compatible `uint16`
  `ProtocolVersion` and `int32` error codes.
- Lossless JSON-RPC error objects, mirroring Rust's raw JSON-RPC error:
  `acp.RPCError` keeps omitted, `null`, and value `data` apart and preserves
  unknown members on both `Call` results and handler-returned errors (#66).
  The `v2` package builds and decodes the typed draft-v2 `ErrorData`
  variants (`-32022` and session-not-found `-32002`) and converts to the typed
  v2 `schema.Error` explicitly.
- Stable programmatic tool-call names in v1 and draft v2, surfaced by the
  reference client host with `tool_call_update` patch semantics.
- Required draft-v2 prompt acceptance identity: the client returns the
  response's `messageId` and the agent runtime refuses to answer
  `session/prompt` without one.
- Client initialization lifecycles like Rust's: the v1 client is unchecked
  and may initialize again at any time; the draft-v2 client may repeat
  initialization until it sends another request, and carries no other traffic
  in either direction until initialization completes.
- Protocol version negotiation per the v2 spec and Rust 5.0 (#62):
  - each endpoint selects the highest version it supports at or below the
    requested one; a missing or malformed `protocolVersion` is `-32602`,
  - the agent runtimes set the negotiated version on every initialize
    response, whatever the agent or `InitializeResult` returned,
  - the agent runtimes apply their lifecycle in wire order, on the read loop,
    like Rust's incoming actor,
  - the v1 agent runtime keeps v1's fallback-to-latest rule, normalizes a
    newer request to v1's shape, and, like Rust's unchecked v1 agent, passes
    every `initialize`, repeated or concurrent, to the agent,
  - the draft-v2 agent runtime answers a request below v2 with `-32022` and
    `{"requested", "supported": [2]}` before the agent runs, accepts a newer
    request with what parses under v2 (`info` defaults to `"unknown"`), reads
    an exact v2 request as leniently as the reference schema, and negotiates
    a repeat sent before any other request like the first one,
  - the draft-v2 client reports a v1 selection as a typed lower-version
    outcome, keeps the previous initialization after a failed repeat, and
    fails locally with Rust's `-32602`/`-32600` errors for an invalid or
    unsupported selected version.
- Explicit agent protocol router, like Rust's:
  - route a request for version 2 or newer to v2, normalizing a newer one,
  - when v1 is configured, route everything else to v1 unchanged, including
    a malformed initialize and any other first message,
  - normalize a v2 initialize request to v1 when only v1 is configured,
  - answer `-32022` when no configured version fits, keep reading after
    rejections, and ignore responses before initialization,
  - never convert traffic after initialization.
- JSON-RPC batches rejected on v1 and v2, matching the ACP v2 transports
  "No batches" rule and Rust 5.0 (#68). This is decided: there is no batch
  support or compatibility path on either version.
  - any array, including `[]`, gets one `-32600` (`Invalid request`) error
    with `id: null` and the Rust error data, and none of its entries run,
  - the connection, agent protocol router, and proxy protocol router keep
    reading after the rejection,
  - a line that is not valid JSON, judged as `serde_json` judges it (lone
    surrogate escapes, float64 overflow, and nesting 128 deep are invalid),
    gets one `-32700` error with `id: null` and the line as data, with the
    same keep-reading behavior, so `[`-prefixed garbage is a parse error,
  - both answers are queued before the next frame is handled, so nothing can
    overtake them,
  - the HTTP transport answers an array `POST` with `400` and the same
    JSON-RPC body without creating a connection; its client treats an array
    from the server as malformed.
- A connection answers instead of tearing down, as Rust 5.0 does (#73):
  - valid JSON that is not a JSON-RPC message gets `-32600` with `id: null`,
    and a malformed response-shaped value is dropped,
  - a null request id runs on v1 and version-neutral connections and gets
    `-32600` with `id: null` on v2 (`acp.WithProtocolVersion`, set by
    `v2.Connect` and the draft-v2 agent runtime); `agentrouter` answers a
    v2-selecting first frame with a null id the same way and keeps reading
    (#62),
  - handler panics are recovered and answered with `-32603`, notification
    handler errors are reported rather than fatal, and incoming requests are
    not limited.
- Streamable HTTP server parity with Rust `agent-client-protocol-http` 5.0.0
  (#65), apart from WebSocket (#64):
  - a connection stream and per-session streams under the Rust session-naming
    rules (rust-sdk #483), with the 16-stream cap on unnamed sessions, `409`
    for a second subscriber, and stream removal on `session/close` and
    `session/delete`,
  - agent frames buffered per stream until the client opens it,
  - `DELETE` returning `202`, `ServerOptions` (path, CORS disabled by default,
    `/health`, 16 MiB body limit with `413`), and the Rust CORS header
    allowlist without credentials,
  - a client that opens session streams the way the Rust client does and
    sends `DELETE` on close.
- Rust-shaped v2 session command surface:
  - reusable session-update projection storing messages, thoughts, tool
    calls, terminals, compactions, session info, and usage with
    omitted/null/value patch semantics and top-level `_meta` metadata patches
    (`ApplyMetaUpdate` and generated `ApplyUpdate` upserts, #67), shared by
    live delivery and replay, with permission subjects applied in receive
    order and full replays rebuilding the projection, plus notices kept as
    live events,
  - per-session active-work tracker using active→idle completion on any
    idle stop reason (including `error`, custom, unknown, and none), keeping
    the typed reason and error details and ignoring repeated idle snapshots
    (#59),
  - explicit `SessionHandle`,
  - prompt acceptance, config-option replacement, and close commands,
  - pending permission cancellation,
  - exact Rust `CancelActiveWork` notification behavior plus an optional
    cancellation-completion barrier that only `cancelled` satisfies,
  - from-start resume replay after preinstalled handlers.
- Explicit client protocol connector:
  - select the highest configured v1/v2 client implementation,
  - start with an individual initialize request,
  - reuse the existing agent connection only when v2 initialization is
    losslessly representable as the configured v1 initialization,
  - reconnect with fresh factories when parameters differ,
  - surface v2 rejection without silently retrying as v1.
- Negotiating proxy protocol router (Rust #464):
  - negotiate `_proxy/initialize` exactly as the agent router negotiates
    `initialize`,
  - validate an exact-version request and hand its complete initial frame
    over unchanged,
  - normalize a request for another version for the selected implementation.
- Unstable schema surfaces from the exact Rust schema crate 1.15.1 unstable
  artifacts:
  - v1 and v2 generated bindings in explicit import-only packages,
  - LLM providers,
  - plan operations,
  - session fork,
  - session archive/unarchive (with the `session/list` archived filter),
    status, and title,
  - client-provided system prompts on the session lifecycle requests, with
    capability-gated client facades and agent-side handlers that `Install`
    serves through runtime overrides (#58),
  - auth status (`auth/status` and the `auth/status_update` notification),
    with a capability-gated client facade, a notification adapter, and an
    agent-side handler and sender (#72),
  - MCP-over-ACP (session options and the request-scoped `mcp/message` mover),
  - end-turn token usage,
  - bidirectional `mcp/message` request/notification registry metadata,
  - generated types, without facades yet, for subagents and
    proxy chains (`proxy/initialize` and `proxy/successor`, registered as
    `SideProxy`).
- Session notices and compaction on the stable surfaces (#57): v1 client
  capability helpers, update constructors, `acp.SessionProjection`, and the
  agent runtime's capability gate; draft-v2 constructors and notice and
  compaction projection in `acpv2.UpdateProjection`, with no capability gate.
  `unstable.Projection` delegates both to the stable projection.
- Optional credential-backed v2 integration coverage: the draft-v2 runner is
  exercised against a real adapter when it advertises v2, and the CI integration
  job runs the v1 and v2 tests together.
- Test harness (`acptest`): an in-memory duplex transport, deterministic typed
  prompt commands, framing helpers, and v1/draft-v2 fake agents shared by the
  agent, proxy, and client router tests.
- Schema pin-change workflow (`docs/schema-pin-workflow.md`): regenerate both
  artifacts and review the generated API diff before either pin moves.
- Semantic validation at the typed facade boundaries
  (`docs/validation-boundaries.md`): absolute paths, required identifiers,
  media types, and URIs are checked before a request reaches the wire, while
  unknown tags and raw `_meta` keep decoding.
- Rust-parity decoding (#82): the generated decoders honor the schema's
  `x-deserialize-*` hints and enforce required fields and union tags, checked
  per pin against outcomes captured from the Rust schema crate
  (`docs/schema-pin-workflow.md`).

## Remaining Rust-parity gaps

### 1. Optional typed facades for unstable methods

The typed facade surface is delivered: client facades and agent dispatch for
providers and session fork on v1 and draft v2 (#61), session
archive/status/title on v1 and draft v2 (#71), client-provided system prompts
on every session lifecycle request (#58), auth status on v1 and draft v2
(#72), the session-update
projection helper, and the MCP session options, including the draft-v2 MCP
fork, all live in [docs/unstable-facades.md](docs/unstable-facades.md)'s
packages (`unstable`, `v2/unstable`, `mcp`, `v2/mcp`). Agents serve the
unstable methods with `Install`, which sets `Handle`, `Advertise`, and the
runtime `Overrides` on the v1 or draft-v2 runtime together, behind its
initialization gate and capability checks; the overrides are `session/list`
with the archive filter and `session/new`, `session/load`, and
`session/resume` with the client-provided system prompt. `Runtime.Overrides` is the one general
mechanism for serving the unstable form of a stable method.

The native MCP-over-ACP binding is delivered (#39): `mcp` and `v2/mcp` expose
the standard-library message mover decided above, with `MessageClient` on the
consumer side and `MessageRouter` on the provider side. Inner MCP errors stay
in the response carrier; binding failures use the protocol's outer error codes
(`-32602`, `-32800`, `-33000`, `-33001`, `-33002`). See
[docs/unstable-facades.md](docs/unstable-facades.md).

The MCP server framework is delivered (#63): a standard-library counterpart of
Rust's `mcp_server` module and the `rmcp_3` builder's tool semantics, with
`Server`/`Service`, a typed `ToolRegistry`, standalone serving, and automatic
per-session and proxy-wide attachment for v1 (`session/new`, `load`, `resume`,
`fork`) and draft v2 (`session/new`, `resume`, `fork`).

### 2. Rust ecosystem crate ports

[docs/rust-ecosystem-parity.md](docs/rust-ecosystem-parity.md) evaluates the
reference workspace crates that have no Go equivalent and records one decision
per crate. Every port is done: the test harness (`acptest`), the trace viewer
(`traceviewer`), and the cookbook of client, proxy, and agent patterns
(`cookbook`, #22).

Two crates are recorded as won't port: the `agent-client-protocol-rmcp` bridge,
which needs a third-party MCP SDK, and the proxy-chain conductor, which is an
application with no Go consumer. The pinned unstable schema now models the
proxy-chain wire shape as `proxy/initialize` and `proxy/successor`; moving
`proxyrouter` off the old `_proxy/*` names is tracked in #69. Every port stays
standard-library-only and behind its own package or command.

### 3. Schema crate 1.15.1 follow-ups

The 1.15.1 pin landed the generated types; the behavior Rust 5.0.0 builds on
them is tracked separately: subagents (#60), the proxy-chain method rename
(#69), and v2 background work with `tool_call/cancel`/`terminal/cancel`
(#70). Unstable session archive/status/title (#71), client-provided system
prompts (#58), `-32022` version negotiation (#62), and unstable auth status
(#72) are delivered.

## Guardrails

- Standard library only; the dependency policy in
  [licenses/README.md](licenses/README.md) enforces it.
- Open-world enums and preserved unknown tags must continue to decode.
- No silent fallbacks or v1/v2 conversion.
- Capability-gated methods and session-update variants must fail before
  writing to the wire.
- Generated files must remain byte-deterministic for both pinned artifacts.
- Generated decoders must accept and reject what the pinned Rust schema crate
  does; each package's `TestDeserializeHints` checks it.
- The module version is independent of the negotiated wire version.
