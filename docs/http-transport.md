# Draft Streamable HTTP transport

`github.com/BrokkAi/acp-go/transport/http` (package `acphttp`) is an explicit,
opt-in binding for the ACP *Streamable HTTP* transport, over HTTP + SSE and over
WebSocket. The ACP specification
still lists that transport as a draft proposal in progress, so this package is
gated three ways:

1. **Import boundary.** Nothing in the SDK depends on it; a caller must import
   `transport/http` to use it.
2. **No fallback.** The package never falls back to stdio, and `Dial` never
   retries a different transport. The caller picks one binding.
3. **Stdio stays the default.** `acp.Connect` and the `agent`/`runner` packages
   are unchanged.

The wire shape mirrors the reference Rust crate
`agent-client-protocol-http` 5.0.0 and the transport RFD
(`streamable-http-websocket-transport`); status codes, bodies, and headers
follow the Rust server.

The package uses the standard library only, as the rest of the SDK does: the
WebSocket handshake goes through `net/http` (`Hijack` on the server, an
`http.Client` upgrade on the client), and the RFC 6455 framing is implemented in
the package. The dependency policy in `licenses/README.md` is unchanged.

## Binding

| Element | Behavior |
|---|---|
| Endpoint | `ServerOptions.Path` (default `/acp`). `Dial` accepts a base URL and appends `/acp` when the path is empty or missing it. |
| `POST` initialize | `Content-Type: application/json`. Every `initialize` request creates a connection, whatever `Acp-Connection-Id` says. The server answers it in the body with `200` and returns the connection id in `Acp-Connection-Id`. An error response is returned with `200` and no connection id, and the connection is discarded. |
| `POST` later frames | Also `application/json`, with `Acp-Connection-Id`; queued for the agent and answered `202 Accepted` with an empty body, and the response arrives on a stream. |
| Session identity | A session is named by the `Acp-Session-Id` header or by `params.sessionId`. The header is written into `params.sessionId` before the frame reaches the agent; a conflicting or non-string `params.sessionId`, or non-object params, is a `400`. The session-scoped methods the Rust crate lists (`session/prompt`, `session/cancel`, `session/close`, `session/delete`, `session/fork`, `session/load`, `session/resume`, `session/set_config_option`, `session/set_mode`, `session/set_model`) need one or the other. `$/cancel_request` is connection-scoped and never takes the header. |
| `GET` stream | `Accept: text/event-stream` with `Acp-Connection-Id` opens the connection stream; adding `Acp-Session-Id` opens that session's stream. Frames arrive as `data:` events, with an empty comment every 15 idle seconds. |
| `GET` upgrade | A WebSocket upgrade opens a connection that carries every frame over one socket; see [WebSocket](#websocket). |
| `DELETE` | With `Acp-Connection-Id`, stops the agent, ends the connection's streams after what is already queued, and answers `202`. |
| Health | `GET /health` answers `ok` unless `ServerOptions.DisableHealth` is set. |

### Status codes

| Request | Status |
|---|---|
| `POST` without `application/json` | `415` `Content-Type must be application/json` |
| `POST` body over the limit, or a `Content-Length` declaring more | `413` `POST body too large` |
| `POST` JSON array | `400` with the JSON-RPC batch rejection body |
| `POST` without `Acp-Connection-Id` (not `initialize`) | `400` `Acp-Connection-Id header required` |
| `POST`, `GET`, or `DELETE` for an unknown connection | `404`, empty body |
| `POST` of a session-scoped method with no session | `400` `Acp-Session-Id header required` |
| `POST` once the agent has closed its input or returned | `500`, empty body |
| `POST` body that is not valid UTF-8 | `400` `Invalid JSON-RPC: ` followed by the text of Rust's `Utf8Error`, such as `invalid utf-8 sequence of 1 bytes from index 0` |
| `initialize` when the agent exits first | `500` `agent closed before initialize response` |
| `GET` without the event-stream `Accept` | `406` `client must accept text/event-stream` |
| WebSocket upgrade with an `Origin` the CORS policy does not allow | `403` `WebSocket origin not allowed` |
| `GET` or `DELETE` without `Acp-Connection-Id` | `400` `Acp-Connection-Id header required` |
| `GET` of a stream that already has a subscriber | `409` `outbound stream already has a subscriber` |
| `GET` of an unknown session once 16 unconfirmed streams are open | `404` `unknown Acp-Session-Id` |
| Another method on the endpoint | `405` with `Allow: POST,GET,HEAD,DELETE` |
| Any other path | `404`, empty body |

## Streams

Each connection has one connection stream and one stream per session, and
each stream has at most one subscriber. Frames are queued per stream without a
bound until the client reads them, so nothing the agent sends is lost before
the client opens the stream: not the notifications or callbacks it sends
before answering `initialize`, and not the updates it sends right after
`session/new`. A subscriber that leaves keeps what it did not read for the
next one.

The server only keeps streams for sessions that exist on the connection. A
session exists once the agent names it, following the Rust crate (rust-sdk
#483):

- in a `session/new` or `session/fork` result's `sessionId`,
- in a successful response to a request scoped to the session (such as
  `session/load` or `session/resume`),
- in a request or notification the agent sends with that `params.sessionId`.

The session is recorded before the response that names it is queued, so a
stream opened right after that response finds it.

Routing:

- The agent's requests and notifications with a `params.sessionId` go to that
  session's stream; everything else goes to the connection stream.
  `$/cancel_request`, even wrapped in `proxy/successor` envelopes, and other
  `$/` methods are connection-scoped.
- A response goes to the stream of the session its request named, if that
  session has a stream when the agent answers; otherwise to the connection
  stream. Sending a session-scoped request does not create a stream, so the
  response to `session/new`, or to a request for a session without a stream,
  arrives on the connection stream.
- A client may open up to 16 streams for sessions not named yet, so it sees
  the updates `session/load` replays. Each is forgotten when the client closes
  it before the agent names the session. Beyond that, an unknown session gets
  `404`.
- A successful `session/close` or `session/delete` removes the session's
  stream. A client still reading it receives the response there; otherwise the
  response goes to the connection stream.

The client follows the Rust client: it opens the connection stream after
`initialize`, opens a session's stream before it sends the first message that
names the session, and opens one when a `session/new` or `session/fork` result
names a new session. `Close` sends `DELETE` before it stops the streams.

## Server options and CORS

`acphttp.ServerOptions` is ready to use as its zero value, which matches the
Rust crate's `ServerOptions::default()`:

| Field | Default | Meaning |
|---|---|---|
| `Path` | `/acp` | The ACP endpoint. It must start with `/`, and can only be `/health` with the health endpoint disabled. |
| `CORS` | disabled | The browser origin policy. |
| `DisableHealth` | `false` | Turns off `GET /health`. |
| `MaxBodyBytes` | 16 MiB | The `POST` body limit. |

On the HTTP `POST` path, the body limit is the only frame-size limit, as in
the Rust crate: the agent's frames, its event-stream events, and the
`initialize` response the client reads may be of any size. The WebSocket
binding instead has tungstenite's limits: 16 MiB per frame and 64 MiB per
message.

`acphttp.NewServer` validates the options; a `Server` built as a literal with
invalid options answers every request with `500`.

`CORSOptions` is opaque and built with `CORSDisabled()` (the zero value),
`AllowOrigins(origins...)`, which returns an `*InvalidOriginError` naming an
origin that is not a valid header value, or `AllowAnyOrigin()`. When enabled,
the server behaves like the Rust crate's tower-http layer:

- Every `OPTIONS` request is a preflight, answered `200` with
  `Access-Control-Allow-Methods: GET,POST,DELETE,OPTIONS` and
  `Access-Control-Allow-Headers: content-type,accept,acp-connection-id,acp-session-id,sec-websocket-version,sec-websocket-key,connection,upgrade`.
- Other responses carry
  `Access-Control-Expose-Headers: acp-connection-id,acp-session-id`.
- An allowlist echoes a listed `Origin` in `Access-Control-Allow-Origin` and
  sends `Vary: origin`; `AllowAnyOrigin` sends `*`.

CORS is not authentication. The built-in policy does not allow `Authorization`
and never sends `Access-Control-Allow-Credentials`; hosts that need a
credential policy keep CORS disabled here and wrap the server with their own.
The same policy is the origin check for WebSocket upgrades, even when a host
supplies its own HTTP CORS layer: a request without `Origin` passes, a
disabled policy rejects any `Origin` with `403`, and an allowlist must match
it exactly.

## WebSocket

A `GET` of the endpoint that asks for a WebSocket upgrade opens a connection
that carries every frame both ways over one socket, as the Rust crate's
`GET /acp` WebSocket upgrade does. Requests and responses carry no connection
or session headers after the handshake.

### Handshake

The server takes the upgrade when the request passes the checks of axum's
`WebSocketUpgrade` extractor, which the Rust crate tries first:

- an HTTP/1.1 `GET` (`HEAD`, HTTP/1.0, and HTTP/2 requests are served as
  ordinary `GET`s),
- a first `Connection` header that contains `upgrade` in any case, as a
  substring,
- a first `Upgrade` header equal to `websocket` in any case,
- a `Sec-WebSocket-Key` header of any value,
- a first `Sec-WebSocket-Version` header equal to `13`,
- a response writer that can be hijacked.

A request that fails one of them is not an upgrade: it is served as the
event-stream `GET`, so it gets `406`, `400`, or the event stream exactly as the
Rust crate answers it. An upgrade then passes the origin check: a request with
an `Origin` header that `ServerOptions.CORS` does not allow gets
`403 WebSocket origin not allowed`. A request without `Origin` always passes,
so the check is not authentication.

The server answers `101 Switching Protocols` with the headers the Rust server
writes, in its order and case: `connection: upgrade` (`upgrade, close` when
the request's `Connection` lists `close`, as hyper writes it),
`upgrade: websocket`, `sec-websocket-accept`, `acp-connection-id` with the new
connection's id, the CORS headers when CORS is enabled, and `date`. It selects
no subprotocol and no extension, whatever the request offers. A request body,
which hyper reads before it hands the connection over, is read and discarded
first, so it is never taken for frames; it is not buffered, and like the
Rust crate the server does not bound its size. Each upgrade starts one
`Entry.Serve`; the agent sees no initialize handling of its own, so it can
send frames before the client's first message.

`Dial` accepts `ws://` and `wss://` endpoints, with the same `/acp` path rule
as HTTP. Like the Rust client, it sends the handshake before anything else
and checks the answer before any ACP message goes out, failing with
`WebSocket connect failed: ` and the reason:

- the handshake is a `GET` of the `http://` or `https://` form of the
  endpoint, with `Connection: Upgrade`, `Upgrade: websocket`,
  `Sec-WebSocket-Version: 13`, a random 16-byte `Sec-WebSocket-Key`, and
  reqwest's defaults: `Accept: */*`, no `User-Agent`, and no
  `Accept-Encoding`;
- it goes through a copy of `Client.HTTP` (or `http.DefaultClient`) that
  follows no redirects and, for an `*http.Transport`, speaks only HTTP/1.1
  and requests no compression. These are applied after the caller's
  configuration, as the Rust client applies them after `configure_http`.
  Proxies, TLS settings, and dialers of the caller's transport still apply;
- `Client.HTTP.Timeout` bounds the handshake only, as reqwest's request
  timeout covers the opening handshake but not the upgraded socket;
- a response that is not HTTP/1.1 fails with
  `expected HTTP/1.1, received HTTP/2.0; preconfigured TLS must use HTTP/1.1 ALPN`,
  a status other than `101` with `unexpected status ` and the status, such as
  `301 Moved Permanently`, then `invalid upgrade header` (not exactly one
  `Upgrade: websocket`), `invalid connection header` (no `upgrade` token),
  `invalid Sec-WebSocket-Accept` (not exactly one, matching value), and
  `unsupported sec-websocket-protocol` or
  `unsupported sec-websocket-extensions` for a response that has either
  header;
- a refused handshake closes its connection.

`Dial`'s context bounds the handshake and the connection's whole life, as it
bounds the HTTP binding's requests, so pass a context that lives as long as
the connection and bound the handshake with `Client.HTTP.Timeout`:

```go
client := acphttp.Client{Endpoint: "wss://agent.example.com", HTTP: &http.Client{Timeout: 10 * time.Second}}
connection, err := client.Dial(context.Background(), clientHandler, notifications)
```

Neither side sets a write deadline on the socket, as the Rust crate's sends
wait without one: a peer that stops reading stalls the writer until the
socket fails or is closed. Any scheme other than `http`, `https`, `ws`, and
`wss` is refused with
`unsupported URL scheme: <scheme>; expected http, https, ws, or wss`. The
scheme picks the binding, and the transport never falls back from one to the
other.

### Frames

The SDK implements RFC 6455 framing itself, as the Rust crate gets it from
tungstenite with its default configuration, so both sides take and refuse the
same frames:

- Every message is one text frame holding one JSON-RPC message, sent
  unfragmented with the shortest length form. A client masks each frame with
  a fresh random key; the server does not mask.
- Fragmented messages are reassembled, with control frames between
  fragments. A frame may hold 16 MiB and a message 64 MiB: a longer frame
  fails as soon as its header is read, and a longer message at the fragment
  that crosses the limit.
- A text message must be UTF-8; a fragmented one is checked fragment by
  fragment.
- Binary frames are ignored, and so are pongs. A ping is answered with a
  pong carrying its payload.
- A server frame must not be masked and a client frame must be. Reserved
  bits, reserved opcodes, a continuation without a message, a new message
  inside a fragmented one, and a fragmented or over-125-byte control frame
  are protocol violations.

On the server, a text frame goes to the agent as a `POST` body does: a
JSON-RPC message re-serialized as the Rust crate serializes it, and anything
else made one line, so the agent answers a frame that is not JSON with
`-32700` and one that is not a message with `-32600`. A JSON array, including
`[]`, is answered with one `-32600`/`id: null` text frame, and none of its
entries is forwarded; the socket stays open. The `Acp-Session-Id` rules do not
apply to frames, so a session method without a session is forwarded as is.
Every agent line goes to the socket, a JSON-RPC message re-serialized and
anything else verbatim, in the order the agent wrote it.

On the client, every frame the connection writes is one text frame, a
JSON-RPC message re-serialized and anything else verbatim, with no
first-message rule. A text frame received is handed to the connection as an
HTTP event is: a JSON-RPC message re-serialized, anything else made one line,
and a frame of several lines that is not JSON answered with the parse error
the connection would send. An array is malformed.

### Closing

Neither side answers a close frame, and the server never sends one, matching
the Rust server and client, which drop the socket instead:

- The server ends the connection at once, and stops the agent, when the
  client sends a close frame, breaks the protocol, or the socket fails or
  ends. As in the Rust crate, a message that arrived together with the close
  frame or the violation, so that both were read at once, does not reach the
  agent: messages are handed over only once no further frame is already
  buffered.
- When the agent finishes, or `DELETE` (with the connection id from the
  `101`) or `Server.Close` ends the connection, the server writes what the
  agent sent and closes the socket.
- When the agent stops reading, the next text frame stops the server reading
  the socket, which still delivers what the agent sends until it finishes.
- The client's `Close` sends a close frame without a payload and closes the
  socket without waiting.
- A close frame from the server fails the client with
  `WebSocket closed by peer: ` and the frame as Rust debug-prints it, such as
  `None` or `Some(CloseFrame { code: Normal, reason: Utf8Bytes(b"bye") })`.
  A code that may not be received (1005, 1006, 1015, reserved, and out of
  range codes) reads as `Some(CloseFrame { code: Protocol, reason: Utf8Bytes(b"Protocol violation") })`.
- Any other failure fails the client with `ws recv: ` or `ws send: ` and
  tungstenite's text, such as
  `ws recv: WebSocket protocol error: Connection reset without closing handshake`,
  `ws recv: Space limit exceeded: Message too long: 16777217 > 16777216`, or
  `ws recv: UTF-8 encoding error: invalid utf-8 sequence of 1 bytes from index 6`.
  Later writes fail with the same error.

### Connection id

The WebSocket connection is registered under the id the `101` returns, as in
the Rust crate, so HTTP requests can name it:

- a `POST` with `Acp-Connection-Id` is forwarded to the agent with the HTTP
  session header rules and answered `202`, and the agent's response arrives
  on the socket;
- an event-stream `GET` gets `409 outbound stream already has a subscriber`,
  or `404 unknown Acp-Session-Id` with `Acp-Session-Id`;
- `DELETE` ends the connection as above;
- an `initialize` `POST` opens a new HTTP connection, as it always does.

Cancellation can be sent at any point after the socket is open, including
while `initialize` is in flight, since the socket carries frames in order
from the start; with HTTP + SSE it needs the connection id that `initialize`
returns.

## JSON-RPC batches

JSON-RPC batches are not supported, matching the ACP v2 transports rule and the
Rust crate's "JSON-RPC Batches" section. They are rejected on both protocol
versions:

- A `POST` body that is a JSON array, including `[]`, gets `400 Bad Request`
  with `Content-Type: application/json` and a single JSON-RPC error body:
  `-32600` (`Invalid request`) with `id: null` and the data
  `"JSON-RPC batches are not supported; send each message individually"`.
  None of its entries is forwarded, and no connection is created, whatever
  `Acp-Connection-Id` says.
- The client sends every message individually. A JSON array received from the
  server is malformed: on the event stream the connection answers it with the
  same error and runs none of its entries, and as an `initialize` response
  body it fails the initialize.

## Bodies that are not JSON-RPC messages

A body that the Rust SDK cannot read as one JSON-RPC message is forwarded to
the agent as the Rust crate forwards its malformed frames. That covers bodies
that are not valid JSON and valid JSON that is not a message: a value that is
not an object, a missing or wrong `jsonrpc`, a method that is not a string,
params that are not an object or an array, an id that is not null, a string,
or an integer, or a response without exactly one of `result` and a valid
`error`. Validity follows `serde_json`, as in the Rust crate, so `[1e400]` is
not valid JSON rather than a batch.

- Without `Acp-Connection-Id`, such a body gets
  `400 Acp-Connection-Id header required`; on an unknown connection, `404`.
  It never creates a connection, and no route or session header applies to
  it.
- On a live connection it is forwarded and answered `202`, and the agent
  answers on the connection stream: a body that is not valid JSON gets
  `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error","data":{"line":"<body>"}}}`,
  and valid JSON that is not a message gets `-32600` with `id: null`.
- A body on one line is forwarded verbatim. The agent reads one line per
  frame, so a body that spans lines is made one line first, as the Rust
  crate's line writer does: JSON that `serde_json` reads is written again as
  `serde_json` writes it, and anything else is sent as a JSON string of the
  body, which the agent answers with `-32600`.
- The client fails an `initialize` whose response body is not valid JSON or
  not a JSON-RPC message with the Rust client's message, such as
  `initialize: invalid initialize response: Invalid request`, and its
  connection answers an event that is not valid JSON with the parse error.

## Message encoding

Both sides hold a JSON-RPC message as the Rust SDK's `RawJsonRpcMessage`
and send it as `serde_json::to_string` writes it, so the bytes match the Rust
crate's:

- Members are written in the order `jsonrpc`, `id`, `method`, `params` for a
  request or notification and `jsonrpc`, `id`, `result` or `error` for a
  response. Other members of the message are dropped, and so is a `null`
  `params`. An error object is written as `code`, `message`, `data` when
  present (`null` included), then its other members in order.
- Values are written as `serde_json` writes them: escapes are decoded and
  only the escapes `serde_json` needs are written, the last of duplicate
  members wins at the position of the first, an integer that fits `u64` or
  `i64` stays as written, and every other number is an `f64` in
  `serde_json`'s form, such as `1e2` as `100.0`.
- The `Acp-Session-Id` header is appended to `params` as `sessionId` when
  `params` has none.
- The server writes each agent line it reads, a line ending at `\n` or `\r\n`,
  that way when it is a JSON-RPC message. Any other line, an empty or blank
  one included, goes to the connection stream verbatim. Only a valid response
  to the `initialize` request answers the `initialize` `POST`; a line the Rust
  SDK cannot read goes to the connection stream instead.
- Event data is written as axum writes it: `data: ` and the frame, a new
  `data: ` field after each `\r` or `\n` in it, and a blank line; an empty
  frame is a blank line alone.
- The client posts every message it sends that way, and a frame its
  connection writes that is not one JSON-RPC message verbatim, with no
  session header. It reads event streams as the Rust client's parser does:
  lines end at `\r\n`, `\r`, or `\n`, a byte order mark and comments are
  skipped, `data` fields are joined with `\n`, and an event with empty data
  is skipped. Bytes that are not UTF-8 stall the stream, and when it ends it
  fails with the UTF-8 error.
- An agent line that is not UTF-8 ends the connection as the Rust line
  reader's error does: what the agent sent before it is delivered, the
  streams end, the agent's input closes, and later requests get `404`.

## Client errors

The client fails with the Rust client's error data: `initialize: ` and the
reason for a failed `initialize`, `POST: ` and the reason for a failed
`POST`, such as `POST: HTTP 500 Internal Server Error: ` and the body,
``ACP HTTP transport: first message must be `initialize` `` for a first frame
that is not an `initialize` request, and
``POST: method `session/prompt` requires sessionId in params`` for a session
method without a session. A stream opens on any `2xx` status; one that cannot
open or that ends fails the transport with `connection SSE stream ended: ` or
`<session id> SSE stream ended: ` and the reason, such as
`HTTP 404 Not Found` or `SSE stream closed`, and later frames fail with the
same error.

## Client initialize

The client sends `Accept: application/json` on every `POST` and reads the
`initialize` response body whole. Like the Rust client, it stores the
`Acp-Connection-Id` of the response before it checks the status and body, so
a status other than `2xx` or a body that is not a JSON-RPC message fails the
initialize and sends `DELETE` for that connection. An error response rejects
the connection whatever the headers say: the error is delivered to the
caller, the connection is deleted if the response named one, and the next
frame must be `initialize` again. A successful response without the header
fails the initialize. Later `POST`s accept `202` or any other `2xx` status.

## Usage

Server side, serve any existing agent runtime over HTTP:

```go
acpServer, err := acphttp.NewServer(agent.New(myAgent), acphttp.ServerOptions{})
if err != nil {
	return err
}
defer acpServer.Close()
http.ListenAndServe(":8080", acpServer)
```

To allow a browser origin:

```go
cors, err := acphttp.AllowOrigins("http://localhost:5173")
if err != nil {
	return err
}
acpServer, err := acphttp.NewServer(agent.New(myAgent), acphttp.ServerOptions{CORS: cors})
```

Client side, dial and then drive the connection exactly like stdio:

```go
connection, err := acphttp.Dial(ctx, "https://agent.example.com", clientHandler, notifications)
initialization, err := connection.InitializeWithInfo(ctx, capabilities, info)
```

The same call with a `ws://` or `wss://` URL opens one WebSocket instead:

```go
connection, err := acphttp.Dial(ctx, "wss://agent.example.com", clientHandler, notifications)
```

The server needs nothing more: the `Server` that serves HTTP + SSE also takes
WebSocket upgrades on the same endpoint.

Both `github.com/BrokkAi/acp-go/agent.Runtime.Serve` and
`.../v2/agent.Runtime.Serve` satisfy `acphttp.Entry`, so the same agent
implementation can be served over stdio or HTTP without changes.

## Deliberate limits

These are out of scope for this package today:

- **WebSocket over HTTP/1.1 only.** As in the Rust crate, there is no HTTP/2
  (RFC 8441) or HTTP/3 WebSocket, and no subprotocol or extension, including
  compression. A client handshake through a custom `http.RoundTripper` is sent
  as that round tripper sends it, and fails unless the answer is an HTTP/1.1
  `101` whose body can be written.
- **Connections live until the agent exits.** There is no idle timeout, so a
  server must call `Server.Close`, or the client `DELETE`, to release them.

## Validation

`transport/http/http_test.go` runs a real v1 agent through the binding over an
`httptest` server: initialize is answered inline, `session/new` and
`session/prompt` are `POST`ed, the agent's `session/update` is delivered on the
session stream the client opens, and the prompt completes with the agent's stop
reason. `server_test.go` drives a scripted agent through per-session and
connection routing, buffering before a stream opens, the `409` second
subscriber, the 16 unconfirmed streams and `404`, stream removal on
`session/close` and `session/delete`, session header rules, `DELETE`, the
initialize outcomes, CORS preflight and the origin policy, health, and the
body limit, invalid UTF-8, and an agent that stops reading.
`malformed_test.go` covers batches and bodies that are not JSON-RPC messages.
`streams_test.go` covers the routing table itself, and
`server_internal_test.go` a stream whose write fails and the UTF-8 error text.
`client_test.go`, `client_initialize_test.go`, and `client_internal_test.go`
cover the client's stream lifecycle and `DELETE`, the initialize outcomes, the
`Accept` header, a large `initialize` body, and a failed `session/new`
`POST`.

The WebSocket binding is checked against the Rust crate itself.
`testdata/websocket_rust_server.json` records 141 raw exchanges with the Rust
`AcpHttpServer` serving a `Lines` agent: handshake variants (each extractor
check, `HEAD`, HTTP/1.0, subprotocols and extensions offered, and the origin
policy disabled, wildcard, allowed, denied, and absent), frames of every
length form, fragmentation and interleaved control frames, invalid UTF-8,
masking, reserved bits and opcodes, oversized frames and messages, binary
frames, pings and pongs, close frames with every class of code, batches,
malformed frames, and `POST`, `GET`, and `DELETE` with the connection id.
`websocket_rust_parity_test.go` replays them against `acphttp.Server` and
requires the same 101 response, frames, socket ending, HTTP answers, and agent
input. `testdata/websocket_rust_client.json` records 81 scripted servers
against the Rust `HttpClient` on a `ws://` URL (handshake answers, frames,
close frames, and protocol violations), and `websocket_client_parity_test.go`
requires the same handshake request headers, frames, delivered lines, and
error text from the Go client. `websocket_test.go` and
`websocket_internal_test.go` run real v1 and v2 agent runtimes over `ws://`
and `wss://` (against a server that also offers HTTP/2), cancellation before
the `initialize` response, `Server.Close`, refused schemes and handshakes, and
the codec's masking, length forms, fragmentation, size limits, UTF-8 checks,
and close frames.
