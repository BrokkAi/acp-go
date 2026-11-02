/*
Package acphttp is an explicit, opt-in transport for the draft Streamable HTTP
binding of ACP. It is not part of the stable surface and is not a default: the
SDK stays stdio-first, and a caller must import this package to use it.

The binding is the one the reference Rust SDK's `agent-client-protocol-http`
5.0.0 crate speaks, which the ACP specification still lists as a draft
proposal in progress:

  - `POST {endpoint}` with `Content-Type: application/json` and an `initialize`
    request creates a connection. The server answers the request with the
    JSON-RPC response in the body and returns the connection id in the
    `Acp-Connection-Id` header. The default endpoint path is `/acp`.
  - Later frames (requests, notifications, and responses) are also `POST`ed with
    the `Acp-Connection-Id` header and answered with `202 Accepted`; their
    responses are delivered on a stream.
  - `GET {endpoint}` with `Accept: text/event-stream` and the
    `Acp-Connection-Id` header opens the connection stream; adding
    `Acp-Session-Id` opens that session's stream. Agent frames are buffered per
    stream until the client opens it, and each session's frames go to its own
    stream once the agent has named the session.
  - Session-scoped methods carry the `Acp-Session-Id` header or
    `params.sessionId`, matching the set of methods that require it in the
    Rust crate.
  - `DELETE {endpoint}` with `Acp-Connection-Id` tears the connection down.

ServerOptions configures the path, a CORS policy (disabled by default), the
`/health` endpoint (enabled by default), and the 16 MiB POST body limit. Its
zero value is the default.

JSON-RPC batches are not supported: a JSON array POST body gets `400 Bad
Request` with a single JSON-RPC `-32600` error with `id: null`, and creates no
connection. A body that is not a JSON-RPC message, valid JSON or not, cannot
open a connection; on a live connection it is forwarded as the Rust crate
forwards malformed frames, and the agent answers it on the stream with a
-32700 parse error or a -32600 invalid request. An array or invalid JSON
received from the server is treated as malformed.

Both sides send a JSON-RPC message as the Rust SDK serializes its
RawJsonRpcMessage, so the bytes on the wire match the Rust crate's: members in
a fixed order, other envelope members dropped, and values written as
serde_json writes them. The server forwards an agent line that is not a
message to the connection stream verbatim. See `docs/http-transport.md`.

WebSocket upgrade is not implemented yet; see `docs/http-transport.md` for the
remaining limits. This transport never falls back to stdio or between
transports: a caller chooses one binding explicitly.
*/
package acphttp
