# Draft Streamable HTTP transport

`github.com/BrokkAi/acp-go/transport/http` (package `acphttp`) is an explicit,
opt-in binding for the ACP *Streamable HTTP* transport. The ACP specification
still lists that transport as a draft proposal in progress, so this package is
gated three ways:

1. **Import boundary.** Nothing in the SDK depends on it; a caller must import
   `transport/http` to use it.
2. **No fallback.** The package never falls back to stdio, and `Dial` never
   retries a different transport. The caller picks one binding.
3. **Stdio stays the default.** `acp.Connect` and the `agent`/`runner` packages
   are unchanged.

The wire shape mirrors the reference Rust crate
`agent-client-protocol-http`, which is the only concrete description of the
draft today.

## Binding

| Element | Behavior |
|---|---|
| Endpoint | `Path` (default `/acp`). `Dial` accepts a base URL and appends `/acp` when the path is empty or missing it. |
| `POST` initialize | `Content-Type: application/json`. The server creates a connection, answers the `initialize` request in the body, and returns the connection id in `Acp-Connection-Id`. |
| `POST` later frames | Also `application/json`, with `Acp-Connection-Id`; answered `202 Accepted` and the response arrives on the stream. |
| `Acp-Session-Id` | Required for the session-scoped methods the Rust crate lists (`session/prompt`, `session/cancel`, `session/close`, `session/delete`, `session/fork`, `session/load`, `session/resume`, `session/set_config_option`, `session/set_mode`, `session/set_model`). |
| `GET` stream | `Accept: text/event-stream` with `Acp-Connection-Id`; server-to-client frames arrive as `data:` events. |
| Status codes | `415` for a missing JSON content type, `400` for a missing connection id or session header, `404` for an unknown connection, `406` for a stream without the event-stream `Accept`. |

## Usage

Server side, serve any existing agent runtime over HTTP:

```go
acpServer := &acphttp.Server{Entry: agent.New(myAgent)} // path defaults to /acp
http.ListenAndServe(":8080", acpServer)
```

Client side, dial and then drive the connection exactly like stdio:

```go
connection, err := acphttp.Dial(ctx, "https://agent.example.com", clientHandler, notifications)
initialization, err := connection.InitializeWithInfo(ctx, capabilities, info)
```

Both `github.com/BrokkAi/acp-go/agent.Runtime.Serve` and
`.../v2/agent.Runtime.Serve` satisfy `acphttp.Entry`, so the same agent
implementation can be served over stdio or HTTP without changes.

## Deliberate limits

These are out of scope for the draft port and would be needed before claiming
full parity with the Rust crate:

- **No WebSocket upgrade.** The Rust server can also upgrade to WebSocket; this
  package serves HTTP + SSE only.
- **No CORS handling.** Browser cross-origin access is not configured.
- **One stream per connection.** The Rust crate can open a distinct event
  stream per session and route session-scoped frames to it. This package keeps a
  single connection-level stream; the session header is validated but routing is
  connection-wide.
- **No JSON-RPC batch bodies.** A `[`-prefixed body is rejected with `400`; the
  draft crate accepts batches.
- **Connections live until the agent exits.** There is no idle timeout, so a
  server must call `Server.Close` (or shut the process down) to release them.

## Validation

`transport/http/http_test.go` runs a real v1 agent through the binding over an
`httptest` server: initialize is answered inline, `session/new` and
`session/prompt` are `POST`ed, the agent's `session/update` is delivered on the
event stream, and the prompt completes with the agent's stop reason. Separate
tests cover the status-code error paths and the stream `Accept` requirement.
