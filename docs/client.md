# v1 client

The root package (`github.com/BrokkAi/acp-go`, package name `acp`) is the
stable ACP v1 client. For the transport underneath it, see
[connection.md](connection.md).

## Responsibilities

The caller owns launching the agent process and implementing the filesystem,
terminal, and permission handlers it advertises. Capabilities default to
disabled. If you would rather not manage the process yourself, use
[`runner`](runner.md).

## Supported features

- Authentication and logout
- Session new, load, resume, list, close, and delete
- Modes (including generated mode state), model and boolean config selection,
  and reasoning effort
- Prompt content blocks
- MCP servers and additional workspace directories
- Typed error classification

Optional methods and prompt content are checked against the agent's advertised
capabilities before a request is sent. `NewSessionWithOptions`, `LoadSession`,
and `ResumeSession` validate MCP transports and additional workspace roots
against initialization capabilities. [validation-boundaries.md](validation-boundaries.md)
lists where validation happens.

### Selections

Call `SetModel` before `SetEffort`: models may expose different effort
choices. Explicit selections must be acknowledged by the agent; they never
silently fall back.

## Initialization

The v1 client follows Rust's unchecked v1 client:

- `InitializeWithInfo` may be repeated at any time.
- A response's `protocolVersion` is checked with Rust's errors: `-32602` when it
  is missing or invalid, and `-32600` when it is 2
  (``required ACP protocol version 1 but peer negotiated 2; ...``) or any other
  version. Neither closes the connection.

`acp.Connection` itself is version-neutral, so a raw `Call` with `initialize`
is sent as given. Use `InitializeWithInfo` for the v1 client role.

## Session updates

`acp.SessionUpdates` adapts `session/update` notifications to the generated
discriminated union.

`acp.SessionProjection` folds session notices and compaction per session, in
receive order. Both are stable v1 features that a client must advertise with
`SessionNoticesClientCapabilities` and `SessionCompactionClientCapabilities`
(combined with other session capabilities through `SessionClientCapabilities`).

- **Compactions** keep first-seen order. `summary`, `error`, and `_meta` apply
  as patches: unset keeps, `null` or `summary: []` clears, and a value
  replaces. Summary chunks append to the current summary.
- **Notices** are live events. `Notices` peeks at them and `DrainNotices`
  consumes them.

Replay through `session/load` takes the same path as live delivery.

## Elicitation

`acp.HandleElicitation`, the elicitation response constructors,
`ElicitationComplete`, and `CombineNotifications` provide typed form and URL
elicitation support for hosts, including the agent's URL-completion
notification.

## Migrating to the generated-schema API

Since v0.2 the runtime uses the generated [`schema`](../schema/) types
directly: `Capabilities`, `Initialization`, `Session`, `Content`, `Update`, and
`ClientInfo` are aliases into that package. This was an intentional v0.x API
break.

| Before | Now |
|---|---|
| Boolean FS and terminal capability fields | `acp.WorkspaceCapabilities(readFiles, writeFiles, terminal)` |
| `Session.ID` | `Session.SessionID` |
| Hand-written content discriminator fields | Content constructors such as `acp.NewTextContent` |
