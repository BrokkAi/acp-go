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

Still open from #10: the native MCP-over-ACP *methods* (`mcp/connect`,
`mcp/message`, `mcp/disconnect`). Nothing in the SDK serves or drives them yet,
and the shape decision comes first — a standard-library message mover versus an
integration with a third-party MCP SDK, which the dependency policy forbids
today. Tracked in #39.
