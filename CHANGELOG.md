# Changelog

All notable changes to this project are documented in this file. Release
entries use [Semantic Versioning](https://semver.org/); during the `0.x`
series, minor releases may contain breaking API changes.

## Unreleased

### Added

- `acp.IsCancelRequestNotification`, the reference SDK's
  `is_cancel_request_notification`: whether a notification is a
  `$/cancel_request`, plain or wrapped in any number of `proxy/successor`
  envelopes, so forwarding code can drop it instead of relaying a
  connection-scoped id (#69).
- The draft HTTP transport (`transport/http`) speaks the WebSocket binding of
  Rust `agent-client-protocol-http` 5.0.0 (#64), with RFC 6455 framing
  implemented on the standard library, so the dependency policy is unchanged:
  - `acphttp.Server` upgrades a `GET` of the endpoint that passes axum's
    `WebSocketUpgrade` checks and serves any other `GET` as before. An upgrade
    whose `Origin` the CORS policy does not allow gets
    `403 WebSocket origin not allowed`; one without `Origin` passes. A
    request body is read and discarded before the upgrade, as hyper does.
    The `101`
    carries the Rust server's headers, in its order, with the connection id
    in `acp-connection-id`, and no subprotocol or extension. Each socket runs
    one `Entry.Serve`, so any `agent.Runtime` or `v2/agent.Runtime` is served
    unchanged.
  - Each text frame is one JSON-RPC message, forwarded as a `POST` body is; a
    JSON array frame gets one `-32600`/`id: null` frame and none of its
    entries is forwarded. Binary frames and pongs are ignored and pings
    answered. Frames may hold 16 MiB and messages 64 MiB; text must be UTF-8;
    masking, reserved bits and opcodes, and fragmentation are checked as
    tungstenite checks them.
  - As in the Rust crate, neither side answers a close frame: a close frame
    or protocol violation from the client ends the connection and stops the
    agent, and an agent that finishes, `DELETE`, or `Server.Close` delivers
    what was queued and closes the socket without a close frame. `POST`,
    `GET` (`409`, or `404` with a session), and `DELETE` with the
    connection id behave as in the Rust crate.
  - `acphttp.Dial` accepts `ws://` and `wss://`: the handshake goes through a
    copy of `Client.HTTP` with no redirects, HTTP/1.1 only, and reqwest's
    request headers (`Accept: */*`, no `User-Agent`, no compression);
    `Client.HTTP.Timeout` bounds the handshake only. It is checked
    before any ACP data, failing with the Rust client's
    `WebSocket connect failed: ...` texts. The connection writes one masked
    text frame per message and fails with `WebSocket closed by peer: ...`,
    `ws recv: ...`, or `ws send: ...` as the Rust client does; `Close` sends
    a close frame without a payload. Other schemes are refused with
    `unsupported URL scheme: <scheme>; expected http, https, ws, or wss`.
  - Cancellation can be sent as soon as the socket is open, including while
    `initialize` is in flight.

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
    `ForkAttachedSession` (v1) and `NewAttachedSession`,
    `ResumeAttachedSession`, and `ForkAttachedSession` (v2): unique
    `mcp-server:<uuid>` IDs, registration before the request, release on
    failure or `Attachment.Close`, and, for v2, `-32602` before writing when
    the request already declares the name.
  - `Attachment.HandleSessionSetup` and `Attachment.AppendDeclarations` for
    connection-scoped proxy attachment, appending to the raw params so unknown
    fields keep their values.
  - `ErrorCodeResourceLimitExceeded` (`-33000`), `ErrorCodeServerUnavailable`
    (`-33001`), and `ErrorCodeBackendFailure` (`-33002`), and
    `MessageRouter.SetMaxActive`, which answers `-33000` past its limit.
  - `MessageRouter.UnregisterAndWait` and `MessageRequest.RawParams`.
  - v1 `mcp.LoadSession`.
- Draft v2 background work, `tool_call/cancel`, and `terminal/cancel`, per
  the v2 prompt lifecycle and Rust `agent-client-protocol` 5.0.0 (#70):
  - `acpv2.SessionTracker.CancelToolCall` and `.CancelTerminal`, and
    `SessionHandle.CancelToolCall` and `.CancelTerminal`, send the requests
    only while the entity currently advertises `capabilities.cancel` in the
    session's projection, and otherwise fail with an
    `*acpv2.NotCancellableError` before writing. Neither method has an
    initialize-time capability. The empty response only accepts the
    cancellation; the tool call's final status or the terminal's
    `exitStatus` confirms it.
  - `UpdateProjection.CanCancelToolCall` and `.CanCancelTerminal` read the
    stored `capabilities` with patch semantics (omitted keeps, `null` clears,
    an object replaces).
  - `UpdateProjection.Background` keeps the `background` list of the latest
    `state_update` (running, requires_action, idle, or a custom state) as a
    snapshot; omitted, `null`, or malformed means not reported, not empty.
    `ToolCallInBackground` (listed, or referencing a listed terminal) and
    `TerminalInBackground` read it.
  - `ActiveWork.Background` and `WorkResult.Background`/`.BackgroundReported`
    report the list the work observed; an idle with background work still
    completes the foreground work.
  - `v2/agent.ToolCallCanceller` and `TerminalCanceller` serve the methods,
    and `ToolCallNotCancellableError` and `TerminalNotCancellableError` build
    the `-32602` for an unknown, final, or non-cancellable entity.
  - `v2/agent.ForegroundPermissions` sends an Agent's foreground permission
    requests and, from its `CancelSession`, stops waiting for them: each is
    cancelled with `$/cancel_request` and reported as the cancelled outcome,
    which is not an approval. Background requests sent directly stay open;
    on `session/close` the Agent must cancel every pending request itself,
    including those.
- `acp.Connection.InputClosed`, closed when the connection stops reading
  frames. On a clean EOF it closes before `Done`, which still waits for
  in-flight handlers, so long-running handlers can stop instead of waiting on
  a peer that is gone.

- Protocol version negotiation per the ACP v2 spec and the reference Rust SDK
  5.0.0 (#62). An endpoint selects the highest version it supports at or
  below the requested one, and answers when it supports none with `-32022`
  (`Unsupported protocol version`) and
  `data: {"requested": N, "supported": [...]}`. A missing, null, non-integer,
  or out-of-range `protocolVersion` stays `-32602`, with data
  `"initialize.protocolVersion must be a valid ACP protocol version"`. See
  Changed for each endpoint.
- `acp.ErrorCodeUnsupportedProtocolVersion` (`-32022`) and
  `acp.IsUnsupportedProtocolVersion`.
- Typed draft-v2 error data in `github.com/BrokkAi/acp-go/v2` (#66):
  `UnsupportedProtocolVersionError` builds the `-32022` error on the generated
  `UnsupportedProtocolVersionErrorData`, like Rust's
  `v2::schema::Error::unsupported_protocol_version`, and the runtimes and
  routers answer with exactly it. `SessionNotFoundError` builds the `-32002`
  error with `SessionNotFoundErrorData`. `UnsupportedProtocolVersionData` and
  `SessionNotFoundData` decode those variants from any error that is or wraps
  an `*acp.RPCError` with the matching code. `SchemaError` is the explicit
  conversion to the typed v2 `schema.Error`: it drops `Extra`, reads omitted
  and `null` data as nil, and keeps unknown data shapes raw in
  `ErrorData.Other`.
- `v2.Connection.SendInitialize` returns an `InitializeOutcome`, like Rust's
  `v2::InitializeOutcome`: `Initialized` when the agent selects v2, or
  `LowerVersion` when it selects v1. `LowerVersion` is the new root
  `acp.InitializationVersionMismatch`, with the requested version and the
  agent's validated v1 response in `acp.SelectedInitializeResponse`, like
  Rust's crate-root types. The connection stays open with its previous
  initialization, if any (none for a first initialize; the previous one for a
  repeat), so the caller can fall back to a v1 client or close it.
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
  - `acp.WithRequestObserver`, a `ConnectOption` for `acp.Connect` and
    `acpv2.Connect` (#73), sets an optional `acp.RequestObserver` that sees
    each incoming request in wire order, on the read loop, before its handler
    starts. A panic in it is recovered and reported like a handler panic.
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
  and requires a non-blank forked session ID in the response. All checks fail
  before writing to the wire.
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
- `acp.Connect` takes optional `acp.ConnectOption`s (#73).
  `acp.WithProtocolVersion` records the protocol version a version-neutral
  connection serves, which decides how a null request id is handled.
  `acp.WithErrorHandler` receives the failures the connection absorbs instead
  of closing: notification handler errors and `*acp.HandlerPanic` values
  (method, panic value, stack) recovered from handlers. `v2.Connect`,
  `acptest.Endpoint.Connect`, and the draft HTTP transport's `acphttp.Dial`
  and `Client.Dial` pass options through, and `v2.Connect` always sets
  protocol version 2.
- Draft v2 error and custom stop reasons (#59):
  - `acpv2.WorkResult` gains `Error`, the JSON-RPC error the agent reported
    with the `error` stop reason, and `StopReasonFields`, the other fields of
    a custom or future stop reason. `WorkResult.Err` returns a typed
    `*acpv2.WorkError` (its `Failure` is that error, or nil without details)
    only for the `error` stop reason, so callers need not compare strings.
  - `v2/runner.Result` gains `Error`, the `*acpv2.WorkError` of a turn that
    ended with the `error` stop reason.
  - `acpv2.NewIdleStateUpdate` and `acpv2.NewErrorIdleStateUpdate` build idle
    `state_update` session updates with a stop reason or with the `error`
    stop reason and its details.
  - `acpv2.ErrWorkNotCancelled` is wrapped by
    `SessionHandle.CancelActiveWorkAndWait` when the work became idle without
    `cancelled`.
- Session notices and compaction on the stable surfaces (#57):
  - v1 clients advertise them with `acp.SessionNoticesClientCapabilities`
    (`session.notices: {}`) and `acp.SessionCompactionClientCapabilities`
    (`session.compaction: {}`); `acp.SessionClientCapabilities` combines
    these with other session capabilities such as
    `ConfigOptionsClientCapabilities`.
  - `acp.NewNoticeUpdate`, `NewCompactionUpdate`, and
    `NewCompactionSummaryChunkUpdate` build the v1 updates, and
    `acpv2.NewNoticeUpdate`, `NewCompactionUpdate`, and
    `NewCompactionSummaryChunkUpdate` the draft-v2 ones.
  - `acp.SessionProjection` folds v1 notices and compactions per session in
    receive order, live and in `session/load` replay: compactions keep
    first-seen order, a first-seen ID starts `in_progress`, `summary`,
    `error`, and `_meta` are patches (unset keeps the stored value, `null`
    clears it, a value replaces it, and `summary: []` also clears), summary
    chunks append to the current summary, and unknown statuses are kept.
    Notices are live events that `Notices` peeks and `DrainNotices`
    consumes. `Notifications` adapts `session/update` and passes each
    notification on to an optional next callback.
  - `acp.RequireSessionUpdateCapability` reports whether an agent may send an
    update to a v1 client, live or in replay: `notice` needs
    `session.notices`, `compaction_update` and `compaction_summary_chunk`
    need `session.compaction`, and a `config_option_update` carrying a
    `type: "boolean"` option needs `session.configOptions.boolean`. A refusal
    is an `*acp.RPCError` with code `-32601`, the agent runtime's
    host-method refusal.
  - `acpv2.UpdateProjection` keeps draft-v2 notices, which need no client
    capability, as live events outside the timeline (`Notices`,
    `DrainNotices`), and `Compactions` lists its compactions in first-seen
    order.
- `unstable` and `v2/unstable` gain the session archive, status, and title
  facades (#71): `ArchiveSession` and `UnarchiveSession` (gated on the
  `archive` session capability), `SessionStatus` (`status`), and
  `SetSessionTitle` (`title`), each requiring a non-blank session ID (and
  title) before writing, plus `ListSessions`, the unstable `session/list`
  whose `archived` filter needs the `archive` capability. The filter and
  `SessionStatus` stay open enums: unknown values are sent and decoded
  unchanged. `SessionStatus` rejects a response without `status` and
  `SetSessionTitle` one without `title` (omitted or `null`), as the reference
  SDK's typed responses do.
- `ArchiveHandler`, `StatusHandler`, and `TitleHandler` serve those methods on
  the agent side through `Handle`, and `Advertise` announces the `status` and
  `title` session capabilities (v2 entries only alongside the baseline session
  capabilities). Status and title responses are sent as returned, as the
  reference SDK sends them; archive-aware list entries without `archived`,
  which the archive RFD requires on every entry, answer `-32603` instead of
  being sent.
- `agent.Runtime` and `v2/agent.Runtime` gain `Overrides`, a
  `map[string]acp.Handler` that serves the named methods in place of the
  runtime's own dispatch to the `Agent`, with the raw params, after the
  initialization gate and the method's capability check and ahead of
  `Extensions`. `Serve` refuses an override for `initialize`,
  `session/prompt`, or a notification such as `session/cancel`, or a nil
  handler, before reading. In v2 an override for `auth/login`,
  `auth/logout`, or `session/delete` satisfies the implementation check those
  capabilities make at initialize.
- `agent.ClientFromContext(ctx)` and `v2/agent.ClientFromContext(ctx)` return
  the connection's `Client` inside the requests the runtime dispatches,
  including `Overrides` and `Extensions` handlers, which take no `Client`
  argument. It is the client the stable dispatch passes to the `Agent`, so an
  override of `session/load` can stream its replay as `session/update` before
  it answers, and `session/new` and `session/resume` overrides can call the
  client, as the reference SDK gives every request handler its connection.
- `unstable.Install(runtime, handler, customize...)` and
  `v2/unstable.Install` set `Extensions` (`Handle`, chained in front of any
  `Extensions` already set), `InitializeResult` (`Advertise`), and the
  `Overrides` the handler needs on an agent runtime in one call, and refuse a
  nil runtime, an `InitializeResult` already set, or a conflicting override
  without changing the runtime. For an `ArchiveHandler` they serve
  `session/list` from `ListSessionsWithArchive`, so that the `archived` filter
  and `SessionInfo.archived` survive, and announce the `archive` capability
  (v1 only alongside the agent's own `sessionCapabilities.list`), which
  `Advertise` alone never does: an agent cannot advertise `archive` without
  serving the filter. That decision is made from the `Agent`'s own initialize
  response, before the `customize` functions run.
- `acp.SessionNotFoundError(sessionID)` builds the reference SDK's v1
  `session_not_found` error: `-32002` `Resource not found` with data
  `{"sessionId": ...}`. It is v1-only.
- `v2.SessionNotFoundError(sessionID)` builds the draft-v2 `session_not_found`
  error with `SessionNotFoundErrorData`, `v2.SessionNotFoundData(err)` decodes
  that data from any error that is or wraps a `-32002` `*acp.RPCError`, and
  `v2.SchemaError` converts an `*acp.RPCError` to the typed draft-v2
  `schema.Error`.
- Client-provided system prompts (#58), following the client-provided system
  prompt RFD and the reference SDK's `unstable_session_system_prompt`:
  - `unstable` (v1) and `v2/unstable` (draft v2) gain the `SystemPrompt` and
    `SystemPromptMode` aliases, the `Append` and `Override` modes,
    `NewSystemPrompt`, and `NewTextSystemPrompt`.
  - Lifecycle facades that carry a system prompt: `NewSession`,
    `LoadSession` (v1 only), and `ResumeSession`, alongside the existing
    `ForkSession`. They take the complete unstable request (the reference
    SDK's `_from` helpers), gate on `loadSession` or
    `sessionCapabilities.resume` where needed, require an absolute `cwd` and
    gated absolute additional directories, reject inline MCP servers, send
    v1's required `mcpServers` as `[]`, and require a non-blank session ID in
    the session/new response.
  - Before writing, every lifecycle facade, including the `mcp` and `v2/mcp`
    session helpers, rejects a `systemPrompt` the agent did not advertise
    (`agentCapabilities.sessionCapabilities.systemPrompt` in v1,
    `capabilities.session.systemPrompt` in v2), `override` without the nested
    `mode.override`, a missing or unknown mode, empty content, and any
    non-text content block. `ValidateSessionSystemPrompt` exposes the same
    check.
  - Agent side: `SystemPromptHandler` (`NewSessionWithSystemPrompt`, and in v2
    `ResumeSessionWithSystemPrompt`), and in v1 `SystemPromptLoader`
    (`LoadSessionWithSystemPrompt`) and `SystemPromptResumer`
    (`ResumeSessionWithSystemPrompt`), receive the unstable lifecycle requests
    with their `systemPrompt`, which the stable request types the runtimes
    decode drop. `Install` serves `session/new`, `session/load`, and
    `session/resume` (v2: `session/new` and `session/resume`) from them
    through `Runtime.Overrides`, in place of the `Agent`'s own methods, and
    the handlers reach the client through `ClientFromContext`. Params that do
    not decode answer `-32602` in the deserialization shape `Handle` uses,
    including a `systemPrompt` the reference SDK refuses to deserialize but
    `encoding/json` would accept leniently (a missing or `null` `mode` or
    `content`, or a text block without `text`), with the reference SDK's
    error text for the first fault in document order within `systemPrompt`,
    as serde reports it. That check runs before the full decode, so a prompt
    fault wins over a bad sibling member earlier in the params.
  - Before a handler runs, the `session/new`, `session/load`,
    `session/resume`, and `session/fork` routes check a decoded
    `systemPrompt` against the capability the runtime actually sent in this
    connection's initialize response, and reject an unadvertised prompt,
    `override` without `mode.override`, a missing or unknown mode, empty
    content, and any non-text content block with the reference SDK's
    `-32602` (`Invalid params`, with the reason as string data). Handlers no
    longer re-derive the capability, so one can never accept a prompt that
    was not advertised. The `agent` and `v2/agent` runtimes record the
    encoded initialize response they sent for this check.
  - `Install` announces the `systemPrompt` capability (`{}`, append only)
    only when the handler receives the prompt on every lifecycle request the
    agent advertises (v1: `loadSession` needs `SystemPromptLoader`, `resume`
    `SystemPromptResumer`, `fork` `ForkHandler`; v2: `fork` needs
    `ForkHandler`). A customize function may add `mode.override`; a
    `systemPrompt` capability some advertised lifecycle request would drop
    fails `initialize` instead of being sent. `Advertise` alone never
    announces it.
  - `mcp.NewSessionOptions` and `v2/mcp.NewSessionOptions` gain
    `SystemPrompt`, so MCP servers and a system prompt can be combined.
- Unstable auth status on v1 and draft v2 (#72), following the auth-status
  RFD and the reference SDK's `unstable_auth_status` feature:
  - `unstable.AuthStatus` and `v2/unstable.AuthStatus` send `auth/status`,
    gated on `agentCapabilities.auth.status` (v1) or `capabilities.auth.status`
    (v2) before writing. A response without `state` (omitted or `null`) is an
    error; an unknown `state` is preserved.
  - `AuthStatusUpdates` adapts the agent's `auth/status_update` notification to
    a typed handler and composes with the existing notification handlers, such
    as `clienthost.Host.Notification`, through `acp.CombineNotifications` or
    `acpv2.CombineNotifications`.
  - `AuthStatusHandler` serves `auth/status` on the agent side through
    `Handle` (and `Install`), and `Advertise` announces `auth.status` for it.
  - `NotifyAuthStatusUpdate` sends `auth/status_update`, which needs no client
    capability. Inside a request the runtime dispatched, it fails before
    writing when that connection's initialize response did not announce
    `auth.status`.
- The draft HTTP server reaches parity with the Rust
  `agent-client-protocol-http` 5.0.0 server, apart from WebSocket (#65):
  - Per-session event streams. `GET` with `Acp-Session-Id` opens the stream of
    a session the agent has named: in a `session/new` or `session/fork`
    result, in a successful response to a request scoped to the session, or in
    a request or notification it sent (rust-sdk #483). The session is recorded
    before the response naming it is queued. Responses go to the stream of the
    session their request named when it has one, and to the connection stream
    otherwise. `$/cancel_request`, also inside `proxy/successor` envelopes, is
    connection-scoped. Up to 16 streams may be open for sessions not named
    yet, for `session/load` replay; beyond that an unknown session gets `404`.
    A second subscriber to a stream gets `409`. A successful `session/close`
    or `session/delete` removes the session's stream.
  - Agent frames are queued per stream without a bound until the client opens
    it, including frames sent before the `initialize` response, and a
    subscriber that leaves keeps what it did not read for the next one.
  - `DELETE` with `Acp-Connection-Id` stops the agent, ends the streams after
    what is queued, and answers `202`.
  - `ServerOptions` with `Path` (default `/acp`), `CORS` (default disabled),
    `DisableHealth` (`GET /health` answers `ok` by default), and
    `MaxBodyBytes` (default `DefaultMaxBodyBytes`, 16 MiB). `NewServer`
    validates them.
  - `CORSOptions`, built with `CORSDisabled`, `AllowOrigins` (returning an
    `*InvalidOriginError` for an origin that is not a valid header value), or
    `AllowAnyOrigin`, answers preflights and sets the CORS headers as the
    Rust crate's tower-http layer does, allowing the ACP transport headers but
    not `Authorization` or credentials. The same policy is the origin check the
    WebSocket binding (#64) will use.
  - Event streams send an empty SSE comment after 15 idle seconds.
- The HTTP client opens a session's event stream before it sends the first
  message naming the session and when a `session/new` or `session/fork`
  result names a new one, and `Close` sends `DELETE`, as the Rust client does.
- Unstable subagent sessions on v1 and draft v2 (#60), following the
  subagents RFD and the reference SDK's `unstable_subagents` feature:
  - `unstable.SubagentsClientCapabilities` returns the v1
    `clientCapabilities.subagents` (`{}`), and `unstable.Initialize`
    negotiates v1 exactly as `acp.Connection.InitializeWithInfo` does with the
    unstable client capabilities, which the stable `acp.Capabilities` cannot
    carry, decoding the response as the unstable schema. Draft v2 needs no
    capability.
  - `SubagentTree` (v1 and draft v2) folds `subagent_update`,
    `session_message`, and `session_message_chunk`, and in v2 the child's own
    `state_update`, into a parent/child tree: `Subagent`, `IsSubagent`,
    `Children`, `Descendants`, per-child association patches (omitted
    unchanged, `null` clears, values replace; v2 `_meta` patches by key),
    effective `Capabilities` and confirmed `State` kept apart from the
    reported `Association`, inherited `Directories`, per-session `Messages`
    and `Message` (upserts replace content, `null` and `[]` clear it, chunks
    append, participants come only from upserts), `LinkedMessage`, and v2
    `BackgroundSubagents`. It refuses an association that reparents a child,
    names its own parent, or creates a cycle, and reports a relative
    directory with an error while applying the rest of the update.
  - Restricted children: `CheckClientMethod` permits only `session/cancel`
    (while the child advertises `capabilities.cancel`) and the read-only
    `session/status` on a child; `Cancel` sends `session/cancel` for one
    child and fails before writing otherwise.
  - Invalidation: `Restore` runs a parent `session/load` or
    `session/resume` (v2: `session/resume`, with replay for
    `replayFrom: start`) with child controls disabled until it succeeds, keeps
    snapshots replayed before the response as history only, and marks the
    response boundary in wire order; `Closed` invalidates a closed parent's
    descendants and returns them for request cleanup; `Disconnected` drops
    every live state and control.
  - `unstable.Projection.Subagents` returns the tree the projection feeds.
  - Agent side: `NotifySubagentUpdate`, `NotifySubagentState` (v2 sends the
    child's `state_update` and mirrors it on the association),
    `NotifySessionMessage`, `NotifySessionMessageChunk`, the
    `*Notification` constructors, the `RunningState`, `RequiresActionState`,
    `UnknownState`, `IdleState`, and `ErrorState` snapshots, and the `Value`
    and `Null` patch-field helpers. They fail before writing for blank IDs
    and relative directories.

- `acp.Connection.Shutdown(ctx)`, the reference SDK's orderly close of a
  connection whose `connect_with` foreground returned `Ok` (#80). It stops
  sending anything new (`Notify`, `Call`, and `CancelRequest` fail with
  `io.ErrClosedPipe`, incoming requests' contexts are cancelled and responses
  not yet queued are not sent, and incoming frames are read and discarded), writes
  every frame already queued, then closes the output and input streams. `ctx`
  bounds the drain: when it ends, `Shutdown` aborts like `Close` and returns
  its error. A read or write failure during the drain is returned; a clean
  peer EOF is not. `Close` still aborts at once, dropping queued frames, as
  dropping the reference SDK's connection does. `v2.Connection` gains it
  through its embedded `acp.Connection`.

### Changed

- Frames have no size limit, as in Rust `agent-client-protocol` 5.0.0, whose
  line readers have none (#90). `acp.Connection` (and so `v2.Connection` and
  the agent runtimes), `agentrouter`, `clientrouter`, `proxyrouter`, the HTTP
  client's event-stream reader, and `traceviewer` read a line of any length;
  before, a line over 8 MiB failed the connection, also while `Shutdown`
  drained it, where the line is now read and discarded. An HTTP `POST` body
  between 8 and 16 MiB now reaches the agent instead of being accepted with
  `202` and then ending the connection, and an event or `initialize` response
  over 8 MiB reaches the client. On the HTTP `POST` path, the server's 16 MiB
  body limit, which Rust has, is the only limit left. The connection's and the
  routers' line buffers grow only for a line that needs it and are not kept
  after a large one; the HTTP client's event-stream reader drops a large
  event's data buffer, but its line buffer stays as large as the longest line
  it has read. As in Rust, only a
  `"\n"` ends a line, and a `"\r"` is removed only before it, so a last line
  without `"\n"` keeps a trailing `"\r"`.
- **Breaking (wire):** the proxy-chain methods use the names in the ACP
  proxy-chains RFD and the pinned unstable schema, like Rust
  `agent-client-protocol` 5.0.0 (#69): `_proxy/initialize` is now
  `proxy/initialize` and `_proxy/successor` is now `proxy/successor`. The old
  names are neither sent nor accepted, so a proxy built on this release works
  with a Rust 5.0 conductor and not with an earlier one; upgrade every
  component of a chain together.
  - `proxyrouter` routes only `proxy/initialize`. A v2-only router answers
    `_proxy/initialize` with ``-32600 "first ACP request must be
    `proxy/initialize`"`` and keeps reading; with a v1 proxy configured, the
    v1 proxy receives it like any other first message.
  - `proxyrouter` and `agentrouter` with no configured implementation no
    longer fail `Serve` immediately: like Rust's routers, they answer each
    `proxy/initialize` or `initialize` with `-32022` and `"supported": []`,
    keep reading, and return at the end of input.
  - A `$/cancel_request` wrapped in `proxy/successor` (at any depth) cancels
    the inbound request it names on any `acp.Connection`, as the reference
    SDK accepts that form.
  - The cookbook proxy recipe uses the generated
    `schema/unstable.ProxySuccessorRequest` and `ProxySuccessorNotification`
    envelopes instead of a hand-modeled one: it accepts an envelope whose
    `params` is omitted or `null`, reads `_meta` (not `meta`) and keeps it on
    its hop, writes the envelope's `params` as `null` when the wrapped message
    has none (as Rust does), cancels a forwarded request with a plain
    `$/cancel_request` naming the `proxy/successor` request, drops plain and
    wrapped cancellations instead of forwarding them, and passes results and
    errors through unchanged.
- **Breaking (wire):** `acp.Connection` serializes outgoing requests and
  notifications as the reference SDK does. The protocol guards (the v1 and
  v2 runtimes' and clients' initialization and version checks) see the
  message first; then params that encode as `null` (including nil) are
  omitted from the frame instead of written as `"params":null`, and params
  that are not an object, an array, or `null` cannot be sent: a `Call` fails
  with `-32602` (data `"JSON-RPC params must be an object or array"`), and
  is settled like an abandoned call when a guard had admitted it, while a
  notification send succeeds, writes nothing, and reports the `-32602` to
  the `WithErrorHandler` hook, as the reference SDK's outgoing actor drops
  the frame. The `proxy/successor` envelope's own `params` member is a
  schema field and is still written as `null`.
- **Breaking (wire):** JSON-RPC ids are compared and echoed as the reference
  SDK decodes them: an inbound request id is re-serialized, so a response to
  `"id":"\u0062"` carries `"id":"b"`, a `$/cancel_request` naming `"\u0061"`
  cancels the request `"a"`, and a response id is matched to its call the
  same way. Previously ids were compared and echoed byte for byte.
- **Breaking:** a plain `$/cancel_request` now reaches the `Notifications`
  handler after it cancels the request it names, as the reference SDK
  dispatches it, matching the wrapped form. The positional params form
  (`[requestId, _meta]`) cancels too. A `$/cancel_request`, plain or
  wrapped, whose params do not parse as a `CancelRequestNotification` cancels
  nothing and is dropped without reaching the handler. A handler that relays
  every notification to another connection should drop one, using
  `acp.IsCancelRequestNotification`, because its `requestId` is scoped to its
  own connection.
- **Breaking (draft v2):** a `v2/agent` runtime whose Agent does not
  implement `ToolCallCanceller` or `TerminalCanceller` answers
  `tool_call/cancel` or `terminal/cancel` with `-32602` (`the tool call is
  unknown or not cancellable`, or the terminal form) and data holding the
  request's `toolCallId` or `terminalId` when it is a string, without
  decoding the params, as the reference SDK's `Agent.v2()` does; it answered
  `-32601` before (#70). Overrides for these methods still take precedence,
  and they no longer reach `Runtime.Extensions`.
- **Breaking (draft v2):** `SessionHandle.CancelActiveWork` (and
  `CancelActiveWorkAndWait`) only sends `session/cancel`, like the reference
  SDK's `cancel_active_work`; it no longer cancels pending permission
  requests locally. Only the Agent knows which work a request belongs to:
  it cancels the requests of the cancelled foreground work with
  `$/cancel_request`, which `CancellablePermissions` resolves as the
  cancelled outcome, and requests for background work stay open (ACP
  background-work RFD revision of 2026-11-02). `SessionHandleOptions` loses
  its `Permissions` field, which only served that local cancellation; call
  `CancellablePermissions.CancelPermissionRequests` explicitly to cancel
  every pending request of a session (#70).
- **Breaking:** the v1 agent runtime's `Client.Notify` gates
  `session/update` params of every type, not only `acp.Update`: a typed
  `schema/unstable.SessionNotification`, raw JSON, or any value that encodes
  as a session update. Before writing, with `-32601`, it refuses `notice`
  without `session.notices`, `compaction_update` and
  `compaction_summary_chunk` without `session.compaction`, a
  `config_option_update` carrying a boolean option without
  `session.configOptions.boolean`, `plan_update` and `plan_removed` without
  the unstable `clientCapabilities.plan` (never gated before, even typed),
  and `subagent_update`, `session_message`, and `session_message_chunk`
  without the unstable `clientCapabilities.subagents`. Params that do not
  decode, including the positional form, are refused by their
  `sessionUpdate` tag for every kind the tag alone decides. The v1 runtime
  now reads the initialize request's client capabilities with the unstable
  schema for these checks.
- **Breaking:** `unstable.Projection.Notifications` returns an error for a
  subagent association the projection's `SubagentTree` refuses, instead of
  ignoring subagent and session-message updates.
- **Breaking:** every stable dispatch layer answers params that do not
  decode with the reference SDK's `json_cast_params` `-32602` (#94): code
  `-32602`, message `Invalid params`, and data
  `{"error": <serde error>, "json": <params>, "phase": "deserialization"}`.
  This covers `agent.Runtime`, the draft-v2 `agent.Runtime`,
  `agent.HandleClientHost` and its `HandleFilesystem`, `HandleTerminal`,
  `HandlePermissions`, and `HandleElicitation` parts, the v2
  `HandleClientHost`, `HandlePermissions`, and `HandleElicitation`,
  `acp.HandleElicitation`, `clienthost.Host`, and the `initialize` of
  `acptest.AgentHandler`, which now decodes the typed v1 or v2 request.
  They previously answered `-32602` with the Go decode error as the message
  and no data. The unstable `Handle` routes and the draft-v2 `initialize`
  already used this shape and now share one constructor with the rest; the
  echoed `json` now holds the params as `serde_json::Value` does (compact, a
  repeated key at its first position with its last value, numbers as
  serde_json writes them) instead of the received bytes.
- **Breaking:** the unstable `mcp` and `v2/mcp` routers answer an
  `mcp/message` envelope that does not decode with the same
  `json_cast_params` `-32602` instead of the decode error as string data, and
  `Attachment.AppendDeclarations` and `HandleSessionSetup` decode session
  setup params as the method's request before declaring a server, as the
  reference SDK's `validate_session_setup` does (#94): params that do not
  decode, absent and `null` included, answer that `-32602` and are not
  forwarded.
- **Breaking:** the stable dispatch layers answer a request no handler serves
  with the reference SDK's `-32601`:
  `{"code":-32601,"message":"Method not found","data":"<method>"}`, as Rust
  answers for both roles and protocol versions, extension methods included
  (#94). This covers `agent.Runtime` and the draft-v2 `agent.Runtime`
  (unknown methods, and optional methods the agent does not implement or
  advertise), `agent.HandleClientHost` and its parts, the v2
  `HandleClientHost`, `HandlePermissions`, and `HandleElicitation`,
  `acp.HandleElicitation` (also without a configured handler),
  `clienthost.Host`, `acptest.AgentHandler`, and the unstable `mcp` and
  `v2/mcp` `MessageRouter.Handle` and `Attachment.HandleSessionSetup` without
  `next` (Rust's MCP-over-ACP handlers decline every other method, which
  then gets the same answer). They previously answered
  `agent does not implement method: X`, `unsupported method: X`,
  `elicitation support is not configured`, or a bare `-32601`. The unstable
  dispatch shares the same constructor. Unknown notifications are still
  ignored, as in Rust.
- **Breaking:** the stable runtimes, client hosts, and notification adapters
  (`SessionUpdates`, `ElicitationComplete`, the session projections and
  trackers, and the runtimes' `session/cancel`) read absent params as `null`,
  as the reference SDK's JSON-RPC layer does for every message (#94). A
  message without params decodes, and fails, exactly like one with
  `"params": null`: `session/list`, `logout`, and v2 `auth/logout` decode
  either as their default request, and any other request fails with
  ``invalid type: null, expected struct X``. Absent params previously failed
  with Go's `unexpected end of JSON input`.
- **Breaking:** the generated decoders in all four schema packages fail with
  serde's exact error text, which reaches peers in those `-32602` answers
  (#94). `TestDeserializeHints` now requires it for every rejected case. An
  error no longer carries a field path or Go type name (such as
  `NewSessionRequest.cwd: invalid type: number, expected a string`); a struct
  reports the first value that does not decode in wire order before the
  first missing required field in Rust declaration order; values are quoted
  as serde quotes them (``invalid type: integer `5`, expected a string``,
  ``invalid value: integer `-1`, expected u32``, ``missing field `cwd` ``,
  ``unknown variant `x`, expected `a` or `b` ``); integers name their Rust
  width (`u16`, `u32`, `u64`, `i64`), v1 `PathBuf` and v2 `AbsolutePath`
  fields expect a `path string`, structs `struct X`, and a union Rust reads as
  untagged fails with `data did not match any variant of untagged enum X`.
- Every method the unstable `Handle` and `Install` routes serve (v1 and
  draft v2) decodes absent params as `null`, as the reference SDK's JSON-RPC
  layer does for every message: `providers/list`, `auth/status`, and the
  archive-aware `session/list`, which read `null` as the default request, no
  longer answer `-32602` without params, and a method with required fields
  answers absent and `null` params with the same error.
- The generated v1 and draft-v2 unstable `SessionStatusResponse` decode a
  malformed optional field as not reported, per its
  `x-deserialize-default-on-error` hints, so a malformed draft-v2 `state`
  snapshot reads as nil instead of failing the response, as the session-status
  RFD requires.
- The generated `SessionInfo` in all four schema packages decodes a malformed
  optional field (`archived` in the unstable packages, `title`, `updatedAt`,
  `_meta`, and v1 `additionalDirectories`) as not reported, per its
  `x-deserialize-default-on-error` hints, as the reference SDK's
  `DefaultOnError` does, so one malformed `archived` reads as nil instead of
  failing the whole `session/list` page. v1 `additionalDirectories` also
  skips its invalid items and keeps the rest, as below.
- **Breaking:** the generated decoders in all four schema packages honor the
  schema's `x-deserialize-*` hints on every field and decode everything else
  as strictly as the reference SDK's schema crate (`agent-client-protocol-schema`
  1.15.1), replacing the per-def `lenientDefs` special case (#82):
  - `x-deserialize-default-on-error` (`DefaultOnError`): a malformed value
    reads as the field's default: not reported for optional fields (where
    `null` reads the same way), unset for a malformed `Nullable` patch field
    (such as `CompactionUpdate`'s `summary`, `error`, and `_meta`, whose
    explicit `null` is still kept as null), and an empty list for a required
    list. A malformed `_meta` or one malformed optional field no longer fails
    the message.
  - `x-deserialize-skip-invalid-items` (`VecSkipError`): list items that do
    not decode are dropped and the others kept, so one bad `availableCommands`,
    `configOptions`, `sessions`, `ToolCall.content`, or `SessionMessage.content`
    entry no longer drops the whole message or list.
  - `x-deserialize-default-on-null` (`DefaultOnNull`): `null` reads as the
    default, including an empty list for required lists such as
    `McpServerStdio.args`.
  - **Breaking:** a missing required field, a `null` where the schema does
    not admit one, and a `null` list item or map value now fail decoding, as
    they do in Rust. A union without its required tag (for example a v2
    `BackgroundWork` or `AvailableCommand` without `type`) is rejected
    instead of decoding as `Other` with an empty `Kind`, and a known tag
    whose payload is missing required fields (such as `{"type":"terminal"}`
    without `terminalId`) is rejected, or read by the union's untagged
    variant where it has one, in schema order. An open variant needs the
    fields its schema arm requires (a v2 `AuthMethod` with a custom `type`
    still needs `methodId` and `name`). Generated structs also match wire
    keys exactly instead of case-insensitively.
  - **Breaking:** closed enums reject values they do not name, as Rust does:
    in v1 `StopReason`, `ToolCallStatus`, `PlanEntryPriority`,
    `PlanEntryStatus`, `Role`, `PermissionOptionKind`, and `StringFormat`,
    and in both versions `ElicitationSchemaType` (whose only value is
    `object`). A malformed optional enum reads as not reported, and an
    unknown list item is skipped, where the hints say so. v1 `ToolKind` reads
    an unknown value as `other` (`#[serde(other)]`). Enums the schema leaves
    open, which are all the other draft-v2 enums, still keep any string.
  - **Breaking:** unions of primitives (such as `SessionConfigSelectOptions`
    and `ElicitationContentValue`) accept only values of their arms instead
    of any JSON value; the Go types are unchanged.
  - Like serde, structs also decode from a JSON array, positionally in Rust
    field declaration order, and tagged unions from `[tag, fields...]`.
    Only what Rust flattens decodes from objects alone: unions with shared
    fields, `IdleStateUpdate`, `McpError`, and v1 `ToolCallUpdate` and
    `WaitForTerminalExitResponse` (v2 `ToolCallUpdate` reads from an array).
    Enums also read the map form `{"variant": null}`. Where serde decodes
    from buffered content (a default-on-error field, a skip-invalid list
    item, a union payload, a union with untagged variants), a non-negative
    integer tag selects the tagged variant at that index.
  - Decode errors use serde's wording and no longer name Go types.
  - v1 `McpServer` (and the other v1 unions with a tag-less variant) read any
    object their tagged variants do not, whatever its `type`, as the
    reference SDK's untagged fallback does.
  - v2 `ToolCallLocation.line` decodes as Rust's `NonZeroU32`: `0` reads as
    no line.
  - The flattened v2 `IdleStateUpdate.stopReason` reads a stop reason whose
    fields do not decode as not reported, and a custom stop reason no longer
    keeps `usage` among its fields.
  - Encoding is unchanged. Decoding splits objects without copying member
    values, so a `session/update` decodes faster and with fewer allocations
    than before. Each package's generated `TestDeserializeHints` compares
    every hinted and required field, enum, union, and array form with
    outcomes captured from the Rust crate (`testdata/hints_rust.json`,
    recaptured by `scripts/capture_hint_fixtures.py`).
- `mcp.MessageRouter` and the v2 `MessageRouter` follow the reference
  binding for low-level `Register` services too:
  - an active request is cancelled when the connection's input reaches EOF,
    and then gets no response;
  - a request whose outer cancellation arrives before the service starts is
    not executed;
  - a result that loses a race with cancellation is discarded, and the
    cancellation error is sent instead;
  - a request cancelled by `Unregister` is answered with `-33001`
    `MCP provider removed` instead of `-32800`;
  - an unknown server is `-33001` `MCP server registration unavailable`, a
    duplicate active request ID is `-32602` `Invalid params` with data
    `duplicate active MCP requestId`, and a malformed envelope is `-32602`
    `Invalid params` with the decode error as data;
  - a service error is `-33002` `MCP backend failure` with the error text in
    `data`, including a `context.Canceled` returned when the request was not
    cancelled (previously `-32800`).
- v1 `mcp.NewSession` sends `"mcpServers": []` instead of `null` when no
  servers are given; the v1 schema requires the array.
- `acpv2.ActiveWork`, and so `SessionTracker`, `SessionHandle.WaitForIdle`,
  and `v2/runner`, follow the reference one-shot client and the v2 prompt
  lifecycle more closely (#59):
  - Any state other than `idle` (`requires_action` or an unknown state, not
    just `running`) starts the tracked work.
  - **Breaking (draft v2):** the work completes at the next `idle` whatever
    its stop reason: known, `error`, custom, unknown, omitted, `null`, or
    malformed, and with or without a non-empty `background` list. Its result
    is fixed there. A repeated idle snapshot, or output after the idle, no
    longer changes it; previously an idle without a stop reason let a later
    idle fill one in, and later output was appended to `Text`.
  - **Breaking (draft v2):** the error `SessionHandle.CancelActiveWorkAndWait`
    returns when the work became idle without `cancelled` changed. It now
    wraps `ErrWorkNotCancelled` (and the `*WorkError` for `error`), and its
    message names the stop reason instead of printing a pointer, so code
    matching the old "active work stopped without cancelled reason" text
    breaks. Because the result is fixed at the first idle, a later idle can
    no longer supply `cancelled` either. Only `cancelled` satisfies the
    barrier, as before; it covers stops the user or Client chose
    (agent-client-protocol #2523, #2576).
  - Tracking assumes each prompt starts new foreground work. A prompt the
    Agent folds into the running turn may leave the tracked work incomplete,
    and `CancelActiveWorkAndWait` returns the earlier result of work that had
    already completed, so bound `WaitForIdle` and `CancelActiveWorkAndWait`
    with `ctx` (documented on `ActiveWork` and `SessionHandle`).
- **Breaking (draft v2):** `v2/runner.Runner.Execute` returns a non-nil
  `*acpv2.WorkError` for a turn that ended with the `error` stop reason,
  where it returned a nil error before. It still closes the session and
  shuts the agent down gracefully, and returns the result, projected text
  included; the agent's stderr is recorded but not appended to that error.
  If `session/close` or the agent's exit then fails as well, that failure is
  joined with the `*acpv2.WorkError`, so `errors.As` finds each.
- Breaking: the v1 agent runtime refuses a `notice` unless the client advertised
  `session.notices`, a `compaction_update` or `compaction_summary_chunk`
  unless it advertised `session.compaction`, and a `config_option_update`
  carrying a boolean option unless it advertised
  `session.configOptions.boolean`, returning an `*acp.RPCError` `-32601`
  (the shape of its host-method refusals) without writing the update (#57).
  The check covers `SessionUpdater.Update` and `Client.Notify` with
  `session/update` params of type `acp.Update` or `*acp.Update`, so history
  replay during `session/load` is gated as the protocol requires. Updates
  sent through `Client.Notify` with other params types, such as raw JSON,
  are not checked. Boolean options in responses (`session/new`,
  `session/load`, `session/set_config_option`) are not gated.
- Breaking: v1 `schema.CompactionUpdate` and `schema/unstable.CompactionUpdate`
  generate `Summary`, `Error`, and `Meta` as `Nullable`
  (`Nullable[[]ContentBlock]`, `Nullable[string]`, `Nullable[Meta]`), as
  Rust's `MaybeUndefined` does, so omission, `null`, and `[]` stay distinct
  on decode and can all be sent (#57). The v1 packages gain the `Nullable`
  type for them. v1 `_meta` still replaces as a whole.
- Breaking: v1 `schema.SessionInfoUpdate` and
  `schema/unstable.SessionInfoUpdate` generate `Title` and `UpdatedAt` as
  `Nullable[string]` instead of `*string`, matching Rust's `MaybeUndefined`
  (#57): an omitted field keeps the stored value and `null` clears it, and
  both can now be sent. `_meta` stays a plain optional `Meta`, as in Rust.
  `cmd/acpgen`'s `v1PatchDefs` lists patch fields per def and field, and
  fails generation if a listed field is missing, required, or not nullable.
- Breaking (unstable): `schema/unstable.SessionMessage` (sender and
  recipient session and message IDs, `content`, `_meta`) and
  `schema/unstable.SubagentUpdate` (`title`, `description`, `capabilities`,
  `state`, `cwd`, `additionalDirectories`, `_meta`) generate those fields as
  `Nullable`, matching Rust's `MaybeUndefined`, so `null` can be sent and is
  kept distinct from omission.
- A `Nullable` field is sent only when `Set` is true: callers set
  `Set: true` to send a value as well as to send `null`.
- The new v1 `Nullable` fields read a malformed value as omitted
  (`DefaultOnError`) and skip invalid summary blocks (`VecSkipError`), as Rust
  does (#82).
- Breaking: `unstable.Projection` hands notices and compactions to
  `acp.SessionProjection` (#57). `CompactionState` is now an alias of
  `acp.CompactionState`, which uses the stable schema types and adds `Meta`;
  `Notices` and `DrainNotices` return stable `schema.Notice` values; and
  compactions follow the patch rules above, so an omitted `summary` or `error`
  keeps the stored value instead of clearing it, and a compaction first seen
  through a summary chunk starts `in_progress` instead of with no status.
  `Notifications` returns the error when a notice or compaction does not
  convert to its stable form instead of dropping it; `Observe` skips such a
  value, which only Go code can build (every content shape that decodes from
  the wire converts).
- **Breaking (draft v2):** `v2/mcp.NewSession` and `v2/mcp.ResumeSession` now take the
  unstable draft-v2 initialize response (from
  `v2.Connection.InitializeExtended`) and send the unstable requests, like
  `v2/mcp.ForkSession` and the v1 `mcp` package. This is how they can carry a
  `systemPrompt`, which the stable draft-v2 types do not model.
  `ResumeSession` takes `schema/v2/unstable.ResumeSessionRequest`, and
  `NewSessionOptions.Servers`, `NewStdioServer`, and `NewHTTPServer` use the
  unstable `McpServer`. As a result, both also accept the native `acp`
  transport, gated on `capabilities.session.mcp.acp`, as the reference SDK's
  `McpServer` does once MCP-over-ACP is enabled. The stable draft-v2 request
  cannot carry `systemPrompt`, and the stable `acpv2.Initialization` drops
  the `systemPrompt` capability that gates it, so no signature-compatible
  change could send or check the prompt; a second, unstable-typed set of
  helpers next to the old ones would be a compatibility path. Migrate by
  initializing with `InitializeExtended` and passing its unstable response.
- `unstable.ForkSession`, `v2/unstable.ForkSession`, `mcp.LoadSession`,
  `mcp.ResumeSession`, `mcp.ForkSession`, `v2/mcp.ForkSession`, and the v1
  and v2 attached-session helpers no longer forward a `systemPrompt`
  unchecked. They apply the system prompt checks above before writing.
  `v2/mcp.NewAttachedSession` and `ResumeAttachedSession` now share the
  `NewSession` and `ResumeSession` checks.
- **Breaking:** `acphttp.Server.Path` moved to `Server.Options.Path`. Build a
  server with `acphttp.NewServer(entry, acphttp.ServerOptions{...})`.
- **Breaking:** the draft HTTP server answers with the Rust server's status
  codes and bodies: `413 POST body too large` above 16 MiB (previously 8 MiB),
  including when `Content-Length` declares more; an empty `404` for an unknown
  connection or path; an empty `500` instead of `410` when the connection has
  ended; an empty `202` without the `Acp-Connection-Id` header; the Rust texts
  for the other plain-text errors; `405` with `Allow: POST,GET,HEAD,DELETE`.
  `params.sessionId` now satisfies the session requirement without the header,
  and the header is written into `params.sessionId` for the agent, refusing a
  mismatch with `400`. Every `initialize` request opens a new connection,
  whatever `Acp-Connection-Id` says. An `initialize` error response is
  returned without a connection id and discards the connection. The 30-second
  initialize timeout is gone, as in the Rust crate: a client that gives up
  cancels its request, which discards the connection. `POST` frames are
  queued for the agent instead of waiting for it to read them. Connection ids
  are UUID v4 strings.
- The draft HTTP server forwards every `POST` body the Rust SDK cannot read
  as one JSON-RPC message, as the Rust crate forwards malformed frames: valid
  JSON that is not a message (a non-object, a wrong `jsonrpc`, a non-string
  method, params that are not an object or array, a bad id, or a response
  without exactly one of `result` and a valid `error`) is now answered `202`
  and the agent sends `-32600` on the stream, instead of a `400` that could
  show Go type names. Such a body without a connection gets
  `400 Acp-Connection-Id header required`. A body that spans lines is made one
  line for the agent as the Rust line writer does: JSON is written again as
  `serde_json` writes it, and anything else is sent as a JSON string, so a
  multi-line body that is not JSON is answered `202` and `-32600` instead of
  `400`.
- The draft HTTP transport sends every JSON-RPC message as the Rust SDK
  serializes its `RawJsonRpcMessage`, instead of forwarding the bytes it
  received: the server for client `POST`s, the `initialize` response, and the
  agent's event-stream frames, and the client for its `POST` bodies and the
  frames it hands its connection. Members are written in the order
  `jsonrpc`, `id`, `method`, `params` or `jsonrpc`, `id`, `result`/`error`;
  other envelope members and a `null` `params` are dropped; an error object is
  `code`, `message`, `data`, then its other members; and values are written as
  `serde_json` writes them (escapes decoded, the last duplicate member kept,
  non-integer numbers as `f64`, such as `1e2` as `100.0`). The
  `Acp-Session-Id` header is appended at the end of `params`. A message with
  an empty `method` is a request or notification, as in the Rust SDK.
- The draft HTTP server sends an agent line that is not a JSON-RPC message to
  the connection stream verbatim, empty and blank lines included (previously
  dropped), and only a valid response to the `initialize` request answers the
  `initialize` `POST`. Event data is written as axum writes it: an empty
  frame is a blank line, and a `\r` in a frame starts a new `data:` field.
- The HTTP client reads event streams as the Rust client's parser does: lines
  end at `\r\n`, `\r`, or `\n`, a `data` field loses one leading space instead
  of all surrounding whitespace, `data` fields are joined with `\n`, and an
  event the stream ends in the middle of is dropped. Bytes that are not UTF-8
  stall the stream, as in the Rust parser: nothing after them is delivered,
  and when the stream ends it fails with the UTF-8 error. A stream opens on
  any `2xx` status (previously only `200`), and a stream that cannot open or
  ends fails the transport with the Rust client's message:
  `connection SSE stream ended: ` or `<session id> SSE stream ended: `, then
  `HTTP 404 Not Found`, `SSE stream closed`, or
  `UTF8 error: invalid utf-8 sequence of 1 bytes from index 0`. Later frames
  fail with that error.
- The HTTP client's errors are the Rust client's error data:
  `initialize: HTTP 500 Internal Server Error: ` and the body,
  `initialize: invalid initialize response: ` and the Rust SDK's error,
  `initialize: server did not return acp-connection-id header`,
  `POST: HTTP 500 Internal Server Error: ` and the body,
  ``POST: method `session/prompt` requires sessionId in params`` for a
  session method without a session, and
  ``ACP HTTP transport: first message must be `initialize` `` for a first
  frame that is not an `initialize` request.
- The HTTP client posts a frame its connection writes that is not one
  JSON-RPC message verbatim, as the Rust client does, with no session header
  and no stream bookkeeping, instead of failing.
- The draft HTTP server ends a connection whose agent writes a line that is
  not UTF-8, as the Rust line reader's error ends the agent's transport: the
  frames before the line are delivered and the streams end, the line and
  everything after it are dropped, the agent's input closes, and later
  requests for the connection get `404`. Before the `initialize` response,
  the `initialize` `POST` gets `500 agent closed before initialize response`.
  Previously the line was forwarded.
- A `POST` body that is not valid UTF-8 gets the Rust text,
  `Invalid JSON-RPC: ` followed by `std::str::Utf8Error`'s message, such as
  `invalid utf-8 sequence of 1 bytes from index 0` or
  `incomplete utf-8 byte sequence from index 5`.
- Once the agent behind a connection closes its input or returns, later
  `POST`s get `500`, as in the Rust crate, instead of `202`.
- The HTTP client reads the `initialize` response body whole instead of
  cutting it at 16 MiB, sends `Accept: application/json` on every `POST`, and
  accepts any `2xx` status for later `POST`s, as the Rust client does. It
  stores `Acp-Connection-Id` before checking the `initialize` response, so a
  non-`2xx` status or a body that is not a JSON-RPC message sends `DELETE` for
  that connection. An `initialize` error response rejects the connection even
  when it carries the header: the error is delivered, the connection is
  deleted, and the next frame must be `initialize`.
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
- **Breaking:** `acp.Connect`, `v2.Connect`, `acptest.Endpoint.Connect`,
  `acphttp.Dial`, and `acphttp.Client.Dial` gain a trailing variadic
  `...acp.ConnectOption` parameter (#73). Calls compile unchanged, but code
  that stores one of these functions in a variable or field of the old
  function type must update that type.
- **Breaking:** one bad message from a peer, or one failing handler, no
  longer closes an `acp.Connection` and fails its other in-flight requests,
  matching the reference Rust SDK 5.0 (#73).
  - Valid JSON that is not a JSON-RPC message, as the Rust
    `RawJsonRpcMessage` deserializer judges it, gets
    `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request"}}`
    and the connection keeps reading. This covers a non-object value, a wrong
    or missing `jsonrpc`, a non-string `method`, `params` that is not an
    object, array, or null, an id that is not null, a string, or an int64
    integer (`1.5`, `1e3`, `-0`, `2^63`), and an object with `method` plus
    `result` or `error`. Member names match exactly, so `"Method"` is not a
    method. Previously the connection closed.
  - A malformed value shaped like a response (no `method`, with `result` or
    `error`) is dropped, as Rust's `frame_entry` does: no answer, no pending
    call completed. This covers both `result` and `error`, a missing or
    invalid id, a wrong `jsonrpc`, and an error object `RPCError` rejects.
    Previously the connection closed. A repeated response for the same id is
    ignored instead of closing the connection. A repeated member of a
    response's error object, such as a second `code`, keeps its last value, as
    Rust reads the whole message as a `serde_json` value first; `RPCError`
    decoding on its own still rejects it.
  - A request with a null id runs normally on a version-neutral or v1
    connection, as JSON-RPC and Rust's v1 connections allow. On a connection
    with protocol version 2, including every `v2.Connect` connection, it gets
    `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"ACP protocol v2 request ids must be a string or an integer, not null"}}`
    without running a handler, and the draft-v2 agent runtime sets version 2.
    Previously either closed the connection. `agentrouter` answers a
    v2-selecting null-id first frame the same way and keeps reading (#62).
  - A request handler panic is recovered. The reference SDK does not catch
    panics; acp-go answers as Rust answers a request whose handler dropped
    its responder without responding, which is the closest Rust wire
    equivalent:
    ``{"code":-32603,"message":"Internal error","data":"request handler dropped its responder for `<method>` without responding"}``,
    or `{"code":-32800,"message":"Request cancelled"}` when the request was
    already cancelled. A panicking notification handler is recovered too. The
    panic and its stack go to the `WithErrorHandler` hook, or to the standard
    logger without one; neither reaches the peer. A panic in the hook itself
    is recovered and logged. A request whose handler panics after input
    closed (`InputClosed`) is not answered, as Rust sends nothing for a
    dropped responder while shutting down and as the MCP bindings leave a
    request unanswered after EOF. Previously a handler panic crashed the
    process.
  - A cancelled incoming request is answered with the message
    `Request cancelled`, Rust's text for `-32800`, instead of
    `request cancelled`.
  - An error returned by a `Notifications` function goes to the
    `WithErrorHandler` hook, or is dropped, instead of closing the
    connection, as Rust logs and ignores it. A malformed `session/cancel` or a
    failing v2 `CancelSession` no longer disconnects the client.
  - The limit of 32 concurrent incoming requests is removed: Rust has no such
    limit, and exceeding it closed the connection. Every incoming request runs
    in its own goroutine.
  - Invalid UTF-8, an oversized frame, and transport failures still close the
    connection.
- **Breaking:** version negotiation in the agent runtimes, routers, and draft
  v2 client (#62):
  - The draft v2 agent runtime answers a request below v2 with `-32022`
    listing `[2]`, instead of a generic `-32600`, without calling
    `Initialize`. It accepts a request for a newer version and selects v2:
    the agent sees `protocolVersion: 2` and what parses under v2, and a
    missing or malformed `info` becomes name and version `"unknown"`. A
    request for exactly v2 must carry an `info` that parses, or it gets
    `-32602` with Rust's deserialization data,
    ``{"error": "missing field `info`", "json": <params>, "phase": "deserialization"}``;
    the rest is read as leniently as the reference schema reads it, so a
    `_meta`, `capabilities`, a capability, or an `info` title or `_meta` that
    does not parse is ignored instead of rejected.
  - The v1 and draft v2 agent runtimes set the negotiated version, 1 or 2, on
    every initialize response, as Rust sets it on the serialized result,
    including the value an `InitializeResult` hook returns. An agent or hook
    that selected another version previously failed the request with
    `-32603` (`agent selected unsupported ACP version N`).
  - The v1 and draft v2 agent runtimes apply their initialization lifecycle
    in wire order, on the connection's read loop, like Rust's incoming actor,
    instead of in each handler's goroutine. On the draft v2 runtime, a
    repeated `initialize` followed immediately by another request now always
    begins first, so the other request is refused with `-32600` and the
    repeat succeeds; previously either could win. (The v1 runtime's lifecycle
    is unchecked, so a v1 repeat never refuses the next request.) Negotiation failures and lifecycle refusals are
    answered in wire order without starting a handler, and a refused request
    no longer reaches an `acp.WithRequestObserver` function.
  - The draft v2 agent runtime accepts a repeated `initialize` sent after the
    previous one completed and before any other request, and negotiates it
    like the first one; the latest successful one is authoritative, including
    its client capabilities. A repeat that fails, whether refused or failed by
    the agent, keeps the previous initialization and the window open. The
    first other request closes the window, after which a repeat gets the
    existing `-32600` refusal. Other requests are refused while a repeat is in
    progress, and a concurrent `initialize` is still refused.
  - The v1 agent runtime keeps v1's fallback-to-latest rule: any valid
    `protocolVersion` selects v1, instead of a `-32600` for anything but 1. A
    request for a newer version is normalized to v1's shape: identity,
    terminal authentication, elicitation, and `_meta` carry over when they
    parse, and v1 file-system or terminal capabilities are never enabled.
  - The v1 agent runtime follows Rust's unchecked v1 lifecycle: every
    `initialize` reaches `Initialize`, including a repeat at any time and one
    sent while another is in progress, so the in-progress and cannot-repeat
    refusals no longer apply on v1. Other requests are still refused until the
    first `initialize` succeeds. A successful repeat replaces the advertised
    capabilities; a failed one keeps the previous initialization.
  - `agentrouter` routes like Rust's agent protocol router. A request for v2
    or newer selects v2. When v1 is configured, everything else goes to v1
    unchanged: a request for v1 or older (including `0`, previously rejected),
    a malformed `initialize`, any other request, and a notification. A v2-only
    router answers a request below v2 with `-32022`. A newer request with a
    missing `info` is normalized instead of rejected, and a v2 request
    normalized for a v1-only router keeps what parses under v1. Rejections use
    Rust's error shapes and the router keeps reading instead of closing.
    Responses before initialization, and notifications with no v1 to receive
    them, are ignored. A JSON object that is not a JSON-RPC message gets
    `-32600` with `id: null`, and a v2-selecting request with a `null` id gets
    `-32600`.
  - `proxyrouter` negotiates `proxy/initialize` the same way, like Rust's
    proxy protocol router, instead of requiring the exact configured version:
    it selects the highest configured version at or below the requested one,
    replays an exact-version frame unchanged, normalizes any other, answers an
    unservable request with `-32022`, and keeps reading after rejections.
  - The agent and proxy routers reject an exact v2 request whose `info` does
    not parse with serde's reason, such as
    ``"invalid initialize params: missing field `info`"``, and read the rest
    leniently, like Rust's routers. The routers share one frame reader.
  - `v2.Connection.InitializeWithInfo` returns an
    `*acp.InitializationVersionMismatch` error when the agent selects v1, and
    no longer closes the connection when the agent selects a lower or
    unsupported version.
  - The draft v2 client may repeat `initialize` after one completed and
    before it sends any other request, like Rust's initiator; the latest
    successful one is recorded. A failed repeat keeps the previous
    initialization instead of clearing it. A repeat after another request, or
    one sent while another is in progress, fails locally with an
    `*acp.RPCError` `-32600` carrying Rust's message, replacing "ACP
    connections may only be initialized once".
  - A `v2.Connect` connection follows Rust's v2 initiator lifecycle. Until an
    `initialize` completes, and while a repeat is in progress, its own
    requests and notifications, including `CancelRequest` outside an
    `initialize`, fail with `-32600` and
    ``"ACP initialization must complete before `<method>` can be used"``
    before anything is written; the agent's requests get the same error
    without running the handler; the agent's notifications are dropped; and an
    `initialize` from the agent gets `-32600`
    after it is negotiated like an agent would (`-32022` below v2, `-32602`
    for invalid params). A successful response either way while no
    initialization is complete becomes the same `-32600`. Previously all of
    these were sent or handled. Every `initialize` the connection sends,
    including one through `Call` on the embedded `acp.Connection`, is
    negotiated and recorded: its `protocolVersion` is set to 2 in place and its
    params validated before it is written, and its response is checked when it
    arrives, even after the caller gave up (a cancelled initialize stays in
    progress until its response completes or fails it). A v1 selection outside
    `SendInitialize` fails with Rust's `-32600` "required ACP protocol version
    2 but peer negotiated 1". `SendInitialize` writes the request in Rust's
    member order, `protocolVersion` first. Any other request closes the repeat
    window.
  - The draft v2 client checks an initialize response as Rust does: the
    `protocolVersion` member must be named exactly that; a v2 response whose
    `info` does not parse fails with json_cast's `-32700` (`Parse error`) and
    data `{"error": <serde reason>, "json": <response>, "phase": "deserialization"}`;
    and everything else is read leniently (member names exactly, unparsable
    capabilities, auth methods, or `_meta` ignored), for `Initialization`, the
    v1 response, and `InitializeExtended`'s target. An empty `info` name or
    version is accepted, as Rust accepts it.
  - The draft v2 agent runtime drops notifications that arrive before an
    `initialize` completes or during a repeat (except `$/cancel_request` during
    an `initialize`), as Rust's responder does, and its own requests and
    notifications fail with the same `-32600` until then; an `initialize` it
    sends is validated and then fails with the wrong-direction `-32600`. A
    successful response to the agent's request that arrives during a repeat,
    and one the agent sends then, becomes the same `-32600`. The typed client
    helpers' "client has not initialized the agent" error is replaced by
    Rust's initialization-required error.
  - The v1 and draft v2 agent runtimes complete an initialization when its
    response is queued, as Rust's outgoing_response_to does, so nothing the
    agent sends can precede the response on the wire.
  - The v1 agent's `RequestPermission` is no longer refused before
    initialization, as Rust's unchecked v1 agent does not gate its own
    requests; the capability-gated file-system, terminal, and elicitation
    helpers report the missing capability (`-32601`) instead of "client has
    not initialized the agent".
  - Initialize negotiation reads params and responses with the generated
    decoders (#82), as serde reads them: a case variant such as `"Info"` or
    `"Capabilities"` is an unknown member and is ignored, a member that does
    not parse is ignored at every level the schema marks default-on-error
    (for example `capabilities.auth.terminal: "bad"` keeps the rest of
    `auth`), and no Go error text reaches the wire.
  - `v2/mcp.NewMessageClient` takes a `v2/mcp.Caller` (any value with
    `acp.Connection`'s `Call` method) instead of `*acpv2.Connection`, so the
    agent, the consumer side of MCP-over-ACP, can pass its `*acp.Connection`
    or its `v2/agent.Client`. Existing callers compile unchanged.
  - The v1 client (`acp.Connection.InitializeWithInfo`) follows Rust's
    unchecked v1 client: it may initialize again at any time, including while
    another `initialize` is in progress, instead of refusing a second
    successful one. A response with a missing or invalid `protocolVersion`
    fails with `-32602`, one selecting 2 with `-32600` ("required ACP protocol
    version 1 but peer negotiated 2; use a matching implementation for the
    negotiated protocol version"), and any other version with `-32600`
    ("unsupported ACP protocol version N; this endpoint only supports ACP
    protocol version 1"), as `*acp.RPCError`s; previously an unsupported
    version closed the connection with a plain error. `acp.Connection` itself
    is version-neutral, as one connection type serves v1 clients, v1 agents,
    and other peers, so a raw `Call` with `initialize` is sent as given and
    its response is not checked; `InitializeWithInfo` is the v1 client role
    that Rust's v1 client mode applies to every outgoing `initialize`.
  - The v1 agent runtime applies Rust's outgoing initialize handling: an
    `initialize` it sends carries `protocolVersion` 1, set in place. The v1
    agent runtime and both draft v2 endpoints refuse a request that already
    wraps an initialize in `proxy/successor` with Rust's `-32600` ("initialize
    requests must be sent as a logical `initialize` message; ...").
  - The draft v2 client fails an initialize response with a missing or invalid
    `protocolVersion` with `*acp.RPCError` `-32602`
    (`"initialize.protocolVersion must be a valid ACP protocol version"`),
    and one selecting a version other than 1 or 2 with `-32600`
    (`"unsupported ACP protocol version N; this endpoint only supports ACP protocol version 2"`),
    as Rust does, instead of plain errors.
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
  including one that holds `initialize` or `proxy/initialize`, with that same
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
  JSON-RPC message is answered or dropped too; see the #73 entry below.
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
  that is not valid UTF-8 is a plain-text `400`. The HTTP client fails an
  `initialize` whose response body is not valid JSON. (#65 extends this to
  every body that is not a JSON-RPC message, including multi-line ones.)

### Fixed

- The v1 and draft-v2 runners closed the agent's stdin with `Close` at the
  end of a successful run, dropping frames still queued for the agent, such
  as a response to its last request. They now call `Shutdown`, bounded by the
  run's context, before stdin closes, and a failed drain fails the run, as it
  fails `connect_with` in the reference SDK (#80). A failed or cancelled run
  still aborts with `Close`.
- On a clean peer EOF with no handler running, `acp.Connection` stopped at
  once and dropped frames still queued for the peer. As the reference SDK
  keeps its output working after incoming EOF, the connection now writes
  every frame queued by the time in-flight handlers return before it stops,
  and `Done` waits for that. From then on nothing new is sent (`Notify`,
  `Call`, and `CancelRequest` fail with `io.ErrClosedPipe`), and a
  `Shutdown` called during the flush waits for it. As in the reference SDK
  the flush has no time limit; `Shutdown`'s context or `Close` bounds it. A
  write failure during the
  flush becomes `Err` instead of `io.EOF` (#80).
- Two concurrent `initialize` requests on the v1 agent runtime could leave
  the client capabilities of one and the agent capabilities of the other;
  both are now recorded together (#62).
- The draft HTTP server's event stream takes queued frames one at a time, as
  the Rust crate does, so a write that fails on a departing subscriber no
  longer drops every frame queued behind it; they stay for the next
  subscriber.
- The draft HTTP server queues the `initialize` request before it starts the
  agent, so an agent that returns at once always gets
  `500 agent closed before initialize response`.
- The v1 and draft v2 agent runtimes could call `Agent.Initialize` twice when
  two `initialize` requests were in flight at once, because the
  already-initialized check released its lock before the handler ran. The
  runtimes now mark initialization as in progress under the lock and answer a
  concurrent `initialize` with `-32600` and the data "ACP initialization is
  already in progress on this connection" without reaching the handler. A
  failed initialization clears the in-progress state, so a later `initialize`
  can retry. Failures include a handler error and a rejected v2
  advertisement. The draft v2 runtime
  matches the reference Rust SDK. In the Rust SDK, the v1 agent mode does no
  lifecycle tracking (`InitializationRole::Unchecked`) and passes a repeated
  `initialize` to the handler; #62 aligned the v1 runtime with it (see
  Changed), so the in-progress refusal now applies to the draft v2 runtime
  only (#74).
- `v2.Connection.InitializeWithInfo` left the connection marked as
  initializing after the agent answered `initialize` with an error, so a retry
  failed with "ACP initialization is already in progress". A failed initialize
  now leaves the connection uninitialized for a retry (#62).
- The draft v2 agent runtime kept the client capabilities of an earlier
  initialization when a later one advertised none (#62).
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
