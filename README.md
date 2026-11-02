# acp-go

A small, standard-library-only Go client for [Agent Client Protocol v1](https://agentclientprotocol.com/protocol/v1/overview), extracted from [release-bot](https://github.com/BrokkAi/release-bot) for reuse by release-bot and issue-bot.

```sh
go get github.com/BrokkAi/acp-go@master
```

The generated-schema API below is the v0.2 surface.

```go
connection := acp.Connect(stdout, stdin, handleRequest, handleNotification)
defer connection.Close()
_, err := connection.InitializeWithInfo(ctx, acp.Capabilities{}, acp.ClientInfo{
    Name: "my-app", Version: "1.0.0",
})
// Handle err, then create a session and send prompts.
session, err := connection.NewSession(ctx, absoluteWorkingDirectory)
reason, err := connection.Prompt(ctx, session, "Inspect the project")
```

The import path is `github.com/BrokkAi/acp-go`; its package name is `acp`.
The runtime now uses the generated [`schema`](schema/) types directly: `Capabilities`,
`Initialization`, `Session`, `Content`, `Update`, and `ClientInfo` are aliases
into that package. This is an intentional v0.x API break. Use
`acp.WorkspaceCapabilities(readFiles, writeFiles, terminal)` for the former
boolean FS/terminal fields, `Session.SessionID` instead of `Session.ID`, and
the content constructors (for example `acp.NewTextContent`) instead of
hand-written discriminator fields.

The caller owns launching the process and implementing the filesystem, terminal,
and permission handlers it advertises. Capabilities default to disabled.
Authentication, modes (including generated mode state), model and boolean config
selection, reasoning effort, prompt content blocks, MCP servers, additional
directories, session load/resume/list/close/delete, logout, and typed error
classification are supported. Optional methods and prompt content are checked
against the agent's advertised capabilities before a request is sent. Use
`SetModel` before `SetEffort`: models may expose different effort choices.
Explicit selections must be acknowledged by the agent; they never silently fall back.

`acp.SessionUpdates` adapts `session/update` notifications to the generated
discriminated union. Session notices and compaction are stable v1 features: a
client advertises them with `SessionNoticesClientCapabilities` and
`SessionCompactionClientCapabilities` (combined with other session
capabilities through `SessionClientCapabilities`), and `acp.SessionProjection`
folds them per session in receive order. Compactions keep first-seen order and
apply `summary`, `error`, and `_meta` as patches (unset keeps, `null` or
`summary: []` clears, a value replaces), with summary chunks appending to the
current summary; notices are live events that `Notices` peeks and
`DrainNotices` consumes. Replay through `session/load` takes the same path.
`acp.HandleElicitation`, the elicitation response constructors,
`ElicitationComplete`, and `CombineNotifications` provide typed form/URL host
support, including the agent's URL-completion notification.
`NewSessionWithOptions`, `LoadSession`, and `ResumeSession` validate MCP
transports and additional workspace roots against initialization capabilities.

`Connect` owns and closes both streams. Notifications run in wire order before
responses and must return promptly without calling back into the connection.
Requests run concurrently and must honor cancellation. Frames are bounded at
8 MiB; incoming requests are not limited, as in the reference Rust SDK.
`Close` cancels and joins handlers.
Generic `Call` and `Notify` permit extensions. `CancelRequest`
sends a typed `$/cancel_request` for a single outstanding request, preserving
the peer's string or numeric ID. JSON-RPC batches are not supported on either
protocol version, matching the ACP v2 transports rule and the reference Rust
SDK 5.0: a JSON array, including `[]`, gets one `-32600` (`Invalid request`)
error with `id: null`, none of its entries run, and the connection keeps
serving. Send each message individually. Likewise, a line that is not valid
JSON (judged as `serde_json` judges it) gets one `-32700` error with
`id: null` and the line as `data`, and the connection keeps serving.

One bad message or failing handler never closes the connection, as in the Rust
SDK. Valid JSON that is not a JSON-RPC message gets `-32600` with `id: null`,
unless it is shaped like a response (no `method`, with `result` or `error`),
which is dropped without completing any call. A handler panic is recovered and
answered with `-32603` (`-32800` if the request was cancelled), and a
notification handler's error is not fatal; both go to the
`acp.WithErrorHandler` hook, and without one panics are logged. A request with
a null id runs on the version-neutral connection and on v1, while
`acp.WithProtocolVersion(2)`, which `v2.Connect` and the draft-v2 agent
runtime set, answers it with
`-32600` and `id: null`. Only invalid UTF-8, an oversized frame, or a
transport failure closes the connection. This root
transport is
also used by the draft-v2 package, while its typed v1 facade remains v1-only.

`*acp.RPCError` is the JSON-RPC error object as it appears on the wire, kept
losslessly like Rust's raw JSON-RPC error. `Call` returns it for an error
response, and a handler returns one (directly or wrapped) to choose its error
response. `Data` keeps an omitted `data` (nil), `"data": null`, and a value
apart, and `Extra` keeps every other member, such as extension fields, so a
relay can re-emit the object semantically unchanged; members after `code`,
`message`, and `data` are re-emitted in sorted key order. A handler that
returns a `*RPCError` obtained from `Call` therefore forwards the peer's
`Extra` members too. `code`, `message`, and `data` always come from the typed
fields, never from `Extra`, so no member is ever encoded twice. As in Rust,
decoding rejects an error object whose `code` or `message` is missing, null,
or mistyped, whose `code` is not an `int32` integer, or that repeats `code`,
`message`, or `data`, and a handler error whose `Code` does not fit `int32` is
answered with `-32603`. `acp.IsAuthRequired` and `acp.IsResourceNotFound`
classify by code alone. `RPCError.SchemaError` is the explicit conversion to the typed v1
`schema.Error`, and drops `Extra`, as Rust's `into_acp_error` does.

Run `go test -race ./...` and `go vet ./...`. No agent credentials are needed.
The fuzz seed corpus runs as part of that command; see
[CONTRIBUTING.md](CONTRIBUTING.md) for longer campaigns and opt-in tests
against real ACP agents.

`github.com/BrokkAi/acp-go/acptest` is the standard-library-only test harness.
`acptest.NewPair` returns an in-memory duplex transport that either a v1
`acp.Connection` or a draft-v2 `v2.Connection` can drive; `acptest.TestAgent`
executes deterministic typed prompt commands (`acptest.Command`), and the
package also ships v1/draft-v2 fake agents plus framing helpers (`Serve`,
`WriteRequest`, `ReadResponse`) for protocol-router tests. The router tests
themselves consume it, so the harness cannot drift.

## Agent runtime

`github.com/BrokkAi/acp-go/agent` serves an agent over stdio. Implement the
mandatory `agent.Agent` interface (`Initialize`, `NewSession`, and `Prompt`);
optional lifecycle and config methods are discovered as narrow interfaces such
as `SessionLoader`, `SessionCloser`, and `ConfigOptionSetter`. During a prompt,
call `SessionUpdater.Update` to emit typed `session/update` notifications.
Before writing an update, the runtime refuses a `notice` unless the client
advertised `session.notices`, compaction updates unless it advertised
`session.compaction`, and a `config_option_update` with a boolean option
unless it advertised `session.configOptions.boolean`. `Client.Notify` applies
the same check to `session/update` params of type `acp.Update`, so history
replay during `session/load` is gated too. A refusal is an `*acp.RPCError`
with code `-32601`, like an unadvertised host method;
`acp.RequireSessionUpdateCapability` exposes the check. The
runtime enforces protocol initialization, dispatches generated request/response
types, maps `session/cancel` to prompt-context cancellation, and rejects optional
methods omitted from the agent's advertised capabilities. It runs at most one
`Initialize` at a time: an `initialize` that arrives while another is in
progress gets `-32600` without reaching the agent, and a failed initialization
leaves the connection uninitialized so the client can retry. Lifecycle
refusals use the reference SDK's shape: `-32600`, message `Invalid request`,
and the explanation as a JSON string in `data`. The draft v2 runtime behaves
the same way.

The same package gives agents typed `agent.Client` methods for filesystem,
permission, terminal, and elicitation callbacks. Client applications can compose
typed hosts with `agent.HandleFilesystem`, `HandleTerminal`,
`HandlePermissions`, and `HandleElicitation`.
`github.com/BrokkAi/acp-go/clienthost` is the reference workspace-confined host
used by the runner: rooted filesystem access, process-group terminals, bounded
output, opt-in auto-approval, slog streaming, and JSONL transcripts.
Text-file reads accept only regular files; FIFOs and other special files are
rejected without waiting for data.

Runnable examples are included:

```sh
go run ./examples/minimal-client -agent-command 'go run ./examples/minimal-agent' -prompt hello
go run ./examples/drive-cli -command 'go run ./examples/minimal-agent' -prompt hello
```

`drive-cli` accepts the same mode/model/effort/auth options commonly needed by
external CLI adapters; substitute the real agent command for the minimal agent.

`github.com/BrokkAi/acp-go/cookbook` is a guide to common client, proxy, and
agent patterns, ported from the reference Rust cookbook. Each recipe is a
runnable example that `go test ./...` executes: one-shot v1 and draft-v2
prompts, building an agent, ordered application dispatch, draft-v2 session
coordination, a `proxyrouter` proxy component, and attaching MCP servers. Read
it with `go doc github.com/BrokkAi/acp-go/cookbook`.

## Wire schema

`github.com/BrokkAi/acp-go/schema` provides typed constants, request and
response structs, notifications, and a method registry for every message in
the pinned [ACP JSON Schema release](https://github.com/agentclientprotocol/agent-client-protocol/releases)
(`schema-v1.29.1` at the time of writing), including session updates, content
blocks, tool calls and their programmatic names, permissions, terminals,
elicitation, config options, session notices, and compaction.
The pins are the exact artifacts of Rust schema crate 1.15.1, which the
reference Rust SDK `agent-client-protocol` 5.0.0 pins.
The package is generated by `cmd/acpgen` and guarded by round-trip parity
tests, so tracking a new schema release is a reviewed regeneration rather
than hand transcription. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
update workflow.

acp-go exposes ACP's optional ("unstable") features through opt-in packages,
not through the stable surface. `schema/unstable` and `schema/v2/unstable` are
generated from Rust schema crate 1.15.1's unstable artifacts and carry the
combined optional feature types, including LLM providers, MCP-over-ACP, plan
operations, session fork/archive/status/title, auth status, client-provided
system prompts, subagents, proxy chains, and end-turn token usage. The
`unstable` (v1) and `v2/unstable` (draft v2) packages add typed facades on
top: capability-gated provider configuration, session fork, session
archive/unarchive, status, title, archived-filter listing, and the session
lifecycle requests that carry a client-provided system prompt for clients, and
`Install` for agents, which sets `Handle` dispatch, the `Advertise`
initialize hook, and the runtime `Overrides` (such as `session/list` with the
archive filter, and `session/new`, `session/load`, and `session/resume` with
the system prompt) on the agent runtimes together, and rejects a supplied
system prompt the connection was not told it may send before the handler
runs. The `mcp` and `v2/mcp` packages
carry the session helpers that accept MCP servers, including fork, alongside an
optional system prompt.
The stable `acp`, `v2`, `schema`, and `schema/v2` packages do not import or
expose any of it. See [docs/unstable-facades.md](docs/unstable-facades.md).

## Trace context

`github.com/BrokkAi/acp-go/tracecontext` sets and extracts the root-level
`_meta` keys `traceparent`, `tracestate`, and `baggage` that the ACP
extensibility conventions reserve for W3C Trace Context, so MCP and
OpenTelemetry integrations can correlate ACP requests, responses, and
notifications with the surrounding trace. `IntoMeta` and `FromMeta` operate on
the generated `schema.Meta` and `v2.Meta` maps (and the `Nullable[Meta]`
fields that wrap them in draft v2 and in v1 `compaction_update`), while
`ValidTraceparent` checks the
fixed W3C grammar without parsing the opaque `tracestate` or `baggage` values.
The package is standard-library only.

## Draft ACP v2

`github.com/BrokkAi/acp-go/v2` is a separate draft client for ACP v2, pinned
to the official `schema-v2.0.0-alpha.13` artifacts in
[`schema/v2`](schema/v2). It supports initialization and version selection,
`auth/login`/`auth/logout`, session creation/resume/list/delete/close,
prompt acceptance, cancellation, additional-directory capability checks, and
typed `session/update` dispatch. There is intentionally no v1/v2
conversion layer. The artifacts match Rust schema crate 1.15.1 byte for byte.
The generated v2 types use `Nullable[T]` where the draft
distinguishes omitted fields from explicit JSON null, such as message-content
patches. The stable v1 types use it only for the patch fields Rust models with
`MaybeUndefined`: `compaction_update`'s `summary`, `error`, and `_meta`, and
`session_info_update`'s `title` and `updatedAt`. A `Nullable` is sent only
when `Set` is true, for a value and for `null` alike. These fields still
decode strictly where Rust skips a malformed value (#82).

Draft-v2 upserts (`tool_call_update`, `terminal_update`, the message updates,
`session_info_update`, and `compaction_update`) have a generated `ApplyUpdate`
that applies a later update to stored entity state: omitted fields are
unchanged, `null` clears, and values replace. Their top-level `_meta` is a
metadata patch applied by `schema.ApplyMetaUpdate`: `null` clears it, and an
object patches it by top-level key, removing keys set to `null` and replacing
the others whole without merging nested objects. v1 is unchanged: there a
`_meta` object still replaces the stored metadata as a whole.
`acpv2.UpdateProjection` uses these helpers to store messages, thoughts, tool
calls, terminals, compactions, session info, and usage by kind and ID,
applying chunks in receive order, so live delivery and replay share one code
path. `Compactions` lists compactions in first-seen order. Notices need no
client capability in v2; the projection keeps them as live events, outside
the timeline, which `Notices` peeks and `DrainNotices` consumes.
`acpv2.NewNoticeUpdate`, `NewCompactionUpdate`, and
`NewCompactionSummaryChunkUpdate` build the updates. Plans are not projected.

ACP v2 changes prompt semantics: `session/prompt` returns once the agent inserts
the user message into the conversation, while completion is reported later
through an idle `state_update`. The response carries the required `messageId`
of that user message; `Prompt` and `PromptContent` return it, the matching
`user_message` update may arrive before or after the response, and the agent
runtime refuses to answer `session/prompt` when an implementation leaves it
empty. The v1 runner and agent runtime remain under the root package
for stable applications. Draft v2 agents can use
`github.com/BrokkAi/acp-go/v2/agent`; its `Agent` interface covers the baseline
session methods, prompt acceptance, cancellation, and typed permission and
elicitation callbacks.

```sh
go run ./examples/minimal-agent-v2
```

`github.com/BrokkAi/acp-go/v2` also provides typed host adapters with
`HandlePermissions`, `HandleElicitation`, and `HandleClientHost`; install them
before `session/new` because updates and interactive requests can arrive before
setup responses. `github.com/BrokkAi/acp-go/v2/runner` mirrors the reference
SDK's one-shot client: it ignores queued updates until the matching session
reports an active state (`running`, `requires_action`, or an unknown state),
projects subsequent agent-message chunks and patch snapshots, completes at the
next `idle` update whatever its stop reason, and cancels permission requests
by default. Repeated idle snapshots are not new completions. Work that ends
with the `error` stop reason returns the projected text together with a
`*acpv2.WorkError` carrying the reported JSON-RPC error (`WorkResult.Err` gives
the same for `ActiveWork`); custom and unknown reasons are kept as received.
Agents can build idle updates with `acpv2.NewIdleStateUpdate` and
`acpv2.NewErrorIdleStateUpdate`. Run the reference-shaped example with:

```sh
go run ./examples/v2-one-shot-client \
  -command 'go run ./examples/minimal-agent-v2' \
  -prompt 'Say hello using draft ACP v2.'
```

MCP-over-ACP remains an explicit draft opt-in through
`github.com/BrokkAi/acp-go/v2/mcp` (draft v2) and
`github.com/BrokkAi/acp-go/mcp` (v1), matching the Rust SDK's separately gated
`unstable_mcp_over_acp` feature; the v1 package is what carries the native
`acp` server transport that the stable v1 schema does not model. Both packages
carry the request-scoped `mcp/message` mover: `MessageClient` drives an
agent-to-provider request and routes its request-scoped notifications, and
`MessageRouter` serves registered server IDs on the provider side, keeping
inner MCP errors in the response carrier. On top of that mover, both packages
share a standard-library MCP server framework: a `Service` per operation with
a `RequestContext` (server and request IDs, validated metadata, cancellation,
scoped notifications), a typed `ToolRegistry` with caller-supplied JSON
Schemas and allow/deny lists, `NewToolServer` with the reference SDK's
`tools/call` error rules, standalone serving with `Server.Serve`, and automatic
attachment: `NewAttachedSession` and its load/resume/fork siblings generate a
unique `serverId`, register before the request, append the declaration, and
release the registration on failure or `Attachment.Close`, while
`Attachment.HandleSessionSetup` attaches a server to every session a proxy
forwards. See [docs/unstable-facades.md](docs/unstable-facades.md). Generated
protocol versions and error codes use the schema's exact integer widths; in
particular, `ProtocolVersion` is a `uint16`, so strings and values above 65535
fail to decode.

`github.com/BrokkAi/acp-go/agentrouter` serves one endpoint with explicit v1 and
v2 implementations. It selects the highest configured implementation compatible
with the first initialize request, canonicalizes only that initialize frame, and
does not convert subsequent traffic.

`github.com/BrokkAi/acp-go/clientrouter` owns the transport needed for explicit
client-side selection. It starts the highest configured implementation and
follows the Rust fallback rules: reuse an existing v1-negotiated agent
connection only for losslessly identical initialization, otherwise reconnect
with fresh factories, and never turn a v2 rejection into a silent v1 retry.
`github.com/BrokkAi/acp-go/proxyrouter` requires an exact configured
`_proxy/initialize` version and hands the complete initial frame to the selected
implementation without downgrading or cross-version conversion.

Draft-v2 sessions can use `acpv2.NewSessionHandle` for Rust-shaped prompt,
configuration, cancellation, and close commands. `acpv2.SessionTracker` owns
connection-scoped update projections and active-work state; install it before
session setup. Its per-session `Projection` sees every update, including
replay. Connect with `acp.WithRequestObserver(tracker.TrackerRequests(nil))` so that
permission requests' tool calls are applied in receive order with the updates
around them. `SessionTracker.ResumeSessionFromStart` resets the session's
projection first, so a full replay rebuilds it, and closing a session through
its handle drops it. `acpv2.CancellablePermissions` resolves pending
permission requests as cancelled when active work is cancelled.
`ResumeSessionFromStart` requires handlers to be installed before the resume
request and returns only after replay updates have been applied in wire order.

## Process runner

`github.com/BrokkAi/acp-go/runner` adds a process lifecycle, confined client file
operations, terminal callbacks, streaming slog output and private JSONL transcripts.
`Runner.Execute(ctx, prompt)` returns the complete agent text; applications own
receipt parsing and workflow policy. Set `Config.AutoApprove` explicitly to allow
permission requests for unattended operation. It defaults to false. This is not
an OS sandbox: agents and terminal commands inherit the caller's permissions.
`SetupError` distinguishes failures before a prompt from failures during work,
and its `Phase` field names the step that failed (`runner.PhaseSelectModel`,
`runner.PhaseSelectEffort`, and so on). Some selection failures are typed for
`errors.As`: `*acp.UnknownSelectionError` (the value is not offered;
`Available` lists the advertised values), `*acp.UnsupportedSelectionError` (the
agent advertises no such selector), and `*acp.RPCError` (the agent returned a
JSON-RPC error). Other failures stay untyped, including an agent that does not
confirm the selection, malformed selector options, transport and context
errors, and the legacy `unknown session mode` error, so a failure that matches
none of these is not necessarily an agent rejection.

Both runners (`runner` and `v2/runner`) manage the agent process as the Rust
SDK's `AcpAgent` does. After a successful run they close the agent's stdin and
give it one second to exit on its own. On Unix, an agent still running then gets
`SIGTERM` to its whole process group, and `SIGKILL` two seconds later; on
Windows its process tree is killed. A failed or cancelled run kills the process
group at once. Whatever remains of the process group is killed once the agent
exits, so wrapper launchers such as `npx` or `uvx` leave no orphans. If the
agent exits while the run is still in progress, the runner kills its process
group and ends the connection within a second, even if a descendant still holds
stdout open. An agent that exits with a failure status on its own, during the
run or after its stdin closes, is reported as `*runner.AgentExitError` with the
exit status and the stderr tail. A successful run still returns its result
alongside that error, and a setup failure keeps its `SetupError` phase.

Terminal-type authentication methods run outside the protocol connection:
advertise them with `acp.TerminalAuthCapabilities()`, resolve the advertised
method with `runner.TerminalAuthMethod`, run the configured agent program
interactively with `runner.RunTerminalAuth` (the method's `args` are appended and
its `env` overrides the configured environment), then reconnect and initialize
again before `Runner.Execute`. Never pass a terminal method ID to
`Connection.Authenticate`.

## Trace viewer

`github.com/BrokkAi/acp-go/traceviewer` and `cmd/acp-trace-viewer` render a JSONL
transcript (the files `clienthost` writes) as an ordered sequence diagram over
`net/http`, with an embedded page and no external assets:

```sh
go run ./cmd/acp-trace-viewer -addr 127.0.0.1:8787 session.jsonl
```

`FileSource` re-reads the transcript on every poll, so a live session updates in
place, and `Memory` serves events pushed by a host. This is a developer tool, not
a transport: the SDK still speaks stdio by default.

## Draft HTTP transport

`github.com/BrokkAi/acp-go/transport/http` is an explicit, opt-in binding for the
draft Streamable HTTP transport: `POST` with `application/json` for frames,
`Acp-Connection-Id` and `Acp-Session-Id` headers, and a `text/event-stream`
`GET` for server-to-client messages. `Dial` returns a normal `acp.Connection`, so
callers drive it with the usual methods, and any `agent.Runtime` can be served
with `acphttp.Server`. Stdio stays the default and the transport never falls
back between bindings; see [docs/http-transport.md](docs/http-transport.md) for
the binding and its draft limits (no WebSocket, CORS, or per-session streams
yet). A JSON array `POST` body gets `400` with the JSON-RPC batch rejection.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and our
[Code of Conduct](CODE_OF_CONDUCT.md). Report vulnerabilities privately using
[SECURITY.md](SECURITY.md).

## License

Licensed under [Apache-2.0](LICENSE). See [NOTICE](NOTICE) for project
attribution and [licenses/README.md](licenses/README.md) for dependency terms,
third-party notices, and the license review process.
