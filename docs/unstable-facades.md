# Unstable typed facades

ACP's optional features live in the generated `schema/unstable` and
`schema/v2/unstable` packages, which already expose every request, response,
and notification type. `github.com/BrokkAi/acp-go/unstable` adds narrow typed
facades on top where a capability check or a composite invariant earns its
keep, and never re-exports them through the stable `acp` or `v2` facades.

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
| `Handle` | — | Agent-side dispatch: routes providers/* and session/fork to optional `ProviderHandler`/`ForkHandler` implementations, answers method-not-found when a handler is missing, and delegates every other method to the next handler. |

## Remaining #10 surfaces

The evaluation of #10 called out more features than one focused change can
carry. These are tracked separately so each keeps its own review:

- NES (`nes/start`, `nes/suggest`, `nes/accept`, `nes/reject`, `nes/close`) with
  its client facade and dispatch interface: #29.
- MCP-over-ACP for the v1 surface, mirroring the existing `v2/mcp` opt-in: #30.
- Plan operations, compaction and notices, and end-turn token usage, which are
  carried inside session updates and may need projection helpers rather than
  request facades: #31.
