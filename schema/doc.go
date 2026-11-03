// Package schema provides Go types for every message in Agent Client
// Protocol v1, generated from the official JSON Schema release.
//
// The pinned release lives in schema.json, schema/meta.json, and VERSION
// beside this package. Regenerate after pinning a new release:
//
//	go run ./cmd/acpgen -update <version>
//	go run ./cmd/acpgen
//
// Design notes:
//
//   - Discriminated unions (session updates, content blocks, auth methods,
//     …) are structs with one pointer field per variant. Exactly one variant
//     must be set; the Kind field is filled in on decode and derived from the
//     payload on encode.
//   - Enums are plain string or integer types with named constants, so
//     unknown future values decode without error, matching the protocol's
//     extensibility rules.
//   - Integer formats retain their JSON Schema wire width (for example,
//     ProtocolVersion is uint16 and ErrorCode is int32).
//   - Union variants with unrecognized tags decode into the Other variant,
//     preserving the raw payload for proxying.
//   - Encoding writes what the Rust schema crate writes with serde_json:
//     fields in Rust declaration order, None left out, defaults Rust always
//     writes filled in, and f64 values with a decimal point or exponent
//     (1.0, 1e+20). A union of primitives holds the Go value of the arm it
//     decodes as (string, int64, float64, bool, nil, or a typed slice). See
//     docs/schema-pin-workflow.md, Encode rules.
//   - The JSON-RPC routing envelopes (AgentRequest, AgentNotification, and
//     friends) are not emitted; dispatch by method string and decode params
//     with the type named by the Methods registry.
//
// This package is the stable surface only. ACP's optional ("unstable")
// features, including LLM providers, session fork, MCP-over-ACP, client
// system prompts, subagents, and proxy chains, live in the opt-in
// schema/unstable package and are not re-exported here.
package schema
