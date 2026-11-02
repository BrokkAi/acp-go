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
Session archive (ArchiveSession, UnarchiveSession, and ListSessions with the
archived filter), status (SessionStatus), and title (SetSessionTitle) follow
the same rule.

Capability gating reads the unstable draft-v2 initialize response, which is the
only place the optional capability fields are published. The stable
v2.Initialization drops them, so initialize with
v2.Connection.InitializeExtended, which validates the result and records the
connection state like InitializeWithInfo and also decodes it into the unstable
type:

	var initialization unstable.InitializeResponse // schema/v2/unstable
	_, err := connection.InitializeExtended(ctx, capabilities, info, &initialization)
	providers, err := acpunstable.ListProviders(ctx, connection, initialization)

Do not send initialize through Connection.Call instead: that skips the version
and agent-info checks and leaves the connection uninitialized for the stable
facades.

On the agent side, Handle dispatches these methods to typed handlers and
Advertise announces their capabilities in the initialize response. Install
sets both on the v2/agent runtime, together with the runtime overrides the
facades need, so that the runtime's initialization gate, version negotiation,
and baseline methods sit in front of them, as in the reference SDK:

	runtime := agent.New(implementation)
	if err := acpunstable.Install(runtime, implementation); err != nil {
		return err
	}

For an ArchiveHandler, Install also serves session/list through
Runtime.Overrides, because the runtime's stable dispatch cannot carry the
archived filter or SessionInfo.archived, and only Install announces
capabilities.session.archive.
*/
package unstable
