# acp-go roadmap

Goal: make acp-go the canonical standard-library-only Go SDK for Agent Client
Protocol, with v1 and draft-v2 behavior aligned to the
[reference Rust SDK](https://github.com/agentclientprotocol/rust-sdk).

## Reference baseline

The behavioral reference is:

- Rust `agent-client-protocol` **2.1.0**
- Rust `agent-client-protocol-schema` **exactly 1.7.0**
- schema crate 1.7.0 contains:
  - ACP v1 **`schema-v1.21.0`**
  - draft ACP v2 **`schema-v2.0.0-alpha.3`**

Our pinned artifacts are byte-for-byte identical to that Rust release. Do not
track ACP repository `main` or a newer schema release until the Rust SDK moves
its exact `=1.7.0` schema dependency.

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
- Successful initialization is allowed once on v1 and v2 client connections.
- v1 and v2 agent endpoints reject mismatched initialize versions before user
  handlers run.
- Explicit agent protocol router:
  - route version 1 to v1,
  - route version 2 or a newer compatible version to v2,
  - canonicalize a newer initialize request to selected v2 parameters,
  - canonicalize v2 initialize parameters when only v1 is configured,
  - never convert traffic after initialization.

## Remaining Rust-parity gaps

### 1. Transport batches and initialize compatibility

The Rust JSON-RPC layer accepts and preserves JSON-RPC batches and validates
same-version initialize parameters before dispatching siblings. Our transport
currently accepts one JSON object per line and dispatches inbound requests
concurrently.

Work:

- Accept JSON-RPC request/response/notification batches without changing the
  line framing.
- Preserve batch member identity and order where Rust does.
- Reject an initialize batch only when Rust rejects it.
- Preserve Rust's tested malformed-v2-initialize retry behavior while batching.

### 2. V2 session command surface

The Rust SDK exposes a `V2Session` command handle and documents ownership
boundaries explicitly.

Work:

- Add an explicit Go v2 session handle for prompt, config, cancel, and close.
- Make `CancelActiveWork` send `session/cancel`, resolve pending permission
  requests as cancelled, and wait for idle with `stopReason: cancelled`.
- Add a resume helper which requires update/permission handlers to be installed
  before replay can begin.
- Keep update projection session-scoped; do not invent prompt or turn IDs.

### 3. Protocol routing for client and proxy peers

The agent-side v1/v2 router is implemented. Rust additionally has explicit
client and proxy protocol connectors/routers.

Work:

- Add a client-side protocol connector that chooses an explicit v1 or v2 client.
- Add proxy routing without converting successor traffic between versions.
- Ensure future-version canonicalization and extension preservation match Rust.

### 4. Unstable Rust feature surfaces

The Rust schema crate exposes separately gated feature surfaces beyond draft
v2. Our generated packages currently cover the stable pinned artifacts.

Work, gated behind explicit Go package opt-ins where applicable:

- unstable LLM providers,
- unstable plan operations,
- unstable session fork/compaction/notices,
- unstable NES,
- unstable tool-call names and end-turn token usage.

### 5. Semantic validation parity

Rust semantic newtypes enforce IDs, absolute paths, media types, and URI forms
at the type boundary. Most Go generated types currently use string aliases.

Work:

- Determine Go-native validation boundaries that do not add reflection-heavy
  runtime overhead.
- Enforce absolute paths and required identifiers at typed facade boundaries.
- Preserve unknown extension tags and raw `_meta` payloads.

### 6. Draft-v2 ecosystem validation

- Extend optional credential-backed CI to exercise the v2 runner against real
  adapters when they advertise v2.
- Add protocol-router fixtures shared by v1 and v2 fake agents.
- Track each Rust schema pin change as a reviewed regeneration and API diff.

## Guardrails

- Standard library only; the dependency policy in
  [licenses/README.md](licenses/README.md) enforces it.
- Open-world enums and preserved unknown tags must continue to decode.
- No silent fallbacks or v1/v2 conversion.
- Capability-gated methods must fail before writing to the wire.
- Generated files must remain byte-deterministic for both pinned artifacts.
- The module version is independent of the negotiated wire version.
