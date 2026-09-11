// Package unstable provides the complete v1 schema surface generated from Rust
// schema crate 1.7.0's unstable artifacts. It is an explicit import boundary
// equivalent to enabling Rust's optional schema-crate features; none of these
// types are exposed by the stable schema package.
//
// The unstable artifact includes LLM providers, MCP-over-ACP, NES, session
// fork, document events, and other feature-gated surfaces. It is a collective
// opt-in because the published unstable JSON artifacts combine all optional
// Rust features and do not identify feature ownership.
//
// Regenerate with:
//
//	go run ./cmd/acpgen \
//	  -schema schema/unstable/schema.json \
//	  -meta schema/unstable/meta.json \
//	  -out schema/unstable \
//	  -package unstable
package unstable
