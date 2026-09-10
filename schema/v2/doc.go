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
// ACP v2 patch fields distinguish an omitted property from JSON null. Those
// fields use Nullable[T], whose Set, Null, and Value members preserve all
// three wire states: omitted, explicit null, and a concrete value.
// Integer formats retain their JSON Schema wire width; ProtocolVersion is a
// uint16 and ErrorCode is an int32.
package v2
