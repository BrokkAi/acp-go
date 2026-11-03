/*
Package unstable is the draft-v2 counterpart to
github.com/BrokkAi/acp-go/unstable: the import boundary for narrow, opt-in
typed facades over ACP's optional draft-v2 methods. Nothing here is reachable
from the stable v2 facade.

The client facades cover agent provider configuration (ListProviders,
SetProvider, DisableProvider), session fork (ForkSession, the counterpart of
the reference SDK's v2::ForkSessionBuilder), session archive (ArchiveSession,
UnarchiveSession, and ListSessions with the archived filter), status
(SessionStatus), title (SetSessionTitle), and auth status (AuthStatus, with
AuthStatusUpdates for the auth/status_update notification), and the session
lifecycle requests that carry a client-provided system prompt (NewSession,
ResumeSession, and ForkSession, with NewSystemPrompt and NewTextSystemPrompt).
Each checks its advertised capability and request invariants before writing
to the wire. Requests that carry MCP servers go through
github.com/BrokkAi/acp-go/v2/mcp instead.

Subagent sessions are baseline in draft v2 and need no capability.
SubagentTree tracks the parent/child tree, session-directed messages, and
child work state, mirrored between a child's own state_update and its
association, and permits only the child mutations an association
advertises; NotifySubagentUpdate, NotifySubagentState, NotifySessionMessage,
and NotifySessionMessageChunk send the surface from an agent.

Agents that advertise capabilities.session.systemPrompt serve session/new and
session/resume with the prompt through a SystemPromptHandler, which Install
routes to with runtime overrides (session/fork through ForkHandler). The
routes check each supplied prompt before the handler runs, against the
capability the connection's initialize response announced, and answer -32602
in the reference SDK's shape. Install announces the capability only for such
a handler.

Capability gating reads the unstable draft-v2 initialize response, which is the
only place the optional capability fields are published. The stable
v2.Initialization drops them, so initialize with
v2.Connection.InitializeExtended, which validates the result and records the
connection state like InitializeWithInfo and also decodes it into the unstable
type:

	var initialization unstable.InitializeResponse // schema/v2/unstable
	_, err := connection.InitializeExtended(ctx, capabilities, info, &initialization)
	providers, err := acpunstable.ListProviders(ctx, connection, initialization)

An initialize sent through Connection.Call is negotiated and recorded too, as
in the reference SDK, but returns only the raw result; InitializeExtended also
decodes it.

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
