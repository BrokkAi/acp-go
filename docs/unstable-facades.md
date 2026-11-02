# Unstable typed facades

ACP's optional features live in the generated `schema/unstable` and
`schema/v2/unstable` packages, which already expose every request, response,
and notification type from Rust schema crate 1.15.1's unstable artifacts (the
crate the reference Rust SDK `agent-client-protocol` 5.0.0 pins).
`github.com/BrokkAi/acp-go/unstable` (v1) and
`github.com/BrokkAi/acp-go/v2/unstable` (draft v2) add narrow typed facades on
top where a capability check or a composite invariant earns its keep, and never
re-export them through the stable `acp` or `v2` facades. `v2/unstable` has no
facades at present.

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

## Implemented here

| Facade | Gate | Notes |
|---|---|---|
| `ListProviders`, `SetProvider`, `DisableProvider` | `agentCapabilities.providers` | Requires a non-blank provider ID before writing. |
| `ForkSession` | `sessionCapabilities.fork` | Requires a session ID, an absolute `cwd`, and absolute additional directories; rejects inline MCP servers so MCP stays behind its own import; requires a non-blank forked session ID in the response. |
| `MessageClient` | — | Consumer side of the request-scoped `mcp/message` binding: requires a server ID, request ID, and inner MCP method, rejects duplicate active IDs, routes request-scoped notifications to the owning call, and cancels through the outer ACP request. Implemented for v1 and draft v2. |
| `MessageRouter` | — | Provider side of the request-scoped binding: server-ID registry and dispatch, request-scoped notifications through `MessageRequest.Notify`, `Unregister` cancellation, and the binding's outer error codes (`-32602`, `-32800`, `-33001`, `-33002`). Implemented for v1 and draft v2. |
| `Handle` | — | Agent-side dispatch: routes providers/* and session/fork to optional `ProviderHandler`/`ForkHandler` implementations, answers method-not-found when a handler is missing, and delegates every other method to the next handler. |
| `Projection` | — | Client-side: folds optional session updates into ordered per-session state (see below). `Projection.Notifications` adapts `session/update` notifications. |

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
