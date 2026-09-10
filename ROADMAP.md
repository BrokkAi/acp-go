# acp-go roadmap

Goal: make this the canonical Go SDK for [Agent Client Protocol](https://agentclientprotocol.com/)
v1 — coverage on par with the [reference Rust SDK](https://github.com/agentclientprotocol/rust-sdk),
with Go-native ergonomics and zero external dependencies.

## Why this position is open

The ACP project publishes versioned JSON Schema releases and maintains
official SDKs for Kotlin, Java, Python, Rust, and TypeScript. There is no
official Go SDK, and existing community bindings have stalled relative to the
schema. The schema moves monthly (elicitation, boolean config options, and
terminal auth all stabilized within recent releases), so any SDK that
hand-transcribes types drifts. Our answer is mechanical: acp-go binds to the
released schema artifacts and regenerates, making spec tracking a reviewed
diff instead of a rewrite.

## Status

| Phase | Scope | Status |
|-------|-------|--------|
| 0 | Schema codegen pipeline + generated types + parity tests | Done (`832feae`, pinned `schema-v1.21.0`) |
| 1 | Complete v1 client surface | Done |
| 2 | Agent-side runtime | Done |
| 3 | Trust and ecosystem | Next |
| 4 | v2 draft | Planned |

## Phase 1 — complete the v1 client

The runtime (`acp` package, `runner`) migrates onto the generated
[schema](schema/) types; the hand-written wire structs retire. The v0.x
series may break API for this; it lands in one release with a migration note.

- Typed `InitializeResponse`: agent capabilities (`loadSession`,
  `promptCapabilities`, `mcpCapabilities`, `sessionCapabilities`,
  `auth.logout`), `agentInfo`, full auth methods with names and types.
- Session lifecycle: `session/load` (gated on `loadSession`),
  `session/resume`, `session/close`, `session/list`, `session/delete`,
  and `logout` (gated on `auth.logout`).
- Full prompt content blocks: text, image, audio, resource,
  resource_link — sent only when `promptCapabilities` allows.
- Typed session-update callbacks for every kind: plan,
  available commands, current mode, config option updates, session info,
  usage/cost, and `user_message_chunk` replay on load.
- Elicitation host support: `elicitation/create` form and URL modes with
  accept/decline/cancel outcomes and `elicitation/complete`, advertised via
  client capabilities.
- MCP server configs (stdio always; http/sse per `mcpCapabilities`) and
  `additionalDirectories` (gated on `sessionCapabilities`).
- Typed error codes (`auth_required` -32000, resource not found -32002) so
  callers can branch on cause.
- Ergonomic constructors/accessors for union types (e.g. building a
  `SessionUpdate` tool call without knowing the layout).

Acceptance: existing connection and runner tests pass on the generated
types; new methods covered by round-trip tests against golden wire
fixtures; capability gates tested with a simulated agent.

## Phase 2 — agent-side runtime

Serve agents, not just drive them. Most SDK consumers write agents that
want editor integration for free.

- An `Agent` interface covering initialize, session lifecycle, and prompt
  turns, served over stdio by a runtime that owns the JSON-RPC loop.
- Client-capability handler interfaces (filesystem, terminals, permissions,
  elicitation) mirroring what the agent may call.
- Reuse the runner's confined host implementations (workspace-rooted fs,
  process-group terminals, auto-approve policy) as the reference client
  host package, inverted for both sides.
- Examples: a minimal agent, a minimal client, and driving real CLIs
  (claude-code/gemini-style adapters).

## Phase 3 — trust and ecosystem

- Integration tests against the official Claude Agent and Codex ACP adapters
  in a manually enabled CI job; the default test path needs no credentials.
- Fuzz the wire layer (`go test -fuzz`) on top of the malformed-frame handling,
  with a weekly mutation campaign and seed coverage in every default run.
- Track new schema releases as they ship; the update workflow is documented
  in [CONTRIBUTING.md](CONTRIBUTING.md).
- Semver and CHANGELOG discipline; the ACP community libraries page already
  lists `acp-go`.

## Phase 4 — v2 draft

Generate a separate v2 package from the `schema/v2` artifacts when that
spec stabilizes. No cross-version conversion: per the schema team's
guidance, SDKs expose explicit versioned implementations. The codegen
built in Phase 0 makes this mostly free.

## Guardrails

- Standard library only; the dependency policy in
  [licenses/README.md](licenses/README.md) enforces it.
- Open-world enums and preserved unknown tags: future spec values decode,
  never error, per the protocol's extensibility rules.
- No silent fallbacks: selections are acknowledged, capabilities gate
  optional methods, and the generator fails loudly on constructs it cannot
  model.
- Generated files always match the pinned release; regeneration is
  deterministic, so a clean tree after `go run ./cmd/acpgen` proves it.
- Artifact (module) versions are independent of the negotiated
  `protocolVersion`; wire compatibility comes from initialization.
