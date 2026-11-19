// Package unstable provides the complete draft-v2 schema surface generated
// from Rust schema crate 1.19.0's unstable artifacts. Importing this package is
// the Go equivalent of enabling Rust's separate unstable schema features; none
// of these types are exposed by the stable schema/v2 package.
//
// The unstable artifact includes feature-gated surfaces such as LLM providers,
// plan operations, session fork, archive, status, titles, options, and
// cursors, auth status, prompt suggestions, agent skills (skill commands and
// content blocks), the speed config option category, proxy chains
// (registered with SideProxy), MCP-over-ACP, and end-turn token usage.
// Client-provided system prompts (schema crate 1.16) and subagents (1.18)
// are part of the stable schema/v2 package too. It is a collective
// opt-in because the published unstable schema artifacts are generated with
// all optional Rust features enabled and do not carry per-feature ownership
// boundaries. Callers who need Rust's finer feature isolation can import only
// generated subpackages that deliberately expose narrower facades.
//
// Upserts carry the same ApplyUpdate and ApplyMetaUpdate helpers as schema/v2,
// including SubagentUpdate, SessionMessage, and CursorUpdate.
//
// Regenerate with:
//
//	go run ./cmd/acpgen \
//	  -schema schema/v2/unstable/schema.json \
//	  -meta schema/v2/unstable/meta.json \
//	  -out schema/v2/unstable \
//	  -package unstable
package unstable
