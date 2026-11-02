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

Several unstable surfaces in the 1.15.1 pin are generated but have no facade
yet: client-provided system prompts (#58), subagents (#60), session
archive/status/title (#71), auth status (#72), and proxy chains
(`proxy/initialize`, `proxy/successor`, registered with `SideProxy`; #69).

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
| `ForkSession` | `sessionCapabilities.fork` | Requires a session ID and an absolute `cwd`; additional directories need `sessionCapabilities.additionalDirectories`, which the protocol requires before a client sends them, and must be absolute; rejects inline MCP servers so MCP stays behind its own import; requires a non-blank forked session ID in the response. |
| `MessageClient` | — | Consumer side of the request-scoped `mcp/message` binding: requires a server ID, request ID, and inner MCP method, rejects duplicate active IDs, routes request-scoped notifications to the owning call, and cancels through the outer ACP request. Implemented for v1 and draft v2. |
| `MessageRouter` | — | Provider side of the request-scoped binding: server-ID registry and dispatch, request-scoped notifications through `MessageRequest.Notify`, `Unregister`/`UnregisterAndWait` cancellation, an optional `SetMaxActive` limit, and the binding's outer error codes (`-32602`, `-32800`, `-33000`, `-33001`, `-33002`). Implemented for v1 and draft v2. |
| `Server`, `Service`, `ToolRegistry`, `NewToolServer`, `NewTool` | — | Standard-library MCP server framework and typed tool registry, shared by `mcp` and `v2/mcp` (see below). |
| `MessageRouter.Attach`, `NewAttachedSession`, `ResumeAttachedSession` (+ v1 `LoadAttachedSession`, `ForkAttachedSession`) | `mcpCapabilities.acp` (v1), `session.mcp.acp` (v2) | Generates a unique `serverId`, registers before writing, appends the `acp` declaration, and releases the registration when validation or the request fails. v2 rejects a name the request already declares with `-32602` before writing. |
| `Attachment.HandleSessionSetup`, `Attachment.AppendDeclarations` | — | Connection-scoped (global) attachment for proxies: appends the declaration to the raw params of every session setup request, so unknown fields survive. v2 rejects name collisions with `-32602` and does not forward the request. |
| `LoadSession` (v1 `mcp`) | `agentCapabilities.loadSession` | session/load with the unstable MCP server transports. |
| `Handle`, `Advertise` | — | Agent-side dispatch: routes providers/* and session/fork to optional `ProviderHandler`/`ForkHandler` implementations and delegates everything else to the next handler; `Advertise` announces the implemented capabilities. See [Agent-side dispatch](#agent-side-dispatch). |
| `v2/unstable.ListProviders`, `SetProvider`, `DisableProvider` | `capabilities.providers` | Draft-v2 counterparts of the v1 provider facades, with the same non-blank provider ID check. |
| `v2/unstable.ForkSession` | `capabilities.session.fork` | Counterpart of the reference SDK's `v2::ForkSessionBuilder`. Requires a session ID and an absolute `cwd`; additional directories need `capabilities.session.additionalDirectories`, as on the stable v2 `session/new`, and must be absolute. Rejects inline MCP servers (use `v2/mcp.ForkSession`) and requires a non-blank forked session ID in the response. `systemPrompt` is forwarded unchanged; its capability checks are #58. |
| `v2/mcp.ForkSession` | `capabilities.session.fork` | The same fork checks, plus the `v2/mcp` transport checks for each server: stdio, HTTP, and the native `acp` transport each need their `capabilities.session.mcp` entry. |
| `v2/unstable.Handle`, `Advertise` | — | The draft-v2 counterparts of `Handle` and `Advertise`. |
| `Projection` | — | Client-side: folds optional session updates into ordered per-session state (see below). `Projection.Notifications` adapts `session/update` notifications. |

## Agent-side dispatch

`Handle(next, handler)` (v1 `unstable` and draft-v2 `v2/unstable` alike)
serves `providers/list`, `providers/set`, `providers/disable`, and
`session/fork` from whichever of `ProviderHandler` and `ForkHandler` the
handler implements. As in the reference SDK's handler chain, every other
request, including one of these methods the handler does not implement, goes
to `next`, and with no `next` answers `-32601` (`Method not found`, with the
method name as data). Params that do not decode answer `-32602`
(`Invalid params`, with data `{"error", "json", "phase": "deserialization"}`
echoing the received params, or `null` when absent) without reaching the
handler, and a fork response with a blank session ID answers `-32603`
(`Internal error`, with the detail as string data) instead of reaching the wire.

Install it on the agent runtime, whose initialization gate, version
negotiation, repeat-initialize handling, and baseline methods then sit in front
of every unstable method, as in the reference SDK:

```go
runtime := agent.New(implementation) // agent or v2/agent
runtime.Extensions = unstable.Handle(nil, implementation)
runtime.InitializeResult = unstable.Advertise(implementation)
```

`Runtime.Extensions` serves the requests the runtime does not own, only after
initialization completes; before that they get the runtime's `-32600`
(`Invalid request`, data "ACP initialization must complete before
`<method>` can be used"). `Runtime.InitializeResult` replaces the stable
initialize response on the wire: `Advertise` carries it over to the unstable
`InitializeResponse`, adds `providers` and `fork` for the implemented handlers,
and applies optional customize functions, and the runtime validates the stable
view of the value it sends. Neither runtime imports the unstable schema.

Further unstable methods (session archive/status/title in #71, auth status in
#72) join the same dispatch as additional handler interfaces.

## Session updates: projection, not request facades

Plan operations, compaction, notices, and end-turn token usage do not have
methods of their own; they arrive as `session/update` payloads the client has to
interpret. A request facade would be the wrong shape, so they are served by
`unstable.Projection`, which folds updates in wire order and keeps the reference
rules:

- `plan` replaces the session's plan entries, `plan_update` replaces the plan
  payload, and `plan_removed` clears it.
- `compaction_update` upserts by compaction id and its summary is a complete
  replacement; `compaction_summary_chunk` appends to that compaction. Compactions
  keep first-seen order.
- `notice` is a live event rather than history: `Notices` peeks and
  `DrainNotices` consumes.
- `usage_update` keeps the latest end-turn token usage (used, size, cost).

Notices and compaction are stable types since schema crate 1.11, so the stable
`schema` and `schema/v2` packages generate them too. The projection still reads
the unstable bindings; stable helpers are tracked in #57.

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
semantics of `agent-client-protocol-rmcp`'s `rmcp_3` builder, without an MCP
SDK. The framework types are aliases of one internal implementation, so
`mcp.Server` and the v2 `mcp.Server` are the same type: define a server once
and attach it to v1 and v2 sessions.

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
  Inner MCP errors travel in `Outcome.Error`; a returned Go error is a binding
  failure (`-33002`).
- **Binding rules.** Requests to an attached server must carry MCP 2026-07-28
  `_meta`; a missing or malformed field is an inner `-32602`, another version
  an inner `-32022`, and `initialize` an inner `-32601`. `server/discover`
  results are restricted to `2026-07-28`. Duplicate active request IDs get
  `-32602`; an unknown or removed server gets `-33001`; `SetMaxActive` limits
  get `-33000`. Cancellation, registration removal, and input EOF cancel the
  operation; the router waits for the service to return (joining cleanup),
  holds the request ID until then, refuses late notifications, and lets
  cancellation win a race with completion.
- **Tools.** Schemas are caller-supplied JSON Schema; no schema is generated.
  `tools/list` reports enabled tools (with `ttlMs: 0` and
  `cacheScope: "private"` on 2026-07-28 requests). In `tools/call`, an unknown
  or disabled tool is an inner `-32602`; arguments that fail the input schema
  or do not decode, and errors returned by the tool, are results with
  `isError: true`; a cancelled call is an inner `-32603`. A tool with an
  `outputSchema` returns `structuredContent`. `serverInfo` reports the name and
  version (default `0.0.0`). `ToolRegistry` supports allow/deny filtering
  (`EnableTool`, `DisableTool`, `EnableAllTools`, `DisableAllTools`, which
  return `*UnknownToolError` for a typo), instructions, and replacement of a
  tool by name. Argument validation implements the structural keywords
  (`type`, `enum`, `const`, `properties`, `required`, `additionalProperties`,
  `items`, `allOf`, `anyOf`, `oneOf`); other keywords, including `$ref`, are
  not enforced.
- **Standalone.** `Server.Serve(ctx, in, out)` serves a direct MCP client over
  newline-delimited JSON-RPC with no ACP session; every request goes to the
  service. A `NewToolServer` service also answers `initialize` and `ping` for
  clients on revisions before 2026-07-28. Client
  `notifications/cancelled` is not mapped to a request, because the generic
  connection does not expose inbound request IDs; input EOF does cancel
  in-flight work.
- **Low-level mover.** `MessageRouter.Register` with a `MessageService` keeps
  working unchanged for callers that implement the MCP protocol themselves.
  Server IDs are never rebound on a router, even after `Unregister`, as the
  RFD requires.

Draft v2 `session/fork` has no facade yet (#61). `HandleSessionSetup` already
rewrites v2 `session/fork` requests, and the v2 per-session helpers share one
`attachTo` helper, so a `ForkAttachedSession` only has to send the request.

