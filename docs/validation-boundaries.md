# Validation boundaries

Rust's reference SDK enforces semantic newtypes — identifiers, absolute paths,
media types, URI forms — at the type boundary. Go cannot do that on the
generated types without reflection-heavy custom decoders, which would also break
the open-world decoding rules. This page records where `acp-go` validates
instead.

## Decision

Validation happens at the **typed facade boundaries**: the client methods that
build requests. It never happens inside the generated schema packages, and it
never uses reflection over struct fields.

- Generated types stay permissive, so unknown union tags, extension fields, and
  raw `_meta` payloads keep decoding exactly as the pinned schema describes.
- Facades validate the values they are about to send, before anything reaches
  the wire, so a bad request fails fast with a clear message instead of becoming
  a JSON-RPC round trip.
- Validators are plain string checks in `internal/acpvalidate` with no
  dependencies and no reflection; each facade adds only the checks its own
  request shape needs.

## What is enforced, and where

| Invariant | Boundary |
|---|---|
| Absolute workspace paths (`cwd`, additional directories, list `cwd`) | v1 `session.go`, draft-v2 client |
| Required identifiers (session IDs, client info, configuration IDs and values) | v1 `client.go`/`session.go`, draft-v2 client and `SessionHandle` |
| MCP server transport selection with required name/command/URL | v1 `session.go`, draft-v2 client |
| Discriminated-union arity (exactly one variant set) | v1 prompt facade, draft-v2 prompt and handle validation, generated marshalers |
| Media types on image, audio, and embedded content | v1 and draft-v2 prompt facades |
| Absolute URIs on resource links and embedded resources | v1 and draft-v2 prompt facades |
| Capability gates before writing | v1 and draft-v2 facades |

`acpvalidate.AbsolutePath` accepts a path that is absolute on either platform -
POSIX (`/dir`), Windows drive-absolute (`C:\dir` or `C:/dir`), or a Windows UNC
path - because the agent that resolves session paths may run on a different
platform than the client. Drive-relative (`C:dir`) and root-relative (`\dir`)
paths are still rejected. Host-local paths keep host-native `filepath.IsAbs`
checks: `clienthost` resolves them against the client filesystem, and the v2
runner uses one as the local agent's working directory.

`acpvalidate.MediaType` accepts RFC 6838 `type/subtype` with optional
`name=value` parameters. `acpvalidate.URI` requires an RFC 3986 scheme, and
`acpvalidate.Identifier` requires a non-blank value. All three report an error
naming the value, so tests can assert the message.

## What is deliberately not validated

- **Unknown enum values stay open-world.** `stopReason`, `toolKind`, and similar
  string types decode and re-encode values this release does not name.
- **Unknown union tags keep failing to decode.** The generated decoders reject
  an unrecognized `type`/discriminator; that is a schema contract, not a facade
  decision.
- **Received payloads are not exhaustively re-validated.** Facades check the
  invariants of the requests they send (and the identity of responses they
  return) rather than sanitizing everything an agent may send back.
- **Media type parameters and URI structure stay shallow.** Values after the
  media type's `;` are checked for `name=value` shape, and URIs are checked for
  a scheme; neither is parsed further. `tracestate` and `baggage` remain opaque.

## Behavior guarantees kept

- Raw `_meta` payloads round-trip unchanged: `schema.TestMetaPayloadsRoundTrip`,
  and the draft-v2 `Nullable[Meta]` case in `tracecontext`.
- Unknown enum values decode and re-encode: `schema.TestOpenWorldEnumsDecodeUnknownValues`.
- Unknown union tags are rejected: `schema/golden_test.go`.
