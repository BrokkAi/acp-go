# Changelog

All notable changes to this project are documented in this file. Release
entries use [Semantic Versioning](https://semver.org/); during the `0.x`
series, minor releases may contain breaking API changes.

## Unreleased

### Added

- A standard-library MCP server framework in `mcp` and `v2/mcp`, mirroring
  Rust `agent-client-protocol` 5.0.0's `mcp_server` module and the tool-call
  rules of `agent-client-protocol-rmcp`'s `rmcp_3` builder (#63). The types are
  shared, so one `Server` attaches to v1 and v2 sessions:
  - `Server`, `NewServer`, `Service`, `ServiceFunc`, `Request`, `Outcome`,
    `Error` (an inner MCP error), and `RequestContext` with the server ID,
    logical request ID, validated metadata, outer and operation cancellation,
    and operation-scoped `Notify`. `context.Cause` on an operation's context
    reports `ErrRequestCancelled`, `ErrServerUnavailable`, or
    `ErrConnectionClosed`.
  - `ToolRegistry`, `Tool`, generic `NewTool`, `ToolResult`, `TextContent`,
    `UnknownToolError`, and `NewToolServer` (`server/discover`, `tools/list`,
    `tools/call`, plus `initialize`/`ping` for older standalone clients).
    Unknown or disabled tools are an inner `-32602`; tool errors and arguments
    that fail the caller-supplied input schema are `isError` results; a
    cancelled call is an inner `-32603`; `serverInfo` defaults to version
    `0.0.0`.
  - `Server.Serve` for a direct MCP client without an ACP session.
  - `MessageRouter.Attach`, `Attachment`, and the attached-session helpers
    `NewAttachedSession`, `LoadAttachedSession`, `ResumeAttachedSession`, and
    `ForkAttachedSession` (v1) and `NewAttachedSession` and
    `ResumeAttachedSession` (v2): unique `mcp-server:<uuid>` IDs, registration
    before the request, release on failure or `Attachment.Close`, and, for
    v2, `-32602` before writing when the request already declares the name.
  - `Attachment.HandleSessionSetup` and `Attachment.AppendDeclarations` for
    connection-scoped proxy attachment, appending to the raw params so unknown
    fields survive.
  - `ErrorCodeResourceLimitExceeded` (`-33000`), `ErrorCodeServerUnavailable`
    (`-33001`), and `ErrorCodeBackendFailure` (`-33002`), and
    `MessageRouter.SetMaxActive`, which answers `-33000` past its limit.
  - `MessageRouter.UnregisterAndWait` and `MessageRequest.RawParams`.
  - v1 `mcp.LoadSession`.
- `acp.Connection.InputClosed`, closed when the connection stops reading
  frames. On a clean EOF it closes before `Done`, which still waits for
  in-flight handlers, so long-running handlers can stop instead of waiting on
  a peer that is gone.

- `acp.RPCError` gains `Extra`, which keeps the members of a JSON-RPC error
  object other than `code`, `message`, and `data`, matching the reference
  SDK's raw JSON-RPC error (#66). Errors returned by `Call` keep the peer's
  extension members, and handler-returned errors encode theirs. Extra
  entries named `code`, `message`, or `data` are never encoded, so an error
  object cannot carry a member twice.
- `RPCError.SchemaError` converts to the typed v1 `schema.Error`, dropping
  `Extra`, as an explicit boundary like Rust's `into_acp_error`.
- Stable v1 (`schema`) and draft v2 (`schema/v2`) generate session notices
  (`Notice`, `NoticeSeverity`, the `notice` session update) and compaction
  (`CompactionId`, `CompactionStatus`, `CompactionUpdate`,
  `CompactionSummaryChunk` and their session-update variants). v1 also gains
  `ClientSessionCapabilities.Notices` and `.Compaction`.
- Draft v2 generates background work (`BackgroundWork` with terminal and
  tool-call variants, the `background` snapshot on the idle, running, and
  requires-action state updates, and on the unstable unknown state update),
  the `tool_call/cancel` and `terminal/cancel` methods with their request, response, and capability types,
  `TerminalUpdate.Size`/`.Capabilities`, `ToolCallUpdate.Capabilities`,
  `ToolCallStatusPaused`, `ErrorCodeUnsupportedProtocolVersion` (`-32022`), the
  defined error data (`ErrorData`, `UnsupportedProtocolVersionErrorData`,
  `SessionNotFoundErrorData`), `ErrorStopReason`, `StopReasonError`, and
  `_meta` on `DiffPatch`, `Icon`, `ToolCallPermissionSubject`, and
  `CancelledPermissionOutcome`.
- The unstable packages generate client-provided system prompts
  (`SystemPrompt`, `SystemPromptMode`, `SessionSystemPrompt*Capabilities`, and
  `systemPrompt` on the session lifecycle requests), the subagent additions
  (`ErrorStopReason`, `SubagentUpdate.Cwd`/`.AdditionalDirectories`,
  `SessionMessage.SenderMessageID`/`.RecipientMessageID`, v2
  `BackgroundSubagent`), `auth/status` and `auth/status_update`,
  `session/archive`, `session/unarchive`, `session/status`, and
  `session/set_title`, and proxy chains (`proxy/initialize`, `proxy/successor`,
  `InitializeProxyRequest`/`Response`, `ProxySuccessor*`, and the `Proxy*`
  envelopes). These are types only; facades follow in their own issues.
- Draft v2 metadata patches and entity upserts (#67), mirroring Rust's
  `apply_meta_update` and `ToolCallUpdate`/`TerminalUpdate::apply_update`:
  - `schema/v2` and `schema/v2/unstable` generate `ApplyMetaUpdate`, the
    Metadata Patches rule for the top-level `_meta` of upserts: omitted leaves
    stored metadata unchanged, `null` clears it, and an object patches it by
    top-level key, where a key set to `null` is removed (and never stored,
    even on a first update), any other value replaces the key whole, and
    nested objects are not merged. Any Go value that encodes as JSON `null`
    removes its key, and the stored and update maps are never modified.
  - They also generate `ApplyUpdate` on every upsert: `ToolCallUpdate`,
    `TerminalUpdate`, `UserMessage`, `AgentMessage`, `AgentThought`,
    `SessionInfoUpdate`, `CompactionUpdate`, and, in `schema/v2/unstable`,
    `SubagentUpdate` and `SessionMessage`. Omitted fields keep the stored
    value, `null` is stored as an explicit clear, other values replace it, and
    `_meta` is patched with `ApplyMetaUpdate`. The generator fails if an
    upsert gains a field that cannot represent `null`.
  - `acpv2.UpdateProjection` now stores user messages, Agent messages and
    thoughts, tool calls, terminals, compactions, session info, and usage,
    keyed by kind and ID, with `UserMessage`, `AgentMessage`, `AgentThought`,
    `ToolCall`, `Terminal` (`TerminalState` keeps the latest output snapshot
    as received and the decoded current bytes), `Compaction`, `SessionInfo`,
    `Usage`, and `Timeline` (first-seen order) accessors. Plans, notices, and
    the draft subagent and session-message entities are not projected.
    Upserts use the generated `ApplyUpdate`; message, tool-call content,
    terminal output, and compaction summary chunks append to the current
    content, as the spec requires. `ApplyPermissionRequest` applies a
    `tool_call` permission subject's `toolCall` like a `tool_call_update`.
    Accessors return snapshots that later updates never change; they share
    storage with the projection and must not be modified. `Text` is
    unchanged.
  - `acpv2.SessionTracker` keeps a per-session `Projection` that sees every
    update, including replay and updates before `running`, and that
    `BeginWork` does not reset. `TrackerRequests` applies permission subjects
    to it on the connection's read loop, in receive order relative to
    `session/update`, before the permission handler runs. `ResetProjection`,
    `Forget`, and `ResumeSessionFromStart` (which resets the session first,
    so a full replay rebuilds rather than merges) manage its lifetime, and
    `SessionHandle.Close` forgets the closed session. `ActiveWork` keeps
    only the Agent text it reports.
  - `acp.ConnectObserving` and `acpv2.ConnectObserving` take an optional
    `acp.RequestObserver` that sees each incoming request in wire order, on
    the read loop, before its handler starts.
- Method registries gain `SideProxy` for the proxy-chain methods.
- `cmd/acpgen` models the 1.15.1 schema shapes: the `proxyMethods` meta group
  and `x-side: "proxy"`; a tagged union flattened into its parent object
  (`IdleStateUpdate.stopReason`), generated as an open string enum plus the
  variant payload's fields on the parent, with a codec that follows the
  union's wire rules; untyped "any other value" union arms,
  preserved verbatim in an `Other` variant (`ErrorData`) or as `any`
  (`ElicitationContentValue`); defs that are exactly another def, generated as
  aliases (`InitializeProxyRequest = InitializeRequest`); and typeless method
  payloads, generated as `json.RawMessage` aliases (`ProxySuccessorResponse`).
- `v2/unstable` gains the draft-v2 provider and fork facades (#61):
  `ListProviders`, `SetProvider`, and `DisableProvider`, gated on
  `capabilities.providers`, and `ForkSession`, the counterpart of the
  reference SDK's `v2::ForkSessionBuilder`, gated on
  `capabilities.session.fork`. `ForkSession` requires a source session ID and
  an absolute `cwd`, gates additional directories on
  `capabilities.session.additionalDirectories`, rejects inline MCP servers,
  requires a non-blank forked session ID in the response, and forwards
  `systemPrompt` unchanged. All checks fail before writing to the wire.
- `v2/unstable.Handle`, `ProviderHandler`, and `ForkHandler` serve those
  methods on the agent side, and `v2/unstable.Advertise` announces the
  implemented capabilities in the initialize response. v1 `unstable` gains
  `Advertise` too.
- `agent.Runtime` and `v2/agent.Runtime` gain `Extensions`, an `acp.Handler`
  that serves every request the runtime does not own, behind the runtime's
  initialization gate, and `InitializeResult`, a hook that replaces the stable
  initialize response on the wire with an extended one, such as the unstable
  response from `Advertise`. The runtime validates the stable view of the
  value it sends. Installing `Handle` and `Advertise` there serves the
  unstable methods with the reference SDK's layering.
- `v2.Connection.InitializeExtended` runs the validated `InitializeWithInfo`
  and also decodes the result into an extended type, such as the unstable
  `InitializeResponse` the `v2/unstable` and `v2/mcp` facades gate on.
- `v2/mcp.ForkSession` forks a draft-v2 session with MCP servers, applying the
  same fork checks plus the per-transport capability checks of `NewSession` and
  `ResumeSession`, including the native `acp` transport, gated on
  `capabilities.session.mcp.acp`.

### Changed

- `mcp.MessageRouter` and the v2 `MessageRouter` now cancel an active request
  when the connection's input reaches EOF, answer a request cancelled by
  `Unregister` with `-33001` instead of `-32800`, report a service error as
  `-33002` with the message `MCP backend failure` and the error text in
  `data`.
- **Breaking:** a `MessageRouter` no longer rebinds a server ID after
  `Unregister`; the MCP-over-ACP RFD forbids reusing a server ID on a
  connection.
- v1 `mcp.NewSession` sends `"mcpServers": []` instead of `null` when no
  servers are given; the v1 schema requires the array.
- v1 `unstable.Handle` follows the reference SDK's handler chain and error
  shapes, the same as `v2/unstable.Handle`: an unstable method the handler
  does not implement now goes to `next` instead of answering method-not-found
  there; `-32601` is `Method not found` with the method name as data;
  malformed params answer `-32602` `Invalid params` with data `{"error",
  "json", "phase"}` echoing the received params; and a fork response with a
  blank session ID answers `-32603` `Internal error` instead of being sent.
- v1 `unstable.ForkSession` now requires
  `sessionCapabilities.additionalDirectories` before sending additional
  directories, as the protocol requires.
- The v1 and draft v2 agent runtimes now send their initialization lifecycle
  refusals in the reference Rust SDK's wire shape: code `-32600`, message
  `Invalid request`, and the explanation as a JSON string in `data`. A request
  sent before initialization completes now carries "ACP initialization must
  complete before `<method>` can be used", replacing the `agent is not
  initialized` message. A repeated `initialize` now carries "ACP initialization
  cannot be repeated after any other request; reconnect to initialize again",
  replacing `agent is already initialized`. Clients that matched on the old
  messages must match on `data` instead (#74).
- **Breaking:** the new `Extra` field means unkeyed `RPCError` literals such
  as `acp.RPCError{-32602, "bad", nil}` no longer compile; use keyed fields.
  Keyed literals keep working and encode as before.
- **Breaking:** `RPCError` now has its own `MarshalJSON` and `UnmarshalJSON`.
  A struct that embeds `acp.RPCError` inherits them, so the outer struct's
  other fields no longer appear in its JSON.
- `acp.RPCError` decodes as strictly as the reference SDK: an error object
  whose `code` or `message` is missing, null, or mistyped, whose `code` is not
  an integer in the `int32` range (including `1.0`, `1e3`, and `-0`), or that
  repeats `code`, `message`, or `data` is rejected. A peer error response like
  that is now a malformed frame. Member names match exactly, as JSON-RPC
  requires: a differently cased `"Code"` is an extension member, not the code.
  Decoding into an existing value replaces it instead of merging. Re-encoding
  is semantically unchanged; members after `code`, `message`, and `data` are
  emitted in sorted key order.
- A handler error whose `Data` or `Extra` is not valid JSON, or whose `Code`
  does not fit `int32`, is answered with `-32603` instead of failing to send
  any response.
- `runner` and `v2/runner` manage the agent process as the Rust SDK's
  `AcpAgent` does (#75). They used to kill its process group at once.
  - After a successful run they close the agent's stdin and wait up to one
    second for it to exit. On Unix an agent still running then gets `SIGTERM`
    to its process group and `SIGKILL` two seconds later. On Windows its
    process tree is killed.
  - A failed or cancelled run still kills the process group at once.
  - Any process left in the group after the agent exits is killed.
  - If the agent exits mid-run, the runner kills its process group and ends
    the connection within a second, so a descendant holding stdout open can no
    longer hang the run.
  - A failure exit, during the run or after stdin closes, is returned as the
    new `AgentExitError` (also in `v2/runner`) with the exit status and stderr
    tail. It replaces the run error, or comes alongside the result of a
    successful run. A `SetupError` keeps its phase and wraps it.
- `examples/minimal-client` closes the agent's stdin and waits up to a second
  before killing it.
- Schema pins move from ACP v1 `schema-v1.24.1` / draft v2
  `schema-v2.0.0-alpha.7` to `schema-v1.29.1` / `schema-v2.0.0-alpha.13` and
  their `-unstable` artifacts: exactly Rust schema crate
  `agent-client-protocol-schema` 1.15.1, which the reference Rust SDK
  `agent-client-protocol` 5.0.0 pins. The docs now name 5.0.0 as the
  behavioral reference.
- Breaking (draft v2, both v2 packages):
  - The top-level `StopReason` def is gone upstream; `IdleStateUpdate` now
    flattens a stop-reason union. `StopReason` and its constants are
    regenerated from that union with the same names, and the type stays an
    open string. The `error` variant's details arrive in the new
    `IdleStateUpdate.Error`, which is decoded and encoded only with
    `StopReasonError`. A custom or future reason keeps its other fields in
    `IdleStateUpdate.StopReasonFields` and re-encodes them, as Rust does.
  - The idle, running, requires-action, and unstable unknown state updates
    read a malformed `stopReason`, `error`, `background`, `_meta`, or `usage`
    as not reported, like Rust's `DefaultOnError`, instead of failing the whole
    `session/update`. A malformed idle update therefore still completes
    `ActiveWork`.
  - `AvailableCommand` is a union discriminated by `type`: the `command`
    variant is `Command{Name, Description, Input, Meta}` and other kinds are
    preserved in `Other`.
  - `Error.Data` is `*ErrorData` instead of `json.RawMessage`. Data that does
    not match a defined shape, including primitives, stays raw in
    `ErrorData.Other`. A decoded `UnsupportedProtocolVersion` or
    `SessionNotFound` variant re-encodes its original bytes, unknown keys
    included, until the variant is changed, so `data` round-trips like Rust's
    `serde_json::Value`.
  - `AuthCapabilities` is renamed `ClientAuthCapabilities`;
    `RequestPermissionOutcomeCancelled` is replaced by the named
    `CancelledPermissionOutcome`; `ProtocolLevelNotification` is removed.
  - `ElicitationSchema.Type` and `.Properties` are required, so `Type` is a
    value instead of a pointer and `Properties` is always encoded (use an
    empty map, not nil, for a schema without properties).
  - `ResourceLink.Size` is `Nullable[uint64]` and `TerminalExitStatus.ExitCode`
    is `Nullable[int64]`.
  - Patch fields of the upserts and `UsageUpdate` that the schema writes as
    `anyOf [$ref, null]` are `Nullable[T]` instead of pointers, so an explicit
    `null` (clear) no longer decodes the same as an omitted field (#67):
    `ToolCallUpdate.Kind`, `.Status`, and `.Capabilities`;
    `TerminalUpdate.Cwd`, `.Output`, `.ExitStatus`, `.Size`, and
    `.Capabilities`; `UsageUpdate.Cost`; and, in `schema/v2/unstable`,
    `SubagentUpdate.Capabilities`, `.State`, and `.Cwd`, and
    `SessionMessage.SenderSessionID`, `.RecipientSessionID`,
    `.SenderMessageID`, and `.RecipientMessageID`.
- Breaking (v1 unstable): `IdleStateUpdate.StopReason` is the new open
  `IdleStopReason`, Rust's name for it (its union adds `error` with the new
  `IdleStateUpdate.Error`, and custom reasons keep `StopReasonFields`; the
  stable v1 `StopReason` is unchanged).
- Breaking (both unstable packages): `SessionMessageChunk` drops
  `SenderSessionID` and `RecipientSessionID`.
- Outside the state updates, `cmd/acpgen` still ignores the
  `x-deserialize-*` hints (default-on-error, default-on-null, skip invalid
  items), so a malformed value in such a field fails the whole message where
  Rust reads it as not reported. This is a known gap, tracked in #82. The
  1.15 default-on-null hints on v1 request fields are unaffected: `null`
  already decodes as empty.
- **Breaking:** JSON-RPC batches are rejected on both v1 and v2, matching the
  ACP v2 transports "No batches" rule and the reference Rust SDK 5.0 (#68). A
  JSON array, including `[]`, now gets exactly one error,
  `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"JSON-RPC batches are not supported; send each message individually"}}`,
  and none of its entries runs: no request or notification handler is called,
  `$/cancel_request` entries cancel nothing, and response entries complete no
  pending call. The connection keeps serving later single messages.
- **Breaking:** `agentrouter` and `proxyrouter` answer a batched first frame,
  including one that holds `initialize` or `_proxy/initialize`, with that same
  error and keep reading, so a following single initialize is routed
  normally. Previously they validated and dispatched the batch.
- **Breaking:** the draft HTTP transport answers a JSON array `POST` body with
  `400`, `Content-Type: application/json`, and the JSON-RPC error above
  instead of a plain-text `400`, before checking connection headers and
  without creating a connection. Its client treats an array from the server as
  malformed: on the event stream the connection answers it with the same
  error, and as an `initialize` response body it fails the initialize.
- A line that is not valid JSON, including an empty line and one that only
  starts like an array, now gets exactly one parse error, as the reference SDK
  sends it:
  `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error","data":{"line":"<the line>"}}}`,
  with the line as read, without its `\n` or `\r\n` terminator. The connection,
  `agentrouter`, and `proxyrouter` keep reading afterwards. Previously the
  connection closed, the agent router exited, and the proxy router answered
  `-32600` and exited. Invalid UTF-8 still closes the connection, because the
  reference SDK cannot read it as a line either. Valid JSON that is not a
  JSON-RPC message still closes it; that is tracked in #73.
- Validity follows `serde_json`, which the reference SDK parses with, rather
  than `encoding/json`. A lone UTF-16 surrogate escape such as `"\ud800"`, a
  number whose float64 evaluation overflows (including values just below
  `math.MaxFloat64` such as `1.7976931348623158e308`, which `serde_json`
  rejects), and arrays or objects nested 128 deep are parse errors. So
  `[1e400]` is a parse error, not a batch.
- The `-32600` batch rejection and the `-32700` parse error are queued before
  the next frame is handled, so a later response, or a send from the next
  frame's notification handler, can no longer overtake them.
- The draft HTTP server handles a `POST` body that is not valid JSON as the
  Rust crate does. Without a connection it is still a plain-text `400`. On an
  unknown connection it is now `404`. On a live connection it is forwarded and
  answered `202`, and the agent sends the parse error on the stream. A body
  that is not valid UTF-8 is a plain-text `400`. A malformed body that spans
  lines stays a `400`: the Rust crate re-encodes it as one JSON string, which
  acp-go agents cannot answer in place until #73. The HTTP client fails an
  `initialize` whose response body is not valid JSON.

### Fixed

- The v1 and draft v2 agent runtimes could call `Agent.Initialize` twice when
  two `initialize` requests were in flight at once, because the
  already-initialized check released its lock before the handler ran. The
  runtimes now mark initialization as in progress under the lock and answer a
  concurrent `initialize` with `-32600` and the data "ACP initialization is
  already in progress on this connection" without reaching the handler. A
  failed initialization clears the in-progress state, so a later `initialize`
  can retry. Failures include a handler error, an unsupported selected
  protocol version, and a rejected v2 advertisement. The draft v2 runtime
  matches the reference Rust SDK. In the Rust SDK, the v1 agent mode does no
  lifecycle tracking (`InitializationRole::Unchecked`) and passes a repeated
  `initialize` to the handler. The acp-go v1 runtime still refuses every
  repeat, so it refuses the concurrent one too, until #62 aligns v1 repeat
  handling (#74).
- The `RPCError` documentation claimed it preserved extension data while it
  dropped unknown members. It now documents the omitted, `null`, and value
  forms of `data`, which were already preserved and are now tested.

### Removed

- **Breaking:** `Connection.CallBatch` and `BatchCall`, and all inbound batch
  processing (grouped response arrays, response-batch routing, and the agent
  router's batched-initialize validation). Send each message individually
  (#68).
- Next Edit Suggestions, removed from the upstream unstable schema: every
  `nes/*` and `document/did*` method, the `Nes*`, `Did*DocumentNotification`,
  `Position`, `Range`, `PositionEncodingKind`, `TextDocumentContentChangeEvent`,
  `TextDocumentSyncKind`, and `WorkspaceFolder` types, the `nes` and
  `positionEncoding(s)` capabilities, the `unstable.StartNes`, `SuggestNes`,
  `AcceptNes`, `RejectNes`, `CloseNes`, `NesHandler`, and
  `HandleNesNotifications` facades with their `Handle` cases, and the same
  facades in `v2/unstable`, which now has no facades.

## 0.12.1 - 2026-10-05

### Fixed

- Generated `ElicitationFormMode` now carries `RequestedSchema`, and
  `ElicitationUrlMode` carries `ElicitationID` and `URL`, in the v1, draft v2,
  and both unstable schema packages. These required fields were dropped, so
  `CreateElicitation` sent invalid `elicitation/create` requests and decoding
  lost them (#53). `cmd/acpgen` now lifts properties declared beside an
  untagged union into common fields, and fails generation when a union's
  properties would not be modeled.
- The schema golden tests decoded into a generic map instead of the generated
  type, so they never exercised the generated codecs. They now decode into the
  concrete type, and the v1 fixtures that had drifted from the pinned schema
  are corrected.

## 0.12.0 - 2026-10-02

### Added

- Draft v2 `NewSessionResponse` and `ResumeSessionResponse` carry
  `availableCommands`, the initial commands the agent can execute; later
  `available_commands_update` notifications replace the list (#2259).
- The pinned unstable schemas add the subagent capability, session, and
  `subagent_update` types (#1992), plus the session message and state-update
  types (`idle`, `running`, `requires_action`, `unknown`) that ride in
  `session/update`.
- `mcp` and `v2/mcp` gain the request-scoped MCP-over-ACP message mover
  (#39): `MessageClient` drives an agent-to-provider `mcp/message` request and
  routes its request-scoped notifications, while `MessageRouter` serves
  registered server IDs and answers binding failures with the protocol's outer
  error codes (`-32602`, `-32800`, `-33001`, `-33002`). Inner MCP errors stay
  in the response carrier and never surface as ACP errors.

### Changed

- Schema pins move from ACP v1 `schema-v1.23.0` / draft v2
  `schema-v2.0.0-alpha.5` to `schema-v1.24.1` / `schema-v2.0.0-alpha.7`,
  matching Rust schema crate `agent-client-protocol-schema` 1.10.2. The
  unstable MCP-over-ACP surface follows upstream's request-scoped binding
  (#2223): `mcp/connect` and `mcp/disconnect` are removed, `mcp/message` now
  carries `serverId`, `requestId`, `method`, and optional `params`, and its
  response is a shape-disambiguated union carrying exactly one inner MCP
  `result` or `McpError`. The Rust SDK's latest release (2.2.0) still pins
  schema 1.9.1; the pin moved on the schema crate's own release, which the
  pin workflow allows.
- `cmd/acpgen` models untagged unions with inline object variants
  distinguished by required carrier keys, the shape of the new
  `MessageMcpResponse`, with a regression test.

### Fixed

- Client-side path validation now accepts paths that are absolute on either
  platform: POSIX roots (`/`), Windows drive-absolute paths (`C:\dir` or
  `C:/dir`), and UNC paths. A Windows client can address a remote POSIX agent
  with `cwd` `/`, and a POSIX client can address a Windows agent; drive-relative
  (`C:dir`) and root-relative (`\dir`) paths stay rejected. Host-local paths in
  `clienthost` and the draft-v2 runner keep host-native checks.
- `v2.Connection.NewSession(ctx, directory)` uses the initialization from the
  most recent successful `Initialize` call instead of passing an empty
  initialization, which made the convenience method fail before sending
  `session/new` (#46).

## 0.11.0 - 2026-09-29

### Added

- `acp.Connection.CancelRequest(ctx, requestID)` sends a typed
  `$/cancel_request` notification for one outstanding request, covering the v1
  transport and the draft-v2 client that embeds it. Request IDs may be given as
  a string, an integer, or a `json.RawMessage` for exact wire bytes; null,
  empty, and non-scalar IDs are rejected.
- `github.com/BrokkAi/acp-go/tracecontext` gains `IntoMeta` and `FromMeta` for
  the root-level `_meta` keys `traceparent`, `tracestate`, and `baggage` that
  the ACP extensibility conventions reserve for W3C Trace Context, plus
  `ValidTraceparent` for the fixed `traceparent` grammar. The helpers work on
  the generated v1 `Meta` and draft-v2 `Nullable[Meta]` fields with no new
  dependencies.
- `acp.TerminalAuthCapabilities` advertises terminal-type authentication
  methods via `clientCapabilities.auth.terminal`. `runner.TerminalAuthMethod`,
  `runner.TerminalAuthCommand`, and `runner.RunTerminalAuth` support the
  out-of-band interactive login a terminal method requires: find the advertised
  method, build the configured agent command with the method's args and env,
  run it in the user's terminal, then reconnect and initialize.
- `docs/rust-ecosystem-parity.md` records whether each reference Rust workspace
  crate without a Go equivalent should be ported. It breaks out follow-up issues
  for the test harness (#21, delivered as `acptest`), cookbook (#22, delivered
  as `cookbook`), trace viewer (#23, delivered as `traceviewer`), and conductor
  (#24), and records two won't ports with their rationale: the
  `agent-client-protocol-rmcp` bridge, which requires a third-party MCP SDK,
  and the proxy-chain conductor, which is an application with no Go consumer on
  a wire shape the pinned schema does not cover.
- The optional credential-backed integration job now also runs the draft-v2
  runner (`TestRealAgentV2Prompt`), which probes the agent and skips unless it
  advertises ACP v2. `TestRealAgentPrompt` keeps covering the v1 runner against
  the same command.
- `github.com/BrokkAi/acp-go/acptest` is the standard-library-only test harness:
  an in-memory duplex `Pair` transport any v1 or draft-v2 connection can use,
  deterministic typed prompt commands through `TestAgent`, raw framing helpers
  (`Serve`, `WriteRequest`, `WriteInitialize`, `ReadResponse`), and v1/draft-v2
  fake agents. The agent, proxy, and client router tests consume it instead of
  hand-rolled framing.
- `docs/schema-pin-workflow.md` documents the reviewed process for moving a Rust
  schema pin: regenerate both artifacts, review the generated API diff, record
  it in this changelog, and validate before the pins move together.
- Typed client facades now validate media types and URIs before writing, and the
  draft-v2 session handle requires a non-blank configuration value ID. The checks
  live in `internal/acpvalidate` as plain string work (no reflection, no
  dependencies); the generated types stay permissive so unknown union tags,
  open-world enum values, and raw `_meta` payloads keep decoding.
  `docs/validation-boundaries.md` records the boundary decision.
- New opt-in `github.com/BrokkAi/acp-go/transport/http` package (`acphttp`)
  implements the draft Streamable HTTP binding: `POST` + `application/json`
  frames, `Acp-Connection-Id`/`Acp-Session-Id` headers, and a
  `text/event-stream` `GET` for server-to-client messages. `acphttp.Dial`
  returns a normal `acp.Connection` and `acphttp.Server` serves any
  `agent.Runtime` entry, so stdio stays the default and no transport falls back
  to another. Draft limits (no WebSocket, CORS, per-session streams, or batch
  bodies) are documented in `docs/http-transport.md`.
- New opt-in `github.com/BrokkAi/acp-go/unstable` package starts the typed
  facades for optional methods: capability-gated `ListProviders`, `SetProvider`,
  `DisableProvider`, and `ForkSession` client helpers plus agent-side
  `ProviderHandler`/`ForkHandler` dispatch through `Handle`
  (`docs/unstable-facades.md`).
- NES typed facades: `unstable` (v1) and `v2/unstable` (draft v2) gain
  capability-gated `StartNes`, `SuggestNes`, `AcceptNes`, `RejectNes`, and
  `CloseNes`, plus agent-side `NesHandler` dispatch through `Handle` (requests)
  and `HandleNesNotifications` (notifications).
- New opt-in `github.com/BrokkAi/acp-go/mcp` package is the v1 counterpart to
  `v2/mcp`: `NewSession`, `ResumeSession`, and `ForkSession` accept the unstable
  MCP server transports, including the native `acp` variant the stable v1 schema
  does not model, with capability gating, absolute-path checks, and server
  validation. `unstable.ForkSession` points callers at it for MCP servers.
- New `github.com/BrokkAi/acp-go/traceviewer` package and `cmd/acp-trace-viewer`
  command render a JSONL transcript as an ordered sequence diagram over
  `net/http`, with an embedded page and no external assets. `FileSource`
  re-reads the transcript on every poll for live updates and `Memory` serves
  events pushed by a host; `traceviewer.Open` launches the platform browser on
  request.
- `unstable.Projection` folds the optional session updates into ordered
  per-session state: plan/plan_update/plan_removed, compaction updates with
  chunked or replaced summaries in first-seen order, live notices (`Notices`
  peeks, `DrainNotices` consumes), and the latest end-turn token usage.
  `Projection.Notifications` adapts `session/update` notifications. These
  features travel inside session updates, so a projection helper is the right
  shape rather than a request facade, as recorded in
  `docs/unstable-facades.md`.
- New documentation-only `github.com/BrokkAi/acp-go/cookbook` package ports the
  Rust cookbook recipes that map onto acp-go as runnable examples that
  `go test ./...` executes: one-shot v1 and draft-v2 prompts (in-process and
  through both runners), building a v1 agent with tool calls and permission
  requests, ordered application dispatch, draft-v2 session resume/replay/close
  coordination, a `proxyrouter` proxy component, and attaching MCP servers
  through the stable v1 facade, `mcp`, and `v2/mcp`. The package documentation
  records the Go ordering contract each recipe relies on and the Rust recipes
  that do not port.
- `docs/unstable-facades.md` records the MCP-over-ACP method decision (#39):
  a standard-library message mover, with no third-party MCP SDK. The work is
  blocked until a schema release includes upstream's request-scoped
  `mcp/message` binding (agentclientprotocol/agent-client-protocol#2223), which
  removes the pinned `mcp/connect` and `mcp/disconnect`.

### Fixed

- The module builds and tests on Windows. `internal/osrun` used `Setpgid` and
  `syscall.Kill` without a build constraint, so `runner`, `v2/runner`, and
  `clienthost` (and `go install` of anything importing them) failed to compile
  there. Windows now starts commands in a new process group and kills the
  command tree with `taskkill /T`, and never signals a PID after `Wait` has
  released it. Descendants orphaned by an exited command are reaped only on
  Unix. CI adds a `windows-latest` job.

## 0.10.0 - 2026-09-23

### Added

- `runner.SetupError` and `v2/runner.SetupError` gain a `Phase` field of the
  new `Phase` string type naming the setup step that failed, with exported
  constants (`PhaseLaunch`,
  `PhaseInitialize`, `PhaseAuthenticate`, `PhaseSessionNew`, and
  `PhasePrompt`; the v1 runner adds `PhaseSelectMode`, `PhaseSelectModel`, and
  `PhaseSelectEffort`). The values match the phase recorded in the transcript's
  `session_end` event, plus `launch` for failures before the agent connection
  exists. `Error()` text is unchanged.
- `acp.UnknownSelectionError` is returned by `SetModel`, `SetEffort`, and
  config-option `SetMode` when the requested value is not offered. It carries
  the requested category, config ID, option name, value, and advertised values.
- `acp.UnsupportedSelectionError` is returned when the agent advertises no
  model, reasoning effort, or mode selector.

  Both selection errors keep the previous `Error()` text and pass through
  `SetupError` and the runner's diagnostics wrapping, so callers can use
  `errors.As` to tell an unoffered value from a missing selector and from a
  JSON-RPC error returned by the agent (`*acp.RPCError`). Other selection
  failures, such as an unconfirmed selection, malformed options, transport
  errors, and the legacy `unknown session mode` error, remain untyped.

### Changed

- The v1 runner now labels a failure to record the prompt in the transcript as
  the `session/prompt` phase instead of the preceding selection phase.

## 0.9.0 - 2026-09-22

### Changed

- Track the Rust SDK's new schema pin. The behavioral reference is now Rust
  `agent-client-protocol` 2.2.0 with `agent-client-protocol-schema = "=1.9.1"`,
  so the pinned artifacts move from `schema-v1.21.0` to `schema-v1.23.0` and
  from `schema-v2.0.0-alpha.3` to `schema-v2.0.0-alpha.5`. All four generated
  packages were regenerated from the exact Rust 1.9.1 artifacts.
- Draft-v2 `session/prompt` now reports the identity of the user message it
  inserted. `schema-v2.0.0-alpha.5` makes `PromptResponse.messageId` required
  and non-null, so `v2.Connection.Prompt`, `v2.Connection.PromptContent`,
  `v2.SessionHandle.Prompt`, and `v2.SessionHandle.PromptContent` return a
  `MessageID` alongside the error, and `v2/runner.Result` gains
  `UserMessageID`. This is a breaking API change in the draft-v2 packages; the
  stable v1 surface is unchanged.

### Added

- Programmatic tool-call names are now part of the stable v1 and v2 schema
  packages, following schema 1.8.0. `clienthost` logs the name with each tool
  call and keeps it across updates that omit it.
- Session notices reach `schema/unstable` and `schema/v2/unstable`, including
  the `notice` session update, `NoticeSeverity`, and the v1 client
  `notices` session capability. Earlier releases documented notices as
  available; they were not present in the 1.7.0 artifacts this project pinned.

### Fixed

- The draft-v2 agent runtime refuses to answer `session/prompt` when an
  implementation returns an empty `MessageID`, and the v2 client rejects an
  acceptance that omits it. Both previously passed an invalid empty identifier
  through as a success.
- `clienthost` no longer replaces a tool call's title with its tool-call ID
  when a `tool_call_update` omits the title. Omitted title and name fields are
  patch fields and leave the retained value unchanged.

## 0.8.1 - 2026-09-22

### Fixed

- `clienthost.Host.Answer` now starts a new line when the agent starts a new
  message, instead of running the end of one message into the start of the
  next. The protocol says a change in `messageId` begins a new message, and an
  agent may end one without a trailing newline, so a client reading the final
  answer off the last line could see it welded to the commentary before it.
  Chunks of a single message are still joined exactly as they arrive, and an
  agent that omits `messageId` keeps the previous single-buffer behavior.

## 0.8.0 - 2026-09-15

### Added

- An explicit v1/v2 client protocol connector with Rust-compatible matching,
  reconnection, and no-retry-on-v2-rejection behavior.
- An exact-match proxy protocol router that preserves the complete initial
  `_proxy/initialize` frame and rejects unsupported future versions.
- Generated v1 and v2 unstable-schema packages pinned to the exact Rust schema
  1.7.0 unstable artifacts. They collectively expose optional providers, plan
  operations, session fork/compaction, NES, MCP-over-ACP, tool-call names, and
  end-turn token usage without changing the stable packages.
- Method registry support for bidirectional methods that have both request and
  notification payloads, matching Rust `mcp/message`.

### Fixed

- Reject non-regular text-file reads without blocking on FIFOs, preventing
  filesystem callbacks from trapping runner shutdown.
- Release completed request slots before responses become visible to peers,
  allowing replacement requests at the 32-handler concurrency limit.
- Resolve terminal workspace roots consistently so explicit working directories
  work through symlinks while paths outside the workspace remain rejected.

## 0.7.0 - 2026-09-10

### Added

- A Rust-shaped draft-v2 session command handle covering prompt acceptance,
  authoritative config-option replacement, close, session cancellation, and
  optional cancellation completion.
- Public v2 agent-message projection, active-work tracking, and per-session
  update tracking.
- Cancellable v2 permission hosting that resolves pending requests with the
  protocol's cancelled outcome.
- A from-start v2 resume helper that applies replay updates before returning
  the resume response.

## 0.6.0 - 2026-09-10

### Added

- JSON-RPC batch ingress and grouped request responses, matching the reference
  transport’s handling of requests, notifications, responses, malformed members,
  duplicate IDs, trailing notification handlers, and independent standalone
  calls.
- `Connection.CallBatch` for outbound batch requests whose replies may arrive
  individually or in a response batch.
- Agent protocol-router validation of initial v1/v2 initialize batches before
  sibling dispatch.

## 0.5.0 - 2026-09-10

### Changed

- Generated JSON Schema integer fields now use their exact wire widths. Most
  visibly, v1 and v2 `ProtocolVersion` are `uint16` and error codes are
  `int32`, matching the Rust schema crate.
- Successful v1 and v2 client initialization is now allowed only once per
  connection.
- v1 and v2 agent runtimes reject mismatched initialize versions before invoking
  the implementation.

### Added

- An explicit v1/v2 agent protocol router that selects the highest configured
  compatible implementation and canonicalizes only the initial request.
- An explicit draft MCP-over-ACP opt-in package at
  `github.com/BrokkAi/acp-go/v2/mcp`; the core v2 session API no longer accepts
  MCP servers directly.

## 0.4.0 - 2026-09-10

### Added

- Typed v2 client-host adapters for permission and elicitation requests.
- A draft-v2 one-shot process runner that mirrors the reference SDK: updates
  are installed before session setup, queued output is ignored until the
  session reports running, agent messages honor chunk and `Nullable` patch
  semantics, foreground work completes at the next idle update, and permission
  requests default to explicit cancellation.
- A `v2-one-shot-client` example modeled on the reference SDK example.

## 0.3.0 - 2026-09-10

### Added

- Generated draft-v2 schema bindings pinned to the official
  `schema-v2.0.0-alpha.3` release.
- A separate v2 client package for v2 initialization, authentication, session
  lifecycle, prompt acceptance, cancellation, capability checks, and typed
  session updates.
- A draft v2 agent runtime for baseline sessions, prompt acceptance,
  cancellation, optional lifecycle/config methods, and typed editor callbacks.
- Generated open unions now retain the unknown discriminator in `Kind` as well
  as the raw payload in `Other`.
- Generated v2 patch fields use `Nullable[T]` to distinguish omitted values,
  explicit JSON null values, and concrete payloads.

## 0.2.1 - 2026-09-10

### Fixed

- Made the agent runtime's session-cancel hook test wait for the concurrently
  invoked optional hook, removing a narrow CI race.

## 0.2.0 - 2026-09-10

### Added

- Generated schema bindings for every message in ACP `schema-v1.21.0`, with
  golden wire fixtures and parity tests.
- The complete v1 client surface, including sessions, prompt content, config
  selection, authentication, elicitation, MCP servers, and typed errors.
- An agent-side stdio runtime, typed editor callbacks, a reusable reference
  client host, and runnable client/agent examples.
- Seed-corpus and mutation fuzzing for the JSON-RPC wire layer.
- Opt-in CI integration tests against the official Claude Agent and Codex ACP
  adapters. Ordinary CI and local tests never require credentials.

### Changed

- The public client API now exposes generated schema types directly. This is a
  breaking transition from the hand-written v0.1.0 wire types.
- Terminal, transcript, and workspace-host implementations moved from
  `runner` to the reusable `clienthost` package; `runner` now composes that
  host instead of duplicating it.
