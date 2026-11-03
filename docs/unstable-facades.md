# Unstable typed facades

ACP's optional features live in the generated `schema/unstable` and
`schema/v2/unstable` packages, which already expose every request, response,
and notification type from Rust schema crate 1.15.1's unstable artifacts (the
crate the reference Rust SDK `agent-client-protocol` 5.0.0 pins).
`github.com/BrokkAi/acp-go/unstable` (v1) and
`github.com/BrokkAi/acp-go/v2/unstable` (draft v2) add narrow typed facades on
top where a capability check or a composite invariant earns its keep, and never
re-export them through the stable `acp` or `v2` facades.

Next Edit Suggestions (`nes/*`, `document/did*`, and their capabilities) were
removed from the upstream unstable schema in crate 1.15, so acp-go no longer
generates or wraps them.

One unstable surface in the 1.15.1 pin is generated but has no facade yet:
proxy chains (`proxy/initialize`, `proxy/successor`, registered with
`SideProxy`; #69).

## Why a separate import

The upstream release marks this surface unstable and may change or remove it.
Keeping it in its own package means an application opts in explicitly, and a
future schema bump that changes an unstable method cannot break the stable API.
Capability gating reads the unstable initialize response, because the stable
schema does not publish the unstable capability fields:

```go
var initialization unstable.InitializeResponse
json.Unmarshal(encodedInitializeResult, &initialization)
providers, err := unstable.ListProviders(ctx, connection, initialization)
```

The draft-v2 facades read `schema/v2/unstable.InitializeResponse`.
`v2.Connection.Initialize` decodes into the stable `v2.Initialization`, which
drops the unstable capabilities. Initialize with
`v2.Connection.InitializeExtended` instead: it runs the same validation and
records the same connection state as `InitializeWithInfo`, and also decodes the
result into the unstable type:

```go
var initialization v2unstableschema.InitializeResponse
_, err := connection.InitializeExtended(ctx, capabilities, info, &initialization)
providers, err := v2unstable.ListProviders(ctx, connection, initialization)
```

Do not send `initialize` through `Connection.Call` to get the raw result: that
bypasses the v2 initialize validation (the selected protocol version and the
agent's name and version) and leaves the connection uninitialized, so
`NewSession` and the other stable facades that read the recorded
initialization refuse to run.

## Implemented here

| Facade | Gate | Notes |
|---|---|---|
| `ListProviders`, `SetProvider`, `DisableProvider` | `agentCapabilities.providers` | Requires a non-blank provider ID before writing. |
| `ForkSession` | `sessionCapabilities.fork` | Requires a session ID and an absolute `cwd`; additional directories need `sessionCapabilities.additionalDirectories`, which the protocol requires before a client sends them, and must be absolute; rejects inline MCP servers so MCP stays behind its own import; checks `systemPrompt` (see [Client-provided system prompts](#client-provided-system-prompts)); requires a non-blank forked session ID in the response. |
| `NewSession`, `LoadSession`, `ResumeSession` | — / `loadSession` / `sessionCapabilities.resume` | The unstable lifecycle requests, which can carry a `systemPrompt` (the reference SDK's `_from` helpers). Same path, MCP, and `systemPrompt` checks as `ForkSession`; load and resume need a session ID; `mcpServers` goes out as `[]` where v1 requires it; session/new needs a non-blank session ID in the response. |
| `MessageClient` | — | Consumer side of the request-scoped `mcp/message` binding: requires a server ID, request ID, and inner MCP method, rejects duplicate active IDs, routes request-scoped notifications to the owning call, and cancels through the outer ACP request. Implemented for v1 and draft v2. |
| `MessageRouter` | — | Provider side of the request-scoped binding: server-ID registry and dispatch, request-scoped notifications through `MessageRequest.Notify`, `Unregister`/`UnregisterAndWait` cancellation, an optional `SetMaxActive` limit, and the binding's outer error codes (`-32602`, `-32800`, `-33000`, `-33001`, `-33002`). Implemented for v1 and draft v2. |
| `Server`, `Service`, `ToolRegistry`, `NewToolServer`, `NewTool` | — | Standard-library MCP server framework and typed tool registry, shared by `mcp` and `v2/mcp` (see below). |
| `MessageRouter.Attach`, `NewAttachedSession`, `ResumeAttachedSession`, `ForkAttachedSession` (+ v1 `LoadAttachedSession`) | `mcpCapabilities.acp` (v1), `session.mcp.acp` (v2) | Generates a unique `serverId`, registers before writing, appends the `acp` declaration, and releases the registration when validation or the request fails. v2 rejects a name the request already declares with `-32602` before writing. |
| `Attachment.HandleSessionSetup`, `Attachment.AppendDeclarations` | — | Connection-scoped (global) attachment for proxies: appends the declaration to the raw params of every session setup request, so unknown fields keep their values (the encoding is semantically equal; members may be re-ordered). v2 rejects name collisions with `-32602` and does not forward the request. |
| `ArchiveSession`, `UnarchiveSession` | `sessionCapabilities.archive` | Require a non-blank session ID. An unknown session answers `-32002` (`acp.IsResourceNotFound`). |
| `ListSessions` | `sessionCapabilities.list`; the `archived` filter also needs `sessionCapabilities.archive` | The unstable `session/list` request, whose `archived` filter (`exclude`, `include`, `only`, or any other value, sent unchanged) the stable `ListSessions` cannot carry. `cwd` must be absolute. `SessionInfo.Archived` reports the archive state. |
| `SessionStatus` | `sessionCapabilities.status` | Requires a non-blank session ID. Unknown status values are preserved; treat anything other than `open` as `saved`. A response without `status` (omitted or `null`) is an error. An unknown session answers `-32002` with the session ID as data. |
| `SetSessionTitle` | `sessionCapabilities.title` | Requires a non-blank session ID and title. The response carries the title the agent stored, which may be normalized; a response without `title` (omitted or `null`) is an error. |
| `AuthStatus` | `agentCapabilities.auth.status` | The unstable `auth/status` query. A response without `state` (omitted or `null`) is an error; unknown states are preserved, and clients treat them as `unknown`. See [Auth status](#auth-status). |
| `AuthStatusUpdates` | — | Client-side adapter for the agent's `auth/status_update` notification, composed with the connection's other notification handlers. |
| `AuthStatusHandler`, `NotifyAuthStatusUpdate` | `agentCapabilities.auth.status` | Agent side: `Handle` serves `auth/status`, `Advertise` announces the capability, and `NotifyAuthStatusUpdate` pushes `auth/status_update`. |
| `Install`, `Handle`, `Advertise` | — | Agent-side dispatch: `Handle` routes providers/*, session/fork, session/archive, session/unarchive, session/status, session/set_title, and auth/status to optional `ProviderHandler`/`ForkHandler`/`ArchiveHandler`/`StatusHandler`/`TitleHandler`/`AuthStatusHandler` implementations and delegates everything else to the next handler; `Advertise` announces the implemented capabilities except `archive` and `systemPrompt`; `Install` sets both on a runtime together with the runtime overrides, such as `session/list` with the archive filter and `session/new`, `session/load`, and `session/resume` with the system prompt, and is the only way to announce `archive` and `systemPrompt`. See [Agent-side dispatch](#agent-side-dispatch). |
| `ValidateSessionSystemPrompt`, `SystemPromptHandler` (+ v1 `SystemPromptLoader`, `SystemPromptResumer`) | `sessionCapabilities.systemPrompt` (v1), `session.systemPrompt` (v2) | Client and agent sides of client-provided system prompts, v1 and draft v2. See [Client-provided system prompts](#client-provided-system-prompts). |
| `v2/unstable.ListProviders`, `SetProvider`, `DisableProvider` | `capabilities.providers` | Draft-v2 counterparts of the v1 provider facades, with the same non-blank provider ID check. |
| `v2/unstable.ForkSession` | `capabilities.session.fork` | Counterpart of the reference SDK's `v2::ForkSessionBuilder`. Requires a session ID and an absolute `cwd`; additional directories need `capabilities.session.additionalDirectories`, as on the stable v2 `session/new`, and must be absolute. Rejects inline MCP servers (use `v2/mcp.ForkSession`), checks `systemPrompt`, and requires a non-blank forked session ID in the response. |
| `v2/unstable.NewSession`, `ResumeSession` | `capabilities.session` | The draft-v2 lifecycle requests that can carry a `systemPrompt`, with the same path, MCP, and `systemPrompt` checks; resume needs a session ID, and session/new needs a non-blank session ID in the response. |
| `v2/mcp.NewSession`, `ResumeSession`, `ForkSession` | `capabilities.session` (fork: `.fork`) | The same lifecycle checks, plus the `v2/mcp` transport checks for each server: stdio, HTTP, and the native `acp` transport each need their `capabilities.session.mcp` entry. All three read the unstable initialize response and send the unstable requests, so `NewSessionOptions.SystemPrompt` and the requests' `SystemPrompt` combine with MCP servers, including attached ones. |
| `mcp.NewSession`, `LoadSession`, `ResumeSession`, `ForkSession` | as their `unstable` counterparts | v1 MCP session helpers with the unstable MCP server transports; `NewSessionOptions.SystemPrompt` and the requests' `SystemPrompt` get the same checks. The attached-session helpers send through them. |
| `v2/unstable.ArchiveSession`, `UnarchiveSession`, `ListSessions`, `SetSessionTitle` | `capabilities.session.archive` / `.title`; `ListSessions` needs `capabilities.session`, plus `archive` for the filter | Draft-v2 counterparts of the v1 archive, list, and title facades, with the same checks. An unknown session answers `-32002` with `SessionNotFoundErrorData`, which `acpv2.SessionNotFoundData` decodes. |
| `v2/unstable.SessionStatus` | `capabilities.session.status` | As in v1, plus the optional foreground `State` snapshot. `State` is nil when the agent omits it, sends `null`, or sends a malformed value; nil is not idle, and an idle snapshot is not a new completion. |
| `v2/unstable.AuthStatus`, `AuthStatusUpdates`, `AuthStatusHandler`, `NotifyAuthStatusUpdate` | `capabilities.auth.status` | Draft-v2 counterparts of the v1 auth-status surface, with the same checks. |
| `v2/unstable.Install`, `Handle`, `Advertise` | — | The draft-v2 counterparts of `Install`, `Handle`, and `Advertise`. |
| `Projection` | — | Client-side: folds plan operations and end-turn usage into ordered per-session state, hands notices and compactions to the stable `acp.SessionProjection` (see below), and subagent updates to its `SubagentTree` (`Projection.Subagents`). `Projection.Notifications` adapts `session/update` notifications. |
| `SubagentsClientCapabilities`, `Initialize` | — | v1 client: the `clientCapabilities.subagents` value, and initialize with the unstable client capabilities, validated as `acp.Connection.InitializeWithInfo` validates. See [Subagent sessions](#subagent-sessions). |
| `SubagentTree` (v1 and draft v2) | `capabilities.cancel` on the child's association, for `Cancel` | Client-side parent/child tree, session-directed messages, the restricted-child allowlist (`CheckClientMethod`), single-child `Cancel`, and `Restore`, `Closed`, and `Disconnected` invalidation. |
| `NotifySubagentUpdate`, `NotifySubagentState`, `NotifySessionMessage`, `NotifySessionMessageChunk` (v1 and draft v2) | v1: `clientCapabilities.subagents`, checked by the agent runtime's `Client.Notify` | Agent side, with the `*Notification` constructors, the work-state snapshots, and `Value`/`Null` patch fields. |

## Agent-side dispatch

`Handle(next, handler)` (v1 `unstable` and draft-v2 `v2/unstable` alike)
serves `providers/list`, `providers/set`, `providers/disable`,
`session/fork`, `session/archive`, `session/unarchive`, `session/status`,
`session/set_title`, and `auth/status` from whichever of `ProviderHandler`,
`ForkHandler`, `ArchiveHandler`, `StatusHandler`, `TitleHandler`, and
`AuthStatusHandler` the handler implements. As in the reference SDK's handler chain, every other
request, including one of these methods the handler does not implement, goes
to `next`, and with no `next` answers `-32601` (`Method not found`, with the
method name as data). Params that do not decode answer `-32602`
(`Invalid params`, with data `{"error", "json", "phase": "deserialization"}`
echoing the received params, or `null` when absent) without reaching the
handler. The params decode as the reference SDK's schema crate decodes them,
array params and malformed members included, and absent params decode as
`null` for every routed method, as the reference SDK's JSON-RPC layer hands
them over: `providers/list`, `auth/status`, and the archive-aware
`session/list` read them as the default request, and a method with required
fields answers them as it answers `null`. A fork response with a blank session ID answers `-32603`
(`Internal error`, with the detail as string data) instead of reaching the
wire. Status and title responses are sent as the handler returns them, as the
reference SDK sends them.
A v1 handler reports an unknown session with `acp.SessionNotFoundError(id)`,
the reference SDK's v1 `session_not_found`: `-32002` (`Resource not found`)
with data `{"sessionId": id}`. A draft-v2 handler uses
`acpv2.SessionNotFoundError(id)`, whose data is the v2 schema's
`SessionNotFoundErrorData`; `acpv2.SessionNotFoundData(err)` decodes it on the
client.

Install everything on the agent runtime in one call, so that the runtime's
initialization gate, version negotiation, repeat-initialize handling, and
baseline methods sit in front of every unstable method, as in the reference
SDK:

```go
runtime := agent.New(implementation) // agent or v2/agent
if err := unstable.Install(runtime, implementation); err != nil { // or v2/unstable
	return err
}
```

`Install(runtime, handler, customize...)` sets three runtime fields together:

- `Runtime.Extensions` becomes `Handle`, chained in front of any `Extensions`
  already set. It serves the requests the runtime does not own, only after
  initialization completes; before that they get the runtime's `-32600`
  (`Invalid request`, data "ACP initialization must complete before
  `<method>` can be used").
- `Runtime.InitializeResult` replaces the stable initialize response on the
  wire. It carries it over to the unstable `InitializeResponse`, adds
  `providers`, `auth.status`, and the `fork`, `archive`, `status`, and
  `title` session capabilities for the implemented handlers, and applies the customize
  functions; the runtime validates the stable view of the value it sends. The
  session capabilities are only added to session capabilities the agent
  already advertises in v2, and v1 `archive` only alongside the agent's own
  `sessionCapabilities.list`, which the protocol requires with it. Whether
  `archive` is added is decided from the `Agent`'s own response before the
  customize functions run: one that adds `sessionCapabilities.list` (or, in
  v2, the baseline `capabilities.session`) does not gain `archive`, and one
  that removes `archive` leaves the `session/list` override installed.
- `Runtime.Overrides` gains the stable methods whose unstable form the
  handler serves: `session/list` for an `ArchiveHandler`, and the lifecycle
  requests that carry a client-provided system prompt for a
  `SystemPromptHandler` (see
  [Client-provided system prompts](#client-provided-system-prompts)).
- `systemPrompt` is announced (as `{}`, append only) only for a
  `SystemPromptHandler` that receives the prompt on every lifecycle request
  the agent advertises. It is decided from the `Agent`'s own response, like
  `archive`, but checked again after the customize functions: a customize
  function may add `mode.override`, while a `systemPrompt` some advertised
  lifecycle request would drop fails `initialize` instead of being sent.

`Install` fails, changing nothing, for a nil runtime, a runtime whose
`InitializeResult` is already set (including by an earlier `Install`), or one
whose `Overrides` already serves a method it would override. `Handle` and
`Advertise` remain for wiring by hand, but `Advertise` never announces
`archive` or `systemPrompt`: an agent that does must also serve the
`archived` filter or the prompt, which only `Install` wires up. The draft-v2
runtime does not import the unstable schema; the v1 runtime reads only the
initialize request with it, for the subagents capability (see
[Subagent sessions](#subagent-sessions)).

### Runtime overrides

`Runtime.Overrides` (`agent` and `v2/agent`) maps a method name to an
`acp.Handler` that serves it in place of the runtime's own dispatch to the
`Agent`, with the raw params, so it can read and answer fields the stable
schema drops. An override runs after the initialization gate and the
method's capability check: an overridden `session/list` still needs v1
`sessionCapabilities.list` or the v2 baseline `capabilities.session`, and
still answers `-32601` without it. It also takes precedence over
`Extensions`. In v2 an override for `auth/login`, `auth/logout`, or
`session/delete` stands in for the `Agent` interface the matching capability
otherwise requires at initialize. `Serve` refuses an override for
`initialize` or `session/prompt`, which the runtime validates and tracks
itself, for a notification such as `session/cancel` or `$/cancel_request`,
which never reaches a request handler, and a nil handler, before reading from
the connection.

An override or `Extensions` handler takes no `Client` argument; it reaches the
connection's client through `agent.ClientFromContext(ctx)` (or
`v2/agent.ClientFromContext`), as the reference SDK hands every request
handler its connection. It is the same client the stable dispatch passes to
`NewSession`, `LoadSession`, and `ResumeSession`, so a `session/load` override
can stream the replay as `session/update` notifications before it answers,
and the client receives them in order ahead of the response. Each `Install`
route decodes the raw params into the unstable request and passes the
context, so the handler method reads the client from it; serving the unstable
form of another stable request takes one more route.

`session/list` is a baseline method the runtime serves through the stable
schema, which drops the `archived` filter and `SessionInfo.archived`. The
archive capability covers both, so `ArchiveHandler` also serves
`ListSessionsWithArchive` with the unstable request and response, and
`Install` routes `session/list` to it through `Runtime.Overrides`. The handler
must honor `exclude` (also the meaning of an absent filter), `include`, and
`only`, and answer `-32602` for a value it does not support rather than fall
back to `exclude`. A listed session without `archived` answers `-32603`. A
handler that is not an `ArchiveHandler` keeps the stable dispatch. A client
reads a malformed `archived` on a listed session as not reported (nil), as
the reference SDK's `DefaultOnError` does, rather than failing the page.

Further unstable methods join the same dispatch as additional handler
interfaces, as auth status does (see [Auth status](#auth-status)).
`auth/status` is not a method the runtime owns, so it needs no override; the
`auth/status_update` notification it pairs with goes from agent to client and
never reaches an override or `Handle`, which only see requests.

## Auth status

Auth status follows the
[RFD](https://agentclientprotocol.com/rfds/get-auth-state) and the reference
SDK's `unstable_auth_status` feature. It is unstable and lives only in the
unstable schemas. The agent advertises it as `agentCapabilities.auth.status`
(v1) or `capabilities.auth.status` (draft v2); `{}` means the agent answers
`auth/status` and may send `auth/status_update`.

A client checks the capability before it asks; `AuthStatus` fails before
writing without it:

```go
status, err := unstable.AuthStatus(ctx, connection, initialization, unstableschema.AuthStatusRequest{})
switch status.State {
case unstableschema.AuthStateAuthenticated, unstableschema.AuthStateNotRequired:
	// Create sessions.
case unstableschema.AuthStateUnauthenticated:
	// Offer a sign-in, such as authenticate (v1) or auth/login (v2).
default:
	// unknown, and any value the client does not recognize.
}
```

`state` is the only field to act on. It is required and strict: a response
without it (omitted or `null`) or with a non-string value is an error, and a
value other than `authenticated`, `unauthenticated`, `not_required`, and
`unknown` is preserved as it arrived and means `unknown`. `accounts` is
display information only, so it decodes as the reference SDK decodes it: a
malformed value reads as no accounts, an invalid entry (one without `label`,
for example) is skipped, and a malformed optional account field reads as not
reported. Do not use accounts to decide whether to authenticate, as stable
account identifiers, or in logs and telemetry.

Updates are full snapshots that replace the previous status. `AuthStatusUpdates`
adapts them, and composes with the connection's other notification handlers,
such as the reference client host, without changing their interfaces:

```go
notifications := acp.CombineNotifications(host.Notification, unstable.AuthStatusUpdates(func(update unstableschema.AuthStatusUpdateNotification) error {
	showAuthStatus(update.State, update.Accounts)
	return nil
}))
connection := acp.Connect(stdout, stdin, host.Request, notifications)
```

On the agent side, `AuthStatusHandler.AuthStatus` serves the request through
`Handle` (and `Install`), and `Advertise` announces the capability for it.
The query must have no side effects and must not answer `auth_required`
(`-32000`); a check that would block should answer `unknown` and send an
update once it completes. Absent, `null`, and positional params decode as in
the reference SDK (see [Agent-side dispatch](#agent-side-dispatch)); params
that do not decode answer `-32602`. The response is sent as the handler
returns it, as the reference SDK sends it.

`NotifyAuthStatusUpdate(ctx, client, notification)` sends
`auth/status_update`. The RFD gates it on no client capability, only on the
agent's own `auth.status`: inside a request the runtime dispatched (an
`Authenticate`, `Logout`, `auth/login`, or `auth/logout` that changed the
status, for example), it reads that connection's initialize response and
fails before writing when `auth.status` was not announced; to keep that
check for an update sent after the request ends (after answering `unknown`,
for example), send with `context.WithoutCancel` of the request's context.
Outside a request
it has no initialize response to check, so an agent that sends later, after a
sign-in in another terminal, for example, must only do so on connections it
advertised `auth.status` on. Send only when the status differs from the last
one sent on the connection. The notification is connection-scoped, never
per session.

## Client-provided system prompts

Client-provided system prompts follow the
[RFD](https://agentclientprotocol.com/rfds/client-system-prompt) and the
reference SDK's `unstable_session_system_prompt` feature. They are unstable
(Preview) and live only in the unstable schemas.

The agent advertises support at
`agentCapabilities.sessionCapabilities.systemPrompt` (v1) or
`capabilities.session.systemPrompt` (draft v2). `{}` means text content in
`append` mode, and `mode.override` is the nested opt-in for `override`.
Clients send the prompt as the `systemPrompt` member of the lifecycle
requests: v1 `session/new`, `session/load`, `session/resume`, and
`session/fork`; v2 `session/new`, `session/resume`, and `session/fork`.

```go
prompt := unstable.NewTextSystemPrompt("Review changes without modifying files.")
session, err := unstable.NewSession(ctx, connection, initialization, unstableschema.NewSessionRequest{
	Cwd:          directory,
	SystemPrompt: &prompt,
})
```

`NewSystemPrompt` and `NewTextSystemPrompt` default to `Append`, as Rust's
`SystemPrompt::new` does; set `Mode` to `Override` to replace the agent's
default instructions. The type always encodes both members, a nil `Content`
as `null`; the facades below refuse empty content, so a prompt they send
carries at least one text block.

Before any lifecycle request reaches the wire, every facade (`unstable`,
`v2/unstable`, `mcp`, and `v2/mcp`) rejects:

- a `systemPrompt` when the agent did not advertise the capability;
- `override` without `mode.override`;
- a missing mode, or any mode other than `append` and `override`, including
  `_`-prefixed extension modes. The type still decodes and forwards unknown
  modes;
- empty content, or any content block that is not text.

The reference SDK does not check these in its builders. It leaves the
capability negotiation to the caller. acp-go checks them because its facades
gate every capability before writing.

Clients should send the same prompt on load, resume, and fork that they used
when the session was created. Omitting it leaves the result to the agent: it
may keep a stored prompt or fall back to its default. Only send content you
trust at system-instruction precedence. `override` replaces the agent's own
default instructions, not its tools, permissions, MCP servers, or workspace
instructions.

On the agent side, an agent that advertises the capability must apply a
supplied prompt or reject the lifecycle request. It must not create, load,
resume, or fork the session without it. The runtimes decode session/new,
session/load, and session/resume into the stable types, which do not model
`systemPrompt`, so `Install` serves those methods from the unstable requests
through [runtime overrides](#runtime-overrides), in place of the `Agent`'s
own methods:

| Handler | v1 methods | Draft-v2 methods |
| --- | --- | --- |
| `SystemPromptHandler` | `NewSessionWithSystemPrompt` (session/new) | `NewSessionWithSystemPrompt` and `ResumeSessionWithSystemPrompt` (session/new and session/resume, both required in v2) |
| `SystemPromptLoader` (v1) | `LoadSessionWithSystemPrompt` (session/load) | — |
| `SystemPromptResumer` (v1) | `ResumeSessionWithSystemPrompt` (session/resume) | — |
| `ForkHandler` | `ForkSession` (session/fork, through `Handle`) | `ForkSession` |

`Install` announces the capability only when the handler receives the prompt
on every lifecycle request the agent advertises: in v1, `loadSession` needs a
`SystemPromptLoader`, `sessionCapabilities.resume` a `SystemPromptResumer`,
and `fork` a `ForkHandler`; in v2, `fork` needs a `ForkHandler`. Otherwise
some lifecycle request would drop the prompt, so nothing is announced, and a
customize function that announces it anyway fails `initialize`. It announces
`{}` (append only); add `mode.override` with a customize function:

```go
err := unstable.Install(runtime, implementation, func(response *unstableschema.InitializeResponse) error {
	// v2: response.Capabilities and Capabilities.Session
	if capabilities := response.AgentCapabilities; capabilities != nil && capabilities.SessionCapabilities != nil && capabilities.SessionCapabilities.SystemPrompt != nil {
		capabilities.SessionCapabilities.SystemPrompt.Mode = &unstableschema.SessionSystemPromptModeCapabilities{
			Override: &unstableschema.SessionSystemPromptOverrideCapabilities{},
		}
	}
	return nil
})

func (a *myAgent) NewSessionWithSystemPrompt(ctx context.Context, request unstableschema.NewSessionRequest) (unstableschema.NewSessionResponse, error) {
	client, _ := agent.ClientFromContext(ctx)
	// Create the session with request.SystemPrompt (nil when the client sent
	// none), which the route already checked.
}
```

The customize function adds `mode.override` only when `Install` announced the
capability; setting `systemPrompt` where `Install` did not fails `initialize`.

The handlers do not check the prompt themselves. Before one runs, its route
answers `-32602` without reaching it in two cases:

- a `systemPrompt` the reference SDK does not deserialize: a missing or
  `null` `mode` or `content`, a text block whose `text` is missing or `null`,
  or any other value that does not decode. These answer in the reference
  SDK's deserialization shape (`{error, json, phase: "deserialization"}`),
  as `Handle` answers malformed params, with the reference SDK's error text
  (``missing field `mode` ``, for example) for the values `encoding/json` alone
  would accept. As in the reference SDK, the first fault in document order
  within `systemPrompt` is reported, and missing members only after the whole
  object. This check runs before the params are decoded, so a prompt fault
  wins over a bad sibling member of the params even when that member comes
  first, where the reference SDK would report the sibling;
- a prompt the capability the runtime sent in this connection's initialize
  response does not cover: an unadvertised prompt, `override` without
  `mode.override`, an unknown mode, empty content, or non-text content. These
  answer in the reference SDK's `Error::invalid_params().data(reason)` shape:
  message `Invalid params`, with the reason as string data.

The capability checked is the one actually sent, so a handler cannot accept a
prompt that was never advertised, for example on a session/new whose
`Install` declined to announce `systemPrompt` because no `SystemPromptLoader`
serves the advertised `loadSession`. The handlers reach the connection's
client through `agent.ClientFromContext` (`v2/agent.ClientFromContext`), so
`LoadSessionWithSystemPrompt` can replay the conversation with
`session/update` before it answers.

## Subagent sessions

Subagent sessions follow the [RFD](https://agentclientprotocol.com/rfds/subagents)
and the reference SDK's `unstable_subagents` feature, which only enables the
schema types; the tree and the senders here are acp-go's. An agent exposes a
child it delegates work to as its own session: `subagent_update` on the parent
announces and patches the association, the child's own updates use its session
ID, and `session_message` upserts and `session_message_chunk` appends report
messages between sessions.

In v1 a client advertises `clientCapabilities.subagents: {}`, and an agent must
not send any of these updates without it. The stable `acp.Capabilities` cannot
carry the field, so initialize with `unstable.Initialize`, which sends the
unstable client capabilities and validates the response as
`acp.Connection.InitializeWithInfo` does:

```go
capabilities := unstableschema.ClientCapabilities{Subagents: unstable.SubagentsClientCapabilities()}
initialization, err := unstable.Initialize(ctx, connection, capabilities, info)
```

The v1 agent runtime enforces it: its `Client.Notify` refuses the three update
kinds with `-32601` before writing unless the client's initialize request
advertised the capability (an omitted, `null`, or malformed value does not),
whatever the params type, typed or hand-built. It reads the client
capabilities with the unstable schema and applies the other session-update
gates to unstable and hand-built params too: `notice`, compaction, and
boolean config options as for `acp.Update`, and `plan_update` and
`plan_removed`, which need the unstable `clientCapabilities.plan`. Params that
do not decode are refused by their `sessionUpdate` tag, in the named and the
positional form, for every kind the tag alone decides. Draft v2 makes subagents part of
the baseline session model: there is no capability, and the v2 runtime sends
them on any connection.

### Client: the subagent tree

`SubagentTree` (`unstable` and `v2/unstable`) folds the updates in receive
order. `unstable.Projection` carries one (`Projection.Subagents`); in v2,
compose `tree.Notifications()` with the connection's other handlers.

- **Associations.** `subagent_update` is an upsert keyed by the child's session
  ID; the outer session is its immediate parent. `title`, `description`,
  `capabilities`, `state`, `cwd`, `additionalDirectories`, and `_meta` are
  patches: omitted is unchanged, `null` clears, and a value replaces the
  whole field. In v2 a concrete `_meta` patches by top-level key
  (`ApplyMetaUpdate`). `capabilities: {"cancel": null}` disables cancellation.
  The tree refuses, with an error and without applying any of it, an update
  that would reparent a child, make a session its own parent, or create a
  cycle. A relative `cwd` or additional directory is not applied: the rest
  of the update is, and the directory is reported with an error.
- **Two views of a child.** `Subagent.Association` keeps every field as last
  reported, history included, for display. `Subagent.Capabilities` (the
  effective mutation set) and `Subagent.State` (the confirmed current work
  state) drive controls and live activity; nil means none permitted and
  unconfirmed, never idle. In v1 the state comes only from the association;
  in v2 the child's own `state_update` and the mirrored association `state`
  are one logical state, and either replaces it. A v2 session's
  `BackgroundSubagents` lists the `subagent` entries of its current
  `background` snapshot. Error and cancelled idle snapshots, unknown states,
  and unknown stop reasons are kept as received.
- **Directories.** `Directories` resolves a child's effective `cwd` and
  `additionalDirectories`, each inherited independently from the parent when
  unset or cleared; record an ordinary session's own with
  `SetSessionDirectories`. In v1 the result is the child's root set for its
  filesystem and terminal requests and the default terminal `cwd`; in v2 it
  is informational.
- **Messages.** `Messages` and `Message` return a transcript's
  session-directed messages in first-seen order. A `session_message` upsert's
  content replaces the accumulated content (`null` and `[]` clear it), chunks
  append, and sender and recipient come only from upserts, since chunks carry
  none. `LinkedMessage` follows `senderMessageId`/`recipientMessageId` to the
  other view, and reports conflicting references as unlinked.
- **Routing.** Permission, elicitation, filesystem, and terminal requests a
  child sends carry the child's session ID. `Subagent(id)` attributes them to
  that child, with its `Parent` and `Root`; do not handle them as the
  parent's.

### Restricted children

Client-initiated mutations of a child are an allowlist.
`CheckClientMethod(method, sessionID)` refuses every method on a child except
`session/cancel`, while the association's current capabilities carry
`cancel`, and `session/status`, a read-only query. Prompt, load, resume,
close, delete, fork, mode, configuration, title, archive, v2
`tool_call/cancel` and `terminal/cancel`, and any future method are refused.
`Cancel(ctx, connection, childID)` sends `session/cancel` for one child and
fails before writing otherwise. It cancels the child's current work, not its
parent or siblings, and changes no state: the child confirms with
`idle`/`cancelled`. Cancelling any session also cascades, agent-side, to the
descendant work it waits on; the client learns which descendants stopped
from their state reports, and cleans up a descendant's pending permission and
elicitation requests once it reports `idle`/`cancelled`. `Closed(parent)`
records that the client closed an ordinary session, which ends all its
descendants' work: it invalidates their controls and state and returns them,
parents first, so their pending requests can be resolved with the parent's.
History stays.

### Reconnection

Run a parent `session/load` or `session/resume` (v2: `session/resume`)
through `Restore`, so the tree can separate history from current state:

```go
err := tree.Restore(ctx, sessionID, true /* session/load replays */, func(ctx context.Context) error {
	_, err := unstable.LoadSession(ctx, connection, initialization, request)
	return err
})
```

`Restore` first invalidates every known descendant's capabilities and state,
and refuses a child (children are never loaded or resumed directly). With
replay (v1 `session/load`, v2 `replayFrom: {"type": "start"}`), associations
and v2 state updates received before the successful response are history:
they update `Association` but never the effective capabilities or current
state, and the restored transcripts restart empty because the replay sends
them again. Snapshots after the response are current. Without replay,
snapshots during the attempt are current, but `CanCancel` stays false until
the request succeeds. A failed attempt invalidates everything received during
it. The context `Restore` hands `send` marks the response to the first
`session/load` or `session/resume` made with it in wire order, with the
notifications around it, so a snapshot sent right after the response is never
mistaken for history; `send` must use it, and other calls made with it do not
mark the boundary. Restoring again while an attempt is in progress starts a
new attempt and still sends its request; the earlier one then no longer
changes the tree. `Disconnected` records a lost
connection: no child keeps a live state or control.

### Agent: senders

`NotifySubagentUpdate`, `NotifySessionMessage`, and
`NotifySessionMessageChunk` send the three updates through an `agent.Client`
(`v2/agent.Client`), and the `*Notification` functions build them for replay
or hand-wiring. `NotifySubagentState` reports a child's work state: in v1 on
its parent's association, the only v1 path; in v2 as the child's own
`state_update` followed by the same snapshot mirrored on the association.
`RunningState`, `RequiresActionState`, `UnknownState`, `IdleState`, and
`ErrorState` build the snapshots, and `Value` and `Null` the patch fields.
The senders fail before writing for a blank ID and a relative `cwd` or
additional directory; like the reference SDK, they leave the rest of the
RFD's rules for the agent to follow. They do not track announcement order: announce a child before any traffic bearing
its session ID, report directories that differ from the parent's before
traffic that depends on them, and, after a replayed resume, send current
snapshots only after the response.

## Session updates: projection, not request facades

Plan operations and end-turn token usage do not have methods of their own;
they arrive as `session/update` payloads the client has to interpret. A request
facade would be the wrong shape, so they are served by `unstable.Projection`,
which folds updates in wire order and keeps the reference rules:

- `plan` replaces the session's plan entries, `plan_update` replaces the plan
  payload, and `plan_removed` clears it.
- `usage_update` keeps the latest end-turn token usage (used, size, cost).

Session notices and compaction are not unstable features: they are part of
stable ACP v1 and the draft-v2 baseline, and the stable packages serve them.
v1 clients advertise them with `acp.SessionNoticesClientCapabilities` and
`acp.SessionCompactionClientCapabilities` and project them with
`acp.SessionProjection`; draft-v2 clients need no capability and read them from
`acpv2.UpdateProjection`. `unstable.Projection` hands both update kinds to an
`acp.SessionProjection`, so its `Compactions`, `Notices`, and `DrainNotices`
return the stable types (`CompactionState` is an alias of
`acp.CompactionState`). Both schemas are ACP v1, so every update that decodes
from the wire converts; `Projection.Notifications` returns the error for one
that does not instead of dropping it.

MCP-over-ACP for the v1 surface landed as `github.com/BrokkAi/acp-go/mcp`,
mirroring `v2/mcp`: session create, resume, and fork helpers that
accept the unstable server transports, including the native `acp` variant.

## MCP-over-ACP methods: a request-scoped message mover

The native MCP-over-ACP *methods* are still open from #10. The pinned schemas
(`schema-v1.29.1` and `schema-v2.0.0-alpha.13`) model the request-scoped
binding that upstream shipped in
[agentclientprotocol/agent-client-protocol#2223](https://github.com/agentclientprotocol/agent-client-protocol/pull/2223):
`mcp/connect` and `mcp/disconnect` are gone, and each `mcp/message` request
carries a `serverId`, a caller-generated `requestId`, and the inner MCP method
and params. The `mcp` and `v2/mcp` packages serve and drive it as
`MessageClient` and `MessageRouter`, behind those explicit opt-ins.

**Decision:** the binding is a standard-library message mover that carries MCP
requests, results, and notifications and leaves the MCP protocol to the caller.
It does not integrate a third-party Go MCP SDK, for the same dependency-policy
reason that `agent-client-protocol-rmcp` is won't port.

In that revision:

- `mcp/connect` and `mcp/disconnect` are gone. Each agent-to-provider
  `mcp/message` request carries a `serverId`, a caller-generated `requestId`,
  and the inner MCP method and params.
- The response carries exactly one inner MCP `result` or `error`, and inner MCP
  errors never become outer ACP errors.
- The provider (the client or proxy that declared the `acp` server) sends only
  notifications tied to an active request, never requests of its own.
- It targets MCP 2026-07-28 only.

The released schema crate carried the change, and #39 delivered the mover in
`mcp` and `v2/mcp`: an agent-side caller plus a provider-side server-ID
registry and dispatcher, with tests for the result/error split, request IDs,
duplicate active IDs, notification ordering, late-notification rejection, and
cancellation.

## MCP server framework

`mcp` and `v2/mcp` also carry a standard-library MCP server framework that
mirrors Rust `agent-client-protocol` 5.0.0's `mcp_server` module and the tool
semantics of `agent-client-protocol-rmcp`'s `rmcp_3` builder (rmcp 3.4.0),
without an MCP SDK. The framework types are aliases of one internal
implementation, so `mcp.Server` and the v2 `mcp.Server` are the same type:
define a server once and attach it to v1 and v2 sessions.

```go
registry := mcp.NewToolRegistry()
registry.Register(mcp.NewTool("greet", "Greet someone",
	json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
	func(ctx context.Context, in struct{ Name string `json:"name"` }, rc *mcp.RequestContext) (string, error) {
		return "Hello, " + in.Name + "!", nil
	}))
server := mcp.NewToolServer("project-tools", "1.0.0", registry)

router := mcp.NewMessageRouter(connection) // install router.Handle(next) on the connection
session, attachment, err := mcp.NewAttachedSession(ctx, router, initialization, directory, mcp.NewSessionOptions{}, server)
defer attachment.Close(ctx)
```

- **Services.** `Service.Execute(ctx, Request, *RequestContext)` runs one
  operation. `RequestContext` exposes the server ID, the logical request ID,
  the validated `_meta` (protocol version and client capabilities), the outer
  request's cancellation, operation cancellation, and `Notify` for
  operation-scoped notifications. `ctx` is the operation context; its
  `context.Cause` is `ErrRequestCancelled`, `ErrServerUnavailable` (the
  registration was removed), or `ErrConnectionClosed` (input EOF or close).
  Inner MCP errors travel in `Outcome.Error`; a returned Go error, including a
  bare `context.Canceled` when the operation was not cancelled, is a binding
  failure (`-33002`, `MCP backend failure`, the error text as `data`).
- **Binding rules**, with the reference SDK's codes, messages, and data:
  - Requests to an attached server must carry MCP 2026-07-28 `_meta`. A
    missing or malformed field, including a protocol version that is not a
    JSON string, is an inner `-32602` (`Invalid params`, detail in `data`);
    another version is an inner `-32022`; `initialize` is an inner `-32601`.
    `server/discover` results are restricted to `2026-07-28`.
  - A malformed envelope and a duplicate active request ID are outer `-32602`
    `Invalid params` (data: the decode error, or
    `duplicate active MCP requestId`). An unknown server is `-33001`
    `MCP server registration unavailable`; a removed one is `-33001`
    `MCP provider removed`. A `SetMaxActive` limit is `-33000`.
  - Outer cancellation, registration removal, and input EOF cancel the
    operation; work already cancelled when it is admitted never starts. The
    router waits for the service to return, holds the request ID until then,
    discards a result that loses the race with cancellation, refuses late
    notifications, and sends no response once the connection's input has
    closed. Removal never waits on a stalled transport: notifications are
    queued under the operation's gate and the queueing stops on cancellation,
    so `Attachment.Close` and `UnregisterAndWait` return when the operations
    do, or when their `ctx` ends.
- **Tools.** Schemas are caller-supplied JSON Schema; no schema is generated.
  `tools/list` reports enabled tools (with `ttlMs: 0` and
  `cacheScope: "private"` on 2026-07-28 requests). In `tools/call`, an unknown
  or disabled tool is an inner `-32602` whose message names the tool, with no
  data, as rmcp sends it;
  params that do not parse (no string `name`, non-object `arguments`) are an
  inner `-32602` `Invalid MCP request`; arguments that fail the input schema or
  do not decode, and errors returned by the tool, are results with
  `isError: true`; a cancelled call is an inner `-32603` `operation cancelled`.
  A result without an `outputSchema` is one text block holding its JSON
  encoding (a string result is sent quoted, as rmcp does); with one, the result
  is also `structuredContent`. Unknown methods are `-32601` with the method as
  the message, and `prompts/list`, `resources/list`,
  `resources/templates/list`, and `completion/complete` return the reference
  handler's empty defaults. `serverInfo` reports the name and version (default
  `0.0.0`). `ToolRegistry` supports allow/deny filtering (`EnableTool`,
  `DisableTool`, `EnableAllTools`, `DisableAllTools`, which return
  `*UnknownToolError` for a typo), instructions, and replacement of a tool by
  name.
- **Argument checks.** The reference SDK only decodes arguments into the tool's
  type; Go decoding accepts missing and unknown fields, so arguments are first
  checked against the input schema. Only the structural keywords that mirror
  decoding are enforced (`type`, `enum`, `const`, `properties`, `required`,
  `additionalProperties`, `items`, `allOf`, `anyOf`, `oneOf`). Any other
  keyword, including `$ref` and string or numeric bounds, never causes a
  rejection, and a `oneOf` with such a branch only requires some branch to
  match.
- **Standalone.** `Server.Serve(ctx, in, out)` serves a direct MCP client over
  newline-delimited JSON-RPC with no ACP session; every request goes to the
  service. A `NewToolServer` service also answers `initialize` and `ping` for
  clients on revisions before 2026-07-28. A client `notifications/cancelled`
  cancels the named request, which then gets no response, as in rmcp; input
  EOF cancels in-flight work, which gets no response either.
- **Low-level mover.** `MessageRouter.Register` with a `MessageService` serves
  requests without MCP metadata validation, as before, but now shares the
  binding rules above: EOF cancellation and suppressed responses, `-33001` for
  removal, the reference error messages, and `-33002` for a returned error.
  Like the reference SDK, the router keeps no record of released server IDs;
  the RFD forbids rebinding one on a connection, so callers must not reuse an
  ID.
