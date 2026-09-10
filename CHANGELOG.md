# Changelog

All notable changes to this project are documented in this file. Release
entries use [Semantic Versioning](https://semver.org/); during the `0.x`
series, minor releases may contain breaking API changes.

## 0.5.0 - 2026-09-10

### Changed

- Generated JSON Schema integer fields now use their exact wire widths. Most
  visibly, v1 and v2 `ProtocolVersion` are `uint16` and error codes are
  `int32`, matching the Rust schema crate.
- Successful v1 and v2 client initialization is now allowed only once per
  connection.
- v1 and v2 agent runtimes reject mismatched initialize versions before invoking
  the implementation.

### Added

- An explicit v1/v2 agent protocol router that selects the highest configured
  compatible implementation and canonicalizes only the initial request.
- An explicit draft MCP-over-ACP opt-in package at
  `github.com/BrokkAi/acp-go/v2/mcp`; the core v2 session API no longer accepts
  MCP servers directly.

## 0.4.0 - 2026-09-10

### Added

- Typed v2 client-host adapters for permission and elicitation requests.
- A draft-v2 one-shot process runner that mirrors the reference SDK: updates
  are installed before session setup, queued output is ignored until the
  session reports running, agent messages honor chunk and `Nullable` patch
  semantics, foreground work completes at the next idle update, and permission
  requests default to explicit cancellation.
- A `v2-one-shot-client` example modeled on the reference SDK example.

## 0.3.0 - 2026-09-10

### Added

- Generated draft-v2 schema bindings pinned to the official
  `schema-v2.0.0-alpha.3` release.
- A separate v2 client package for v2 initialization, authentication, session
  lifecycle, prompt acceptance, cancellation, capability checks, and typed
  session updates.
- A draft v2 agent runtime for baseline sessions, prompt acceptance,
  cancellation, optional lifecycle/config methods, and typed editor callbacks.
- Generated open unions now retain the unknown discriminator in `Kind` as well
  as the raw payload in `Other`.
- Generated v2 patch fields use `Nullable[T]` to distinguish omitted values,
  explicit JSON null values, and concrete payloads.

## 0.2.1 - 2026-09-10

### Fixed

- Made the agent runtime's session-cancel hook test wait for the concurrently
  invoked optional hook, removing a narrow CI race.

## 0.2.0 - 2026-09-10

### Added

- Generated schema bindings for every message in ACP `schema-v1.21.0`, with
  golden wire fixtures and parity tests.
- The complete v1 client surface, including sessions, prompt content, config
  selection, authentication, elicitation, MCP servers, and typed errors.
- An agent-side stdio runtime, typed editor callbacks, a reusable reference
  client host, and runnable client/agent examples.
- Seed-corpus and mutation fuzzing for the JSON-RPC wire layer.
- Opt-in CI integration tests against the official Claude Agent and Codex ACP
  adapters. Ordinary CI and local tests never require credentials.

### Changed

- The public client API now exposes generated schema types directly. This is a
  breaking transition from the hand-written v0.1.0 wire types.
- Terminal, transcript, and workspace-host implementations moved from
  `runner` to the reusable `clienthost` package; `runner` now composes that
  host instead of duplicating it.
