# acp-go roadmap

Goal: make acp-go the canonical standard-library-only Go SDK for Agent Client
Protocol, with v1 and draft-v2 behavior aligned to the
[reference Rust SDK](https://github.com/agentclientprotocol/rust-sdk).

## Reference baseline

The behavioral reference is:

- Rust `agent-client-protocol` **2.2.0**
- Rust `agent-client-protocol-schema` **exactly 1.9.1**
- schema crate 1.9.1 contains:
  - ACP v1 **`schema-v1.23.0`**
  - draft ACP v2 **`schema-v2.0.0-alpha.5`**

Our pinned artifacts are byte-for-byte identical to that Rust release. Do not
track ACP repository `main` or a newer schema release until the Rust SDK moves
its exact `=1.9.1` schema dependency.

## Status

| Area | Status |
|---|---|
| Schema generator, parity tests, exact Rust artifact pins | Done |
| v1 client surface | Done |
| v1 agent runtime and reference client host | Done |
| Fuzzing, licenses, release discipline, optional real-agent CI | Done |
| Draft-v2 schema, client, agent runtime, host adapters, one-shot runner | Done |
| Rust-parity hardening | In progress |

Completed roadmap phases are retained in `CHANGELOG.md`; this file now tracks
only remaining work.

## Rust-parity work already closed

- Separate v1 and v2 generated schema packages with no implicit conversion.
- v1 client and agent runtimes.
- Draft-v2 client and baseline agent runtime.
- Typed permission and elicitation host adapters.
- One-shot v2 runner with the reference update projection:
  ignore until `running`, apply message chunks and patch snapshots, complete at
  the next `idle`.
- Explicit MCP-over-ACP opt-in through `github.com/BrokkAi/acp-go/v2/mcp`,
  mirroring Rust's separate `unstable_mcp_over_acp` feature boundary.
- Exact JSON Schema integer formats, notably Rust-compatible `uint16`
  `ProtocolVersion` and `int32` error codes.
- Stable programmatic tool-call names in v1 and draft v2, surfaced by the
  reference client host with `tool_call_update` patch semantics.
- Required draft-v2 prompt acceptance identity: the client returns the
  response's `messageId` and the agent runtime refuses to answer
  `session/prompt` without one.
- Successful initialization is allowed once on v1 and v2 client connections.
- v1 and v2 agent endpoints reject mismatched initialize versions before user
  handlers run.
- Explicit agent protocol router:
  - route version 1 to v1,
  - route version 2 or a newer compatible version to v2,
  - canonicalize a newer initialize request to selected v2 parameters,
  - canonicalize v2 initialize parameters when only v1 is configured,
  - never convert traffic after initialization.
- JSON-RPC batch support:
  - inbound request, notification, response, and mixed batches,
  - one grouped response array for request-bearing batches,
  - malformed response-shaped members ignored as Rust ignores them,
  - malformed call-shaped members answered with Invalid Request,
  - duplicate and overlapping inbound request IDs,
  - response batches routed to pending calls by ID,
  - batch completion waits for trailing notification handlers,
  - an incomplete batch does not block independent standalone calls,
  - explicit outbound `CallBatch` support,
  - initial initialize parameters validated before sibling dispatch by the
    protocol router.
- Rust-shaped v2 session command surface:
  - reusable agent-message projection with omitted/null/value patch semantics,
  - per-session active-work tracker using running→idle completion,
  - explicit `SessionHandle`,
  - prompt acceptance, config-option replacement, and close commands,
  - pending permission cancellation,
  - exact Rust `CancelActiveWork` notification behavior plus an optional
    cancellation-completion barrier,
  - from-start resume replay after preinstalled handlers.
- Explicit client protocol connector:
  - select the highest configured v1/v2 client implementation,
  - start with an individual initialize request,
  - reuse the existing agent connection only when v2 initialization is
    losslessly representable as the configured v1 initialization,
  - reconnect with fresh factories when parameters differ,
  - surface v2 rejection without silently retrying as v1.
- Exact-match proxy protocol router:
  - select only the configured exact v1/v2 `_proxy/initialize` version,
  - validate the selected initialize schema,
  - preserve the complete initial single or batch frame without canonicalization,
  - reject future versions rather than downgrading.
- Unstable schema surfaces from the exact Rust 1.9.1 unstable artifacts:
  - v1 and v2 generated bindings in explicit import-only packages,
  - LLM providers,
  - plan operations,
  - session fork, compaction, and notices,
  - NES,
  - MCP-over-ACP,
  - end-turn token usage,
  - bidirectional `mcp/message` request/notification registry metadata.
- Optional credential-backed v2 integration coverage: the draft-v2 runner is
  exercised against a real adapter when it advertises v2, and the CI integration
  job runs the v1 and v2 tests together.
- Test harness (`acptest`): an in-memory duplex transport, deterministic typed
  prompt commands, framing helpers, and v1/draft-v2 fake agents shared by the
  agent, proxy, and client router tests.
- Schema pin-change workflow (`docs/schema-pin-workflow.md`): regenerate both
  artifacts and review the generated API diff before either pin moves.
- Semantic validation at the typed facade boundaries
  (`docs/validation-boundaries.md`): absolute paths, required identifiers,
  media types, and URIs are checked before a request reaches the wire, with the
  generated types left permissive so unknown tags and raw `_meta` keep decoding.

## Remaining Rust-parity gaps

### 1. Optional typed facades for unstable methods

The typed facade surface is delivered: client facades and agent dispatch for
providers, session fork, and NES, the session-update projection helper, and the
MCP session options all live in
[docs/unstable-facades.md](docs/unstable-facades.md)'s packages
(`unstable`, `v2/unstable`, `mcp`, `v2/mcp`).

What remains is the native MCP-over-ACP method dispatch:

- Serve or drive `mcp/connect`, `mcp/message`, and `mcp/disconnect` behind an
  explicit import, after deciding whether the Go side only moves MCP messages or
  integrates a third-party MCP SDK (which the dependency policy forbids today):
  #39.

### 2. Rust ecosystem crate ports

[docs/rust-ecosystem-parity.md](docs/rust-ecosystem-parity.md) evaluates the
reference workspace crates that have no Go equivalent and records one decision
per crate. Every port is done: the test harness (`acptest`), the trace viewer
(`traceviewer`), and the cookbook of client, proxy, and agent patterns
(`cookbook`, #22).

Two crates are recorded as won't port: the `agent-client-protocol-rmcp` bridge,
which needs a third-party MCP SDK, and the proxy-chain conductor, which is an
application with no Go consumer and whose `_proxy/*` wire shape the pinned
schema does not cover. Every port stays standard-library-only and behind its own
package or command.

## Guardrails

- Standard library only; the dependency policy in
  [licenses/README.md](licenses/README.md) enforces it.
- Open-world enums and preserved unknown tags must continue to decode.
- No silent fallbacks or v1/v2 conversion.
- Capability-gated methods must fail before writing to the wire.
- Generated files must remain byte-deterministic for both pinned artifacts.
- The module version is independent of the negotiated wire version.
