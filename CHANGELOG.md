# Changelog

All notable changes to this project are documented in this file. Release
entries use [Semantic Versioning](https://semver.org/); during the `0.x`
series, minor releases may contain breaking API changes.

## Unreleased

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
