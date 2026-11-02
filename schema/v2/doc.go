// Package v2 provides Go types for every message in the draft Agent Client
// Protocol v2 schema, generated from the official JSON Schema artifacts.
//
// ACP v2 is a parallel wire protocol, not an extension of v1 and not a
// drop-in replacement for the stable schema package. It is pinned separately
// here so breaking draft changes can be tracked explicitly without changing
// v1 callers. Do not convert messages implicitly between protocol versions:
// applications must select and negotiate one version explicitly.
//
// Regenerate this package after changing its pin:
//
//	go run ./cmd/acpgen -schema schema/v2/schema.json \
//	  -meta schema/v2/meta.json -out schema/v2 -package v2
//
// Union and enum semantics match the v1 generated package: unrecognized
// discriminators are preserved in Other variants, future enum values decode,
// and ambiguous or empty tagged unions fail to marshal.
//
// A union flattened into its parent object is generated as plain fields on
// the parent. IdleStateUpdate carries the stopReason discriminator as an
// open StopReason string (custom and future values decode unchanged, with
// their other fields in StopReasonFields) and the error variant's payload as
// its Error field, encoded only with StopReasonError. The state updates read
// a malformed stopReason, error, background, or _meta as not reported, as
// Rust does; elsewhere decoding is still strict (#82). Untyped "any other
// value" arms, such as ErrorData's Other, keep the raw JSON verbatim, and a
// decoded ErrorData variant re-encodes its original bytes until changed.
//
// ACP v2 patch fields distinguish an omitted property from JSON null. Those
// fields use Nullable[T], whose Set, Null, and Value members preserve all
// three wire states: omitted, explicit null, and a concrete value.
// Integer formats retain their JSON Schema wire width; ProtocolVersion is a
// uint16 and ErrorCode is an int32.
//
// This package is the stable draft surface only. ACP v2's optional
// ("unstable") features live in the opt-in schema/v2/unstable package and are
// not re-exported here.
package v2
