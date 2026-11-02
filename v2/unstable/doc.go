/*
Package unstable is the draft-v2 counterpart to
github.com/BrokkAi/acp-go/unstable: the import boundary for narrow, opt-in
typed facades over ACP's optional draft-v2 methods. Nothing here is reachable
from the stable v2 facade.

The client facades cover agent provider configuration (ListProviders,
SetProvider, DisableProvider) and session fork (ForkSession, the counterpart of
the reference SDK's v2::ForkSessionBuilder). Each checks its advertised
capability and request invariants before writing to the wire. Forks that carry
MCP servers go through github.com/BrokkAi/acp-go/v2/mcp.ForkSession instead.

Capability gating reads the unstable draft-v2 initialize response, which is the
only place the optional capability fields are published. The stable
v2.Initialization drops them, so decode the agent's initialize result into the
unstable type:

	var initialization unstable.InitializeResponse // schema/v2/unstable
	json.Unmarshal(encodedInitializeResult, &initialization)
	providers, err := acpunstable.ListProviders(ctx, connection, initialization)

On the agent side, Handle composes typed handlers for these methods with any
acp.Handler. The v2/agent runtime serves only the stable baseline, and its
stable initialize response cannot advertise the unstable capabilities, so an
agent that serves these methods hosts its own connection: it answers
initialize with an unstable InitializeResponse that advertises them and wraps
its handler with Handle:

	handler := acpunstable.Handle(baseline, implementation)
	connection := acp.Connect(stdin, stdout, handler, notifications)
*/
package unstable
