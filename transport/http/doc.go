/*
Package acphttp is an explicit, opt-in transport for the draft Streamable HTTP
binding of ACP. It is not part of the stable surface and is not a default: the
SDK stays stdio-first, and a caller must import this package to use it.

The binding is the one the reference Rust SDK's `agent-client-protocol-http`
crate speaks, which the ACP specification still lists as a draft proposal in
progress:

  - `POST {endpoint}` carries one JSON-RPC frame with `Content-Type:
    application/json`, only for clients that have not opened a connection yet.
    The server creates a connection, answers the `initialize` request with the
    JSON-RPC response in the body, and returns the connection id in the
    `Acp-Connection-Id` header. The default endpoint path is `/acp`.
  - Later frames (requests, notifications, and responses) are also `POST`ed with
    the `Acp-Connection-Id` header and answered with `202 Accepted`; their
    responses are delivered on the stream.
  - `GET {endpoint}` with `Accept: text/event-stream` and the
    `Acp-Connection-Id` header opens the server-to-client stream.
  - Session-scoped methods carry the `Acp-Session-Id` header, matching the set
    of methods that require it in the Rust crate.

Deliberate limits of this port, all recorded in `docs/http-transport.md`:
WebSocket upgrade, CORS, per-session streams, and JSON-RPC batch bodies are not
implemented. This transport also never falls back to stdio or between
transports: a caller chooses one binding explicitly.
*/
package acphttp
