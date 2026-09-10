# acp-go

A small, standard-library-only Go client for [Agent Client Protocol v1](https://agentclientprotocol.com/protocol/v1/overview), extracted from [release-bot](https://github.com/BrokkAi/release-bot) for reuse by release-bot and issue-bot.

```sh
go get github.com/BrokkAi/acp-go@master
```

The generated-schema API below is the v0.2 surface.

```go
connection := acp.Connect(stdout, stdin, handleRequest, handleNotification)
defer connection.Close()
_, err := connection.InitializeWithInfo(ctx, acp.Capabilities{}, acp.ClientInfo{
    Name: "my-app", Version: "1.0.0",
})
// Handle err, then create a session and send prompts.
session, err := connection.NewSession(ctx, absoluteWorkingDirectory)
reason, err := connection.Prompt(ctx, session, "Inspect the project")
```

The import path is `github.com/BrokkAi/acp-go`; its package name is `acp`.
The runtime now uses the generated [`schema`](schema/) types directly: `Capabilities`,
`Initialization`, `Session`, `Content`, `Update`, and `ClientInfo` are aliases
into that package. This is an intentional v0.x API break. Use
`acp.WorkspaceCapabilities(readFiles, writeFiles, terminal)` for the former
boolean FS/terminal fields, `Session.SessionID` instead of `Session.ID`, and
the content constructors (for example `acp.NewTextContent`) instead of
hand-written discriminator fields.

The caller owns launching the process and implementing the filesystem, terminal,
and permission handlers it advertises. Capabilities default to disabled.
Authentication, modes (including generated mode state), model and boolean config
selection, reasoning effort, prompt content blocks, MCP servers, additional
directories, session load/resume/list/close/delete, logout, and typed error
classification are supported. Optional methods and prompt content are checked
against the agent's advertised capabilities before a request is sent. Use
`SetModel` before `SetEffort`: models may expose different effort choices.
Explicit selections must be acknowledged by the agent; they never silently fall back.

`acp.SessionUpdates` adapts `session/update` notifications to the generated
discriminated union. `acp.HandleElicitation`, the elicitation response
constructors, `ElicitationComplete`, and `CombineNotifications` provide typed
form/URL host support, including the agent's URL-completion notification.
`NewSessionWithOptions`, `LoadSession`, and `ResumeSession` validate MCP
transports and additional workspace roots against initialization capabilities.

`Connect` owns and closes both streams. Notifications run in wire order before
responses and must return promptly without calling back into the connection.
Requests run concurrently and must honor cancellation. Frames are bounded at
8 MiB and incoming requests at 32. `Close` cancels and joins handlers.
Generic `Call` and `Notify` permit extensions. ACP v2 is not supported.

Run `go test -race ./...` and `go vet ./...`. No agent credentials are needed.
The fuzz seed corpus runs as part of that command; see
[CONTRIBUTING.md](CONTRIBUTING.md) for longer campaigns and opt-in tests
against real ACP agents.

## Agent runtime

`github.com/BrokkAi/acp-go/agent` serves an agent over stdio. Implement the
mandatory `agent.Agent` interface (`Initialize`, `NewSession`, and `Prompt`);
optional lifecycle and config methods are discovered as narrow interfaces such
as `SessionLoader`, `SessionCloser`, and `ConfigOptionSetter`. During a prompt,
call `SessionUpdater.Update` to emit typed `session/update` notifications. The
runtime enforces protocol initialization, dispatches generated request/response
types, maps `session/cancel` to prompt-context cancellation, and rejects optional
methods omitted from the agent's advertised capabilities.

The same package gives agents typed `agent.Client` methods for filesystem,
permission, terminal, and elicitation callbacks. Client applications can compose
typed hosts with `agent.HandleFilesystem`, `HandleTerminal`,
`HandlePermissions`, and `HandleElicitation`.
`github.com/BrokkAi/acp-go/clienthost` is the reference workspace-confined host
used by the runner: rooted filesystem access, process-group terminals, bounded
output, opt-in auto-approval, slog streaming, and JSONL transcripts.

Runnable examples are included:

```sh
go run ./examples/minimal-client -agent-command 'go run ./examples/minimal-agent' -prompt hello
go run ./examples/drive-cli -command 'go run ./examples/minimal-agent' -prompt hello
```

`drive-cli` accepts the same mode/model/effort/auth options commonly needed by
external CLI adapters; substitute the real agent command for the minimal agent.

## Wire schema

`github.com/BrokkAi/acp-go/schema` provides typed constants, request and
response structs, notifications, and a method registry for every message in
the pinned [ACP JSON Schema release](https://github.com/agentclientprotocol/agent-client-protocol/releases)
(`schema-v1.21.0` at the time of writing), including session updates, content
blocks, tool calls, permissions, terminals, elicitation, and config options.
The package is generated by `cmd/acpgen` and guarded by round-trip parity
tests, so tracking a new schema release is a reviewed regeneration rather
than hand transcription. See [CONTRIBUTING.md](CONTRIBUTING.md) for the
update workflow.

## Process runner

`github.com/BrokkAi/acp-go/runner` adds a process lifecycle, confined client file
operations, terminal callbacks, streaming slog output and private JSONL transcripts.
`Runner.Execute(ctx, prompt)` returns the complete agent text; applications own
receipt parsing and workflow policy. Set `Config.AutoApprove` explicitly to allow
permission requests for unattended operation. It defaults to false. This is not
an OS sandbox: agents and terminal commands inherit the caller's permissions.
`SetupError` distinguishes failures before a prompt from failures during work.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and our
[Code of Conduct](CODE_OF_CONDUCT.md). Report vulnerabilities privately using
[SECURITY.md](SECURITY.md).

## License

Licensed under [Apache-2.0](LICENSE). See [NOTICE](NOTICE) for project
attribution and [licenses/README.md](licenses/README.md) for dependency terms,
third-party notices, and the license review process.
