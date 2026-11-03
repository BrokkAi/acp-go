# Draft ACP v2

`github.com/BrokkAi/acp-go/v2` (imported as `acpv2` in the examples below) is a
separate draft client for ACP v2, pinned to the official
`schema-v2.0.0-alpha.13` artifacts in [`schema/v2`](../schema/v2). Those
artifacts match Rust schema crate 1.15.1 byte for byte.

It supports initialization and version selection, `auth/login` and
`auth/logout`, session creation, resume, list, delete, and close, prompt
acceptance, cancellation, additional-directory capability checks, and typed
`session/update` dispatch.

There is intentionally no v1/v2 conversion layer. The v1 client, runner, and
agent runtime stay in the root packages for stable applications.

- [Prompt semantics](#prompt-semantics)
- [Generated types and patches](#generated-types-and-patches)
- [Client](#client)
- [Agent runtime](#agent-runtime)
- [Initialization rules](#initialization-rules)
- [Version routers](#version-routers)
- [MCP over ACP](#mcp-over-acp)

## Prompt semantics

ACP v2 changes what `session/prompt` means. It returns once the agent inserts
the user message into the conversation; completion is reported later through
an idle `state_update`.

- The response carries the required `messageId` of that user message.
  `Prompt` and `PromptContent` return it.
- The matching `user_message` update may arrive before or after the response.
- The agent runtime refuses to answer `session/prompt` when an implementation
  leaves `messageId` empty.

## Generated types and patches

The generated v2 types use `Nullable[T]` wherever the draft distinguishes an
omitted field from an explicit JSON `null`, such as message-content patches. A
`Nullable` is sent only when `Set` is true, for a value and for `null` alike.
(v1 uses it only for a few patch fields; see
[connection.md](connection.md#generated-schema-types).)

Draft-v2 upserts (`tool_call_update`, `terminal_update`, the message updates,
`session_info_update`, and `compaction_update`) have a generated `ApplyUpdate`
that applies a later update to stored entity state: omitted fields are
unchanged, `null` clears, and values replace.

Their top-level `_meta` is a metadata patch applied by
`schema.ApplyMetaUpdate`:

- `null` clears it.
- An object patches it by top-level key: keys set to `null` are removed, and
  the others are replaced whole, without merging nested objects.
- Members are left in the order Rust's `apply_meta_update` leaves them.

v1 is unchanged: there a `_meta` object still replaces the stored metadata as
a whole.

## Client

### Host adapters

`acpv2.HandlePermissions`, `HandleElicitation`, and `HandleClientHost` are
typed host adapters. Install them before `session/new`, because updates and
interactive requests can arrive before setup responses.

### Projecting updates

`acpv2.UpdateProjection` uses the `ApplyUpdate` helpers to store messages,
thoughts, tool calls, terminals, compactions, session info, and usage by kind
and ID, applying chunks in receive order, so live delivery and replay share
one code path.

- `Compactions` lists compactions in first-seen order.
- Notices need no client capability in v2. The projection keeps them as live
  events outside the timeline: `Notices` peeks and `DrainNotices` consumes.
- Plans are not projected.

`acpv2.NewNoticeUpdate`, `NewCompactionUpdate`, and
`NewCompactionSummaryChunkUpdate` build these updates.

### Session handles and tracking

`acpv2.NewSessionHandle` gives Rust-shaped prompt, configuration,
cancellation, and close commands for a session.

`acpv2.SessionTracker` owns connection-scoped update projections and
active-work state. Install it before session setup.

- Its per-session `Projection` sees every update, including replay.
- Connect with `acp.WithRequestObserver(tracker.TrackerRequests(nil))` so that
  permission requests' tool calls are applied in receive order with the
  updates around them.
- The projection keeps the latest `background` list (`Background`) and each
  tool call's and terminal's `capabilities`.
- `ResumeSessionFromStart` resets the session's projection first, so a full
  replay rebuilds it. It requires handlers to be installed before the resume
  request, and returns only after replay updates have been applied in wire
  order.
- Closing a session through its handle drops it from the tracker.

### Cancellation

- `CancelActiveWork` only sends `session/cancel`, like the reference SDK. Only
  the agent knows which work a permission request belongs to, so the agent
  cancels the cancelled foreground work's requests with `$/cancel_request` and
  leaves those for background work open.
- `acpv2.CancellablePermissions` answers a cancelled request with the
  cancelled outcome. Its `CancelPermissionRequests` cancels a session's
  requests locally on explicit request.
- `SessionHandle.CancelToolCall` and `CancelTerminal` send `tool_call/cancel`
  and `terminal/cancel` only while the entity advertises
  `capabilities.cancel`, and otherwise fail with `*acpv2.NotCancellableError`
  before writing. Their empty response only accepts the cancellation: the tool
  call's final status or the terminal's `exitStatus` confirms it.

### One-shot runner

`github.com/BrokkAi/acp-go/v2/runner` mirrors the reference SDK's one-shot
client:

- It ignores queued updates until the matching session reports an active
  state (`running`, `requires_action`, or an unknown state).
- It projects subsequent agent-message chunks and patch snapshots.
- It completes at the next `idle` update, whatever its stop reason. Repeated
  idle snapshots are not new completions.
- It cancels permission requests by default.
- Work that ends with the `error` stop reason returns the projected text
  together with a `*acpv2.WorkError` carrying the reported JSON-RPC error.
  `WorkResult.Err` gives the same for `ActiveWork`. Custom and unknown stop
  reasons are kept as received.

It manages the agent process the same way as the v1 runner; see
[runner.md](runner.md#agent-process-lifecycle).

```sh
go run ./examples/v2-one-shot-client \
  -command 'go run ./examples/minimal-agent-v2' \
  -prompt 'Say hello using draft ACP v2.'
```

## Agent runtime

Draft v2 agents use `github.com/BrokkAi/acp-go/v2/agent`. Its `Agent`
interface covers the baseline session methods, prompt acceptance,
cancellation, and typed permission and elicitation callbacks.

- Agents serve `tool_call/cancel` and `terminal/cancel` through the optional
  `ToolCallCanceller` and `TerminalCanceller`. Without them the runtime answers
  `-32602`, as the reference SDK does.
- `ForegroundPermissions` lets an agent stop waiting for its foreground
  permission requests on `session/cancel`, cancelling each with
  `$/cancel_request`, while requests for background work stay open.
- `acpv2.NewIdleStateUpdate` and `acpv2.NewErrorIdleStateUpdate` build idle
  updates.

```sh
go run ./examples/minimal-agent-v2
```

## Initialization rules

### Agent side

The draft-v2 agent runtime follows the v2 initialization rules:

| Request | Result |
|---|---|
| Below v2 | `-32022` (`Unsupported protocol version`) with `data: {"requested": 1, "supported": [2]}`, without reaching the agent |
| Newer than v2 | Selects v2 with what parses under v2; a missing `info` becomes name and version `"unknown"` |
| Exactly v2 | Must carry an `info` that parses, or gets `-32602`. Like the reference schema, a `_meta`, a capability, or an `info` title that does not parse is ignored |

The response always carries `protocolVersion: 2`, whatever the agent or
`InitializeResult` set.

- The client may repeat `initialize` after it completes and before any other
  request. Each repeat is negotiated like the first, and a failed one keeps the
  previous initialization. After another request, a repeat gets `-32600`.
- At most one `initialize` runs at a time. One that arrives while another is in
  progress gets `-32600` without reaching the agent.
- Any other request that arrives before an `initialize` has completed,
  including during a repeat, is refused with `-32600`
  (`ACP initialization must complete before ... can be used`).
- Notifications before initialization are dropped, and the agent's own
  requests and notifications fail until it completes.
- An initialization completes as its response is queued, so nothing the agent
  sends can precede that response.

These rules follow the order requests arrive on the wire, as in the reference
SDK, not the order their handlers run.

`acpv2.UnsupportedProtocolVersionError` builds the same `-32022` error for a
custom endpoint, and `acpv2.UnsupportedProtocolVersionData`,
`acpv2.SessionNotFoundData`, and `acpv2.SchemaError` decode the typed v2 error
data.

### Client side

`acpv2.Connection.SendInitialize` reports a v1 selection as
`InitializeOutcome.LowerVersion`, an `*acp.InitializationVersionMismatch` with
the agent's validated v1 response, rather than an error. `InitializeWithInfo`
returns the same value as its error. The connection stays open with its
previous initialization, if any, so the application decides whether to fall
back to a v1 client.

- As in the reference SDK, the client may repeat `initialize` until it sends
  another request. A failed repeat keeps the previous initialization.
- A response with a missing or invalid `protocolVersion` fails with `-32602`,
  and one selecting a version other than 1 or 2 with `-32600`, both with the
  reference SDK's data.

Like Rust's v2 initiator, a `v2.Connect` connection carries no other traffic
until an `initialize` completes, or while a repeat is in progress:

- Its own requests and notifications fail with `-32600`
  (``ACP initialization must complete before `<method>` can be used``) before
  anything is written. A `$/cancel_request` during an `initialize` is allowed.
- The agent's requests get the same error without running the handler, and the
  agent's notifications are dropped.
- Successful responses in either direction during that time become the same
  error.
- An `initialize` from the agent is refused after it is negotiated.

Every `initialize` the connection sends, even through the embedded
`acp.Connection.Call`, is negotiated and recorded, and one whose caller gave up
completes when its response arrives.

## Version routers

These packages serve or reach both versions from one endpoint. None of them
converts traffic between versions after selection.

### `agentrouter`

`github.com/BrokkAi/acp-go/agentrouter` serves one endpoint with explicit v1
and v2 implementations, like Rust's agent protocol router.

- A request for v2 or a newer version selects v2.
- When v1 is configured, everything else, including a malformed `initialize`
  or any other first message, goes to v1 unchanged.
- A v2-only router answers a request below v2 with `-32022`.
- Only the initial frame is normalized for the selected version.
- After a rejection the router keeps reading, and responses before
  initialization are ignored.

### `clientrouter`

`github.com/BrokkAi/acp-go/clientrouter` owns the transport needed for explicit
client-side selection. It starts the highest configured implementation and
follows the Rust fallback rules:

- reuse an existing v1-negotiated agent connection only for losslessly
  identical initialization,
- otherwise reconnect with fresh factories, and
- never turn a v2 rejection into a silent v1 retry.

### `proxyrouter`

`github.com/BrokkAi/acp-go/proxyrouter` negotiates `proxy/initialize` the same
way as the agent router: the highest configured version at or below the
requested one is selected, the exact-version frame is handed over unchanged
and any other is normalized, and `-32022` is returned when no configured
version fits.

It uses the proxy-chains RFD's method names, as Rust `agent-client-protocol`
5.0.0 does: a conductor sends `proxy/initialize` and wraps successor traffic in
`proxy/successor`. The pre-RFD underscore-prefixed names are not accepted.

## MCP over ACP

MCP-over-ACP is an explicit opt-in through `github.com/BrokkAi/acp-go/v2/mcp`
(draft v2) and `github.com/BrokkAi/acp-go/mcp` (v1), matching the Rust SDK's
separately gated `unstable_mcp_over_acp` feature. The v1 package is what
carries the native `acp` server transport that the stable v1 schema does not
model.

Both packages carry:

- **The request-scoped `mcp/message` mover.** `MessageClient` drives an
  agent-to-provider request and routes its request-scoped notifications.
  `MessageRouter` serves registered server IDs on the provider side, keeping
  inner MCP errors in the response carrier.
- **A standard-library MCP server framework.** A `Service` per operation with a
  `RequestContext` (server and request IDs, validated metadata, cancellation,
  scoped notifications); a typed `ToolRegistry` with caller-supplied JSON
  Schemas and allow/deny lists; `NewToolServer` with the reference SDK's
  `tools/call` error rules; and standalone serving with `Server.Serve`.
- **Automatic attachment.** `NewAttachedSession` and its load, resume, and fork
  siblings generate a unique `serverId`, register before the request, append
  the declaration, and release the registration on failure or
  `Attachment.Close`. `Attachment.HandleSessionSetup` attaches a server to
  every session a proxy forwards.
- **Session helpers** that accept MCP servers, including fork, alongside an
  optional system prompt.

See [unstable-facades.md](unstable-facades.md) for the full surface.
