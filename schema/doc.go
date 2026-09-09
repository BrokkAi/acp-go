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
//   - Union variants with unrecognized tags decode into the Other variant,
//     preserving the raw payload for proxying.
//   - The JSON-RPC routing envelopes (AgentRequest, AgentNotification, and
//     friends) are not emitted; dispatch by method string and decode params
//     with the type named by the Methods registry.
package schema
