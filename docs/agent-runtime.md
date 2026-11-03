# v1 agent runtime

`github.com/BrokkAi/acp-go/agent` serves an ACP v1 agent over stdio. For the
draft-v2 runtime, see [draft-v2.md](draft-v2.md#agent-runtime).

## Implementing an agent

Implement the mandatory `agent.Agent` interface: `Initialize`, `NewSession`,
and `Prompt`. Optional lifecycle and config methods are discovered as narrow
interfaces such as `SessionLoader`, `SessionCloser`, and `ConfigOptionSetter`.
During a prompt, call `SessionUpdater.Update` to emit typed `session/update`
notifications. [`examples/minimal-agent`](../examples/minimal-agent/main.go)
is a complete echo agent in about 50 lines.

The runtime:

- enforces protocol initialization,
- dispatches generated request and response types,
- maps `session/cancel` to cancellation of the prompt's context, and
- rejects optional methods omitted from the agent's advertised capabilities.

## Calling back into the client

The same package gives agents typed `agent.Client` methods for filesystem,
permission, terminal, and elicitation callbacks.

## Session update capability checks

Before writing an update, the runtime refuses:

| Update | Unless the client advertised |
|---|---|
| `notice` | `session.notices` |
| Compaction updates | `session.compaction` |
| `config_option_update` with a boolean option | `session.configOptions.boolean` |

`Client.Notify` applies the same check to `session/update` params of every
type, so history replay during `session/load` is gated too. For unstable or
hand-built params it also refuses `plan_update` and `plan_removed` without the
unstable `plan` capability, and the subagent updates without the unstable
`subagents` capability.

A refusal is an `*acp.RPCError` with code `-32601`, like an unadvertised host
method. `acp.RequireSessionUpdateCapability` exposes the check.

## Version negotiation and initialization

Version negotiation follows the reference Rust SDK 5.0.0:

- The v1 runtime keeps v1's fallback-to-latest rule: any valid
  `protocolVersion` selects v1, and a request for a newer version reaches the
  agent normalized to v1's shape.
- A missing or malformed `protocolVersion` gets `-32602`.

Like Rust's v1 agent, the runtime does not track the initialization lifecycle
beyond refusing other requests until the first `initialize` succeeds. Every
`initialize`, including a repeat or a concurrent one, reaches the agent, and
the latest successful one sets the advertised capabilities.

## Error responses

These match the reference SDK, and both runtimes and the client hosts answer
the same way:

| Situation | Answer |
|---|---|
| A lifecycle refusal (such as a request before `initialize`) | `-32600`, message `Invalid request`, the explanation as a JSON string in `data` |
| Params that do not decode | `-32602` with `data: {"error": <serde's message>, "json": <params>, "phase": "deserialization"}` |
| A request no handler serves: an unknown or extension method, or an optional method the agent does not implement or advertise | `-32601` (`Method not found`), with the method name as `data` |
| An unknown notification | Ignored |

Absent params read as `null`. See
[Params decoding in the dispatch layers](schema-pin-workflow.md#params-decoding-in-the-dispatch-layers).

## Client hosts

Client applications can compose typed hosts with `agent.HandleFilesystem`,
`HandleTerminal`, `HandlePermissions`, and `HandleElicitation`.

`github.com/BrokkAi/acp-go/clienthost` is the reference workspace-confined host
used by the [runner](runner.md):

- rooted filesystem access,
- process-group terminals,
- bounded output,
- opt-in auto-approval,
- slog streaming, and
- JSONL transcripts (which [`traceviewer`](../README.md#trace-viewer) renders).

Text-file reads accept only regular files; FIFOs and other special files are
rejected without waiting for data.
