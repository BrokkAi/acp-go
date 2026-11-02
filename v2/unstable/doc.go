/*
Package unstable is the draft-v2 counterpart to
github.com/BrokkAi/acp-go/unstable: the import boundary for narrow, opt-in
typed facades over ACP's optional draft-v2 methods. Nothing here is reachable
from the stable v2 facade.

The package has no facades at the moment. Its only one, Next Edit Suggestions,
was removed with the rest of NES from the upstream unstable schema (Rust schema
crate 1.15.1). Until new facades land here, use the generated draft-v2 unstable
bindings in github.com/BrokkAi/acp-go/schema/v2/unstable directly. Capability
gating reads the unstable draft-v2 initialize response, which is the only place
the optional capability fields are published:

	var initialization unstable.InitializeResponse // schema/v2/unstable
	json.Unmarshal(encodedInitializeResult, &initialization)
*/
package unstable
