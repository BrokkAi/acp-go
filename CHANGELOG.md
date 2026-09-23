# Changelog

All notable changes to this project are documented in this file. Release
entries use [Semantic Versioning](https://semver.org/); during the `0.x`
series, minor releases may contain breaking API changes.

## Unreleased

### Added

- `runner.SetupError` and `v2/runner.SetupError` gain a `Phase` field naming
  the setup step that failed, with exported constants (`PhaseLaunch`,
  `PhaseInitialize`, `PhaseAuthenticate`, `PhaseSessionNew`, and
  `PhasePrompt`; the v1 runner adds `PhaseSelectMode`, `PhaseSelectModel`, and
  `PhaseSelectEffort`). The values match the phase recorded in the transcript's
  `session_end` event, plus `launch` for failures before the agent connection
  exists. `Error()` text is unchanged.
- `acp.UnknownSelectionError` is returned by `SetModel`, `SetEffort`, and
  config-option `SetMode` when the requested value is not offered. It carries
  the requested category, config ID, option name, value, and advertised values.
- `acp.UnsupportedSelectionError` is returned when the agent advertises no
  model, reasoning effort, or mode selector.

  Both selection errors keep the previous `Error()` text and pass through
  `SetupError` and the runner's diagnostics wrapping, so callers can use
  `errors.As` to tell an unoffered value from a missing selector and from an
  agent-side rejection (`*acp.RPCError`).

### Changed

- The v1 runner now labels a failure to record the prompt in the transcript as
  the `session/prompt` phase instead of the preceding selection phase.

## 0.9.0 - 2026-09-22

### Changed

- Track the Rust SDK's new schema pin. The behavioral reference is now Rust
  `agent-client-protocol` 2.2.0 with `agent-client-protocol-schema = "=1.9.1"`,
  so the pinned artifacts move from `schema-v1.21.0` to `schema-v1.23.0` and
  from `schema-v2.0.0-alpha.3` to `schema-v2.0.0-alpha.5`. All four generated
  packages were regenerated from the exact Rust 1.9.1 artifacts.
- Draft-v2 `session/prompt` now reports the identity of the user message it
  inserted. `schema-v2.0.0-alpha.5` makes `PromptResponse.messageId` required
  and non-null, so `v2.Connection.Prompt`, `v2.Connection.PromptContent`,
  `v2.SessionHandle.Prompt`, and `v2.SessionHandle.PromptContent` return a
  `MessageID` alongside the error, and `v2/runner.Result` gains
  `UserMessageID`. This is a breaking API change in the draft-v2 packages; the
  stable v1 surface is unchanged.

### Added

- Programmatic tool-call names are now part of the stable v1 and v2 schema
  packages, following schema 1.8.0. `clienthost` logs the name with each tool
  call and keeps it across updates that omit it.
- Session notices reach `schema/unstable` and `schema/v2/unstable`, including
  the `notice` session update, `NoticeSeverity`, and the v1 client
  `notices` session capability. Earlier releases documented notices as
  available; they were not present in the 1.7.0 artifacts this project pinned.

### Fixed

- The draft-v2 agent runtime refuses to answer `session/prompt` when an
  implementation returns an empty `MessageID`, and the v2 client rejects an
  acceptance that omits it. Both previously passed an invalid empty identifier
  through as a success.
- `clienthost` no longer replaces a tool call's title with its tool-call ID
  when a `tool_call_update` omits the title. Omitted title and name fields are
  patch fields and leave the retained value unchanged.

## 0.8.1 - 2026-09-22

### Fixed

- `clienthost.Host.Answer` now starts a new line when the agent starts a new
  message, instead of running the end of one message into the start of the
  next. The protocol says a change in `messageId` begins a new message, and an
  agent may end one without a trailing newline, so a client reading the final
  answer off the last line could see it welded to the commentary before it.
  Chunks of a single message are still joined exactly as they arrive, and an
  agent that omits `messageId` keeps the previous single-buffer behavior.

## 0.8.0 - 2026-09-15

### Added

- An explicit v1/v2 client protocol connector with Rust-compatible matching,
  reconnection, and no-retry-on-v2-rejection behavior.
- An exact-match proxy protocol router that preserves the complete initial
  `_proxy/initialize` frame and rejects unsupported future versions.
- Generated v1 and v2 unstable-schema packages pinned to the exact Rust schema
  1.7.0 unstable artifacts. They collectively expose optional providers, plan
  operations, session fork/compaction, NES, MCP-over-ACP, tool-call names, and
  end-turn token usage without changing the stable packages.
- Method registry support for bidirectional methods that have both request and
  notification payloads, matching Rust `mcp/message`.

### Fixed

- Reject non-regular text-file reads without blocking on FIFOs, preventing
  filesystem callbacks from trapping runner shutdown.
- Release completed request slots before responses become visible to peers,
  allowing replacement requests at the 32-handler concurrency limit.
- Resolve terminal workspace roots consistently so explicit working directories
  work through symlinks while paths outside the workspace remain rejected.

## 0.7.0 - 2026-09-10

### Added

- A Rust-shaped draft-v2 session command handle covering prompt acceptance,
  authoritative config-option replacement, close, session cancellation, and
  optional cancellation completion.
- Public v2 agent-message projection, active-work tracking, and per-session
  update tracking.
- Cancellable v2 permission hosting that resolves pending requests with the
  protocol's cancelled outcome.
- A from-start v2 resume helper that applies replay updates before returning
  the resume response.

## 0.6.0 - 2026-09-10

### Added

- JSON-RPC batch ingress and grouped request responses, matching the reference
  transport’s handling of requests, notifications, responses, malformed members,
  duplicate IDs, trailing notification handlers, and independent standalone
  calls.
- `Connection.CallBatch` for outbound batch requests whose replies may arrive
  individually or in a response batch.
- Agent protocol-router validation of initial v1/v2 initialize batches before
  sibling dispatch.

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
