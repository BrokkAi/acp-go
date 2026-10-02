# Rust SDK ecosystem parity

[Issue #17](https://github.com/BrokkAi/acp-go/issues/17) asked which crates in
the reference Rust SDK workspace have no Go equivalent, which are worth porting,
and which are not. This document records that evaluation.

It was checked against the same baseline as [ROADMAP.md](../ROADMAP.md): Rust
`agent-client-protocol` 2.2.0. The schema baseline has since moved to
`agent-client-protocol-schema` 1.10.2, which the SDK has not adopted yet. For
each crate we read the workspace `Cargo.toml` and its public documentation,
then compared the surface with what `acp-go` already ships.

## Decision rule

`acp-go` is standard-library-only; [licenses/README.md](../licenses/README.md)
rejects any addition to the module graph. A crate is only portable when its
useful surface can be rebuilt on the Go standard library. Work that would need a
third-party Go module (an MCP SDK, a terminal-UI library, a file-opener module)
is recorded as *won't port* even when the Rust crate itself is valuable.

## Summary

| Rust crate | Decision | Follow-up |
|---|---|---|
| `agent-client-protocol-test` | Port the standard-library-compatible core | [#21](https://github.com/BrokkAi/acp-go/issues/21) |
| `agent-client-protocol-cookbook` | Port the guides as a Go cookbook | [#22](https://github.com/BrokkAi/acp-go/issues/22) |
| `agent-client-protocol-trace-viewer` | Port as an opt-in standard-library tool | [#23](https://github.com/BrokkAi/acp-go/issues/23) |
| `agent-client-protocol-conductor` | Won't port (revisit if a Go consumer appears) | [#24](https://github.com/BrokkAi/acp-go/issues/24) |
| `agent-client-protocol-rmcp` | Won't port | none |

## `agent-client-protocol-test` — port the core

A `publish = false` test crate: in-process transports, a mock transport, a
deterministic agent (`testy`) whose prompts are typed JSON commands, v1 and
draft-v2 fixtures, binary locators, and an MCP echo server. Its dependencies
(`tokio`, `rmcp`, `http`, `schemars`, `uuid`, `tracing`) are all Rust-runtime or
third-party.

`acp-go` today has only in-repo `net.Pipe` tests plus the opt-in
`integration/agent_test.go`; there is no reusable harness, and the router tests
hand-roll framing.

Decision: port the parts that stay standard-library-only — an importable
in-memory duplex transport, `testy`-style deterministic prompt commands, and
v1/v2 fake-agent fixtures shared by the router packages. The MCP echo server and
the `rmcp`-based pieces are out of scope. Delivered as
`github.com/BrokkAi/acp-go/acptest` (#21).

## `agent-client-protocol-cookbook` — port as documentation

A documentation-only crate with no runtime dependencies. It walks through the
three roles (clients, proxies, agents): one-shot v1 and draft-v2 prompts, ordered
application dispatch, draft-v2 session coordination, MCP-tool proxies, reusable
components, and running proxies with the conductor.

`acp-go` has `examples/` and package docs, but no structured cookbook tying those
patterns to `acp`, `v2`, `runner`, `clienthost`, and `proxyrouter`.

Decision: port the guides that map onto the Go packages, with every snippet
compiling as a tested example and no new dependencies. Delivered as
`github.com/BrokkAi/acp-go/cookbook` (#22). The recipes that need an MCP server
SDK (`global_mcp_server`, `per_session_mcp_server`, `filtering_tools`) or the
conductor (`running_proxies_with_conductor`) do not port; the package
documentation records why.

## `agent-client-protocol-trace-viewer` — port as a tool

An interactive sequence-diagram viewer for ACP trace events. It serves an
embedded HTML page over an HTTP server (`axum`/`tokio`) and can read events from
a file (re-read per request for live updates) or push them from memory.

`acp-go`'s `clienthost` already writes JSONL transcripts, but nothing renders
them; debugging means reading raw JSON lines.

Decision: port as an explicit opt-in tool — `net/http` for the server, an
embedded asset for the page, and a JSONL/in-memory event source. This is a
debugging tool, not a transport, and must not change the standard-library stdio
default. Delivered as `github.com/BrokkAi/acp-go/traceviewer` (#23).

## `agent-client-protocol-conductor` — won't port

A binary that spawns a chain of proxy components plus the base agent, routes
`_proxy/successor` envelopes between them, and presents the chain to the editor
as a single ACP agent.

It is an application, not a library. Its only consumer is an editor pointed at a
binary, and no Go proxy components or Go proxy-chain users exist to consume one.
The wire shape is also untrackable here: `_proxy/*` appears nowhere in the
pinned schema (`schema/unstable`, `schema/v2/unstable`, or their method
registries), the Rust `SuccessorMessage` lives in that crate's own
`schema/proxy_protocol.rs`, and the reference design document is explicitly
historical. A Go conductor would hand-model a shape the pin-and-regenerate
discipline cannot keep in sync.

`acp-go` already ships the producer side: `proxyrouter` lets an application write
a Go proxy that the reference conductor can orchestrate. If a concrete Go
consumer for proxy chains appears, revisit with a library helper in
`proxyrouter` for `_proxy/successor` routing, driven by that consumer, rather
than a speculative CLI. Recorded as not planned in #24.

## `agent-client-protocol-rmcp` — won't port

This crate bridges the Rust `rmcp` MCP SDK to the ACP MCP server framework,
building an MCP server from an `rmcp` service and attaching it to a proxy. Its
entire value is the `rmcp` integration, which is a third-party crate.

Porting it would require a third-party Go MCP implementation (for example the
official `modelcontextprotocol/go-sdk`), which the dependency policy forbids.
The ACP-side transport it targets is already covered: `v2/mcp` provides the
opt-in MCP-over-ACP surface with no external module. Revisit only if the
dependency policy changes.

## Related crates outside this evaluation

The Rust workspace also contains crates that #17 did not ask about, listed here
so the decision trail is complete:

- `agent-client-protocol-http` — HTTP/WebSocket transport, implemented as the
  opt-in draft package `transport/http` (HTTP + SSE; WebSocket deferred).
- `agent-client-protocol-derive` — a Rust `proc-macro` crate for trait derivation
  with no Go analogue.
- `agent-client-protocol-polyfill` — backward-compatibility proxies (for example
  adapting MCP-over-ACP to HTTP); no standard-library Go counterpart is needed
  until a concrete consumer appears.
- `agent-client-protocol-yopo` — a `publish = false` one-shot prompt client,
  already covered by `examples/drive-cli`, `examples/v2-one-shot-client`, and
  the `runner` package.
