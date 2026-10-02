# Unstable typed facades

ACP's optional features live in the generated `schema/unstable` and
`schema/v2/unstable` packages, which already expose every request, response,
and notification type. `github.com/BrokkAi/acp-go/unstable` (v1) and
`github.com/BrokkAi/acp-go/v2/unstable` (draft v2) add narrow typed facades on
top where a capability check or a composite invariant earns its keep, and never
re-export them through the stable `acp` or `v2` facades.

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
| `StartNes`, `SuggestNes`, `AcceptNes`, `RejectNes`, `CloseNes` | `agentCapabilities.nes` | Requests: start/suggest/close; notifications: accept/reject. Requires non-blank session and suggestion IDs, and an absolute document URI. Implemented for v1 and draft v2. |
| `Handle` | — | Agent-side dispatch: routes providers/* and session/fork to optional `ProviderHandler`/`ForkHandler` implementations, answers method-not-found when a handler is missing, and delegates every other method to the next handler. |
| `HandleNesNotifications` | — | Composes the agent side of `nes/accept`/`nes/reject` (notifications) with another `acp.Notifications` handler. |
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

MCP-over-ACP for the v1 surface landed as `github.com/BrokkAi/acp-go/mcp`,
mirroring `v2/mcp`: session create, resume, and fork helpers that
accept the unstable server transports, including the native `acp` variant.

## MCP-over-ACP methods: a request-scoped message mover

The native MCP-over-ACP *methods* are still open from #10. The pinned schemas
(`schema-v1.24.1` and `schema-v2.0.0-alpha.7`) model the request-scoped
binding that upstream shipped in
[agentclientprotocol/agent-client-protocol#2223](https://github.com/agentclientprotocol/agent-client-protocol/pull/2223):
`mcp/connect` and `mcp/disconnect` are gone, and each `mcp/message` request
carries a `serverId`, a caller-generated `requestId`, and the inner MCP method
and params. Nothing in the SDK serves or drives it yet.

**Decision:** the binding will be a standard-library message mover that carries
MCP requests, results, and notifications and leaves the MCP protocol to the
caller. It will not integrate a third-party Go MCP SDK, for the same
dependency-policy reason that `agent-client-protocol-rmcp` is won't port.

In that revision:

- `mcp/connect` and `mcp/disconnect` are gone. Each agent-to-provider
  `mcp/message` request carries a `serverId`, a caller-generated `requestId`,
  and the inner MCP method and params.
- The response carries exactly one inner MCP `result` or `error`, and inner MCP
  errors never become outer ACP errors.
- The provider (the client or proxy that declared the `acp` server) sends only
  notifications tied to an active request, never requests of its own.
- It targets MCP 2026-07-28 only.

The released schema crate carries the change, so implementation is unblocked
and tracked in #39. It is expected to live in `mcp` and `v2/mcp` as an
agent-side caller plus a provider-side server-ID registry and dispatcher, with
tests for the result/error split, request IDs, notification ordering, and
cancellation.
