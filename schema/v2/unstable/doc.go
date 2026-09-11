// Package unstable provides the complete draft-v2 schema surface generated
// from Rust schema crate 1.7.0's unstable artifacts. Importing this package is
// the Go equivalent of enabling Rust's separate unstable schema features; none
// of these types are exposed by the stable schema/v2 package.
//
// The unstable artifact includes feature-gated surfaces such as LLM providers,
// plan operations, session fork and compaction, session notices, NES,
// MCP-over-ACP, tool-call names, and end-turn token usage. It is a collective
// opt-in because the published unstable schema artifacts are generated with
// all optional Rust features enabled and do not carry per-feature ownership
// boundaries. Callers who need Rust's finer feature isolation can import only
// generated subpackages that deliberately expose narrower facades.
//
// Regenerate with:
//
//	go run ./cmd/acpgen \
//	  -schema schema/v2/unstable/schema.json \
//	  -meta schema/v2/unstable/meta.json \
//	  -out schema/v2/unstable \
//	  -package unstable
package unstable
