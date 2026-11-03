# acp-go

A standard-library-only Go SDK for the
[Agent Client Protocol](https://agentclientprotocol.com/protocol/v1/overview)
(ACP): a client, an agent runtime, a process runner, and draft ACP v2 support.
Behavior tracks the reference Rust SDK, `agent-client-protocol` 5.0.0.

acp-go was extracted from [release-bot](https://github.com/BrokkAi/release-bot)
for reuse by release-bot and issue-bot.

## Install

```sh
go get github.com/BrokkAi/acp-go
```

The import path is `github.com/BrokkAi/acp-go`; the package name is `acp`.
Use `@master` for unreleased changes. During the `0.x` series, minor releases
may contain breaking API changes; see the [changelog](CHANGELOG.md).

## Status

| Surface | Status | Pinned to |
|---|---|---|
| ACP v1 | Stable | `schema-v1.29.1` |
| ACP v2 | Draft, separate packages | `schema-v2.0.0-alpha.13` |
| Unstable ACP features | Opt-in packages only | Rust schema crate 1.15.1 unstable artifacts |
| Streamable HTTP and WebSocket transport | Draft, opt-in | `agent-client-protocol-http` 5.0.0 |

Stdio is the default transport. Every schema pin is the exact artifact set of
Rust schema crate 1.15.1.

## Quick start

### Client

```go
connection := acp.Connect(agentStdout, agentStdin, handleRequest, handleNotification)
defer connection.Close()

_, err := connection.InitializeWithInfo(ctx, acp.Capabilities{}, acp.ClientInfo{
	Name: "my-app", Version: "1.0.0",
})
// handle err

session, err := connection.NewSession(ctx, absoluteWorkingDirectory)
reason, err := connection.Prompt(ctx, session, "Inspect the project")

// After a successful run, write what is still queued before stdin closes.
err = connection.Shutdown(ctx)
```

`Close` aborts the connection and drops frames not yet written; `Shutdown` is
the orderly end of a successful run. You launch the agent process and
implement the handlers for any capabilities you advertise. To have the process
managed for you, use [`runner`](docs/runner.md).

### Agent

```go
type echoAgent struct{}

func (echoAgent) Initialize(context.Context, agent.Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{ProtocolVersion: 1}, nil
}

func (echoAgent) NewSession(context.Context, agent.Client, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	return schema.NewSessionResponse{SessionID: "session-1"}, nil
}

func (echoAgent) Prompt(ctx context.Context, _ agent.Client, request schema.PromptRequest, updates agent.SessionUpdater) (schema.PromptResponse, error) {
	// Stream replies with updates.Update(...).
	return schema.PromptResponse{StopReason: schema.StopReasonEndTurn}, nil
}

func main() {
	if err := agent.New(echoAgent{}).Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		panic(err)
	}
}
```

See [`examples/minimal-agent`](examples/minimal-agent/main.go) for the
complete version.

### Run the examples

```sh
# v1 client talking to the minimal agent
go run ./examples/minimal-client -agent-command 'go run ./examples/minimal-agent' -prompt hello

# CLI driver with mode, model, effort, and auth options; swap in a real agent command
go run ./examples/drive-cli -command 'go run ./examples/minimal-agent' -prompt hello

# Draft v2 one-shot client and agent
go run ./examples/v2-one-shot-client \
  -command 'go run ./examples/minimal-agent-v2' \
  -prompt 'Say hello using draft ACP v2.'
```

For more patterns (one-shot prompts, building an agent, ordered dispatch,
proxies, attaching MCP servers), read the [cookbook](cookbook/doc.go), ported
from the reference Rust cookbook: `go doc github.com/BrokkAi/acp-go/cookbook`.
Every recipe is a runnable example that `go test ./...` executes.

## Packages

All paths are under `github.com/BrokkAi/acp-go`.

| Package | Purpose | Docs |
|---|---|---|
| `.` (`acp`) | v1 client and the JSON-RPC connection every role uses | [client](docs/client.md), [connection](docs/connection.md) |
| `agent` | v1 agent runtime over stdio, plus typed client host adapters | [agent runtime](docs/agent-runtime.md) |
| `clienthost` | Workspace-confined reference host: rooted filesystem, terminals, auto-approval, transcripts | [agent runtime](docs/agent-runtime.md#client-hosts) |
| `runner` | Runs an agent process end to end and returns its text | [runner](docs/runner.md) |
| `schema` | Generated v1 wire types and method registry | [connection](docs/connection.md#generated-schema-types), [schema pins](docs/schema-pin-workflow.md) |
| `jsonvalue` | Untyped JSON held as `serde_json` holds it (backs `_meta`) | [connection](docs/connection.md#_meta-and-untyped-json) |
| `tracecontext` | W3C Trace Context in `_meta` | [connection](docs/connection.md#trace-context) |
| `v2`, `v2/agent`, `v2/runner`, `schema/v2` | Draft ACP v2 client, agent runtime, one-shot runner, and wire types | [draft v2](docs/draft-v2.md) |
| `agentrouter`, `clientrouter`, `proxyrouter` | Serve or reach v1 and v2 from one endpoint | [draft v2](docs/draft-v2.md#version-routers) |
| `unstable`, `v2/unstable`, `schema/unstable`, `schema/v2/unstable` | Opt-in unstable features: providers, session fork/archive/status/title, auth status, system prompts, subagents, plans, usage | [unstable facades](docs/unstable-facades.md) |
| `mcp`, `v2/mcp` | Opt-in MCP-over-ACP and a standard-library MCP server framework | [unstable facades](docs/unstable-facades.md#mcp-server-framework) |
| `transport/http` | Draft Streamable HTTP and WebSocket transport | [HTTP transport](docs/http-transport.md) |
| `acptest` | In-memory transport, deterministic test agent, and framing helpers for tests | `go doc` |
| `traceviewer`, `cmd/acp-trace-viewer` | Renders JSONL transcripts as a sequence diagram | [below](#trace-viewer) |
| `cookbook` | Runnable recipes for common patterns | `go doc` |

The stable `acp`, `v2`, `schema`, and `schema/v2` packages never import or
expose the unstable surface.

## Tools

### Trace viewer

`cmd/acp-trace-viewer` renders a JSONL transcript (the files `clienthost`
writes) as an ordered sequence diagram, served over `net/http` from an embedded
page with no external assets:

```sh
go run ./cmd/acp-trace-viewer -addr 127.0.0.1:8787 session.jsonl
```

The `traceviewer` package behind it offers `FileSource`, which re-reads the
transcript on every poll so a live session updates in place, and `Memory`,
which serves events pushed by a host. It is a developer tool, not a transport.

### Test harness

`acptest` is the standard-library-only test harness. `acptest.NewPair` returns
an in-memory duplex transport that either a v1 `acp.Connection` or a draft-v2
`v2.Connection` can drive; `acptest.TestAgent` executes deterministic typed
prompt commands (`acptest.Command`). The package also ships v1 and draft-v2
fake agents and framing helpers (`Serve`, `WriteRequest`, `ReadResponse`) for
protocol-router tests. The router tests use it themselves, so the harness
cannot drift.

## Documentation

- [v1 client](docs/client.md): features, capability checks, session updates, elicitation, migrating to the schema types
- [Connection and wire behavior](docs/connection.md): handlers, error handling, frame encoding, `RPCError`, `_meta`, generated types
- [v1 agent runtime](docs/agent-runtime.md): implementing an agent, capability gates, error responses, client hosts
- [Process runner](docs/runner.md): permissions, errors, process lifecycle, terminal auth
- [Draft ACP v2](docs/draft-v2.md): prompt semantics, projections, session tracking, initialization, routers, MCP
- [Unstable facades](docs/unstable-facades.md): the opt-in unstable surface and MCP server framework
- [Draft HTTP transport](docs/http-transport.md): the HTTP and WebSocket binding and its limits
- [Validation boundaries](docs/validation-boundaries.md): where requests are validated
- [Schema pin workflow](docs/schema-pin-workflow.md): regenerating for a new schema release; decode and encode rules
- [Rust ecosystem parity](docs/rust-ecosystem-parity.md): which Rust SDK crates are ported, and why

## Development

```sh
go test -race ./...
go vet ./...
```

No agent credentials are needed, and the fuzz seed corpus runs as part of the
tests. See [CONTRIBUTING.md](CONTRIBUTING.md) for longer fuzz campaigns and
opt-in tests against real ACP agents.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and our
[Code of Conduct](CODE_OF_CONDUCT.md). Report vulnerabilities privately using
[SECURITY.md](SECURITY.md).

## License

Licensed under [Apache-2.0](LICENSE). See [NOTICE](NOTICE) for project
attribution and [licenses/README.md](licenses/README.md) for dependency terms,
third-party notices, and the license review process.
