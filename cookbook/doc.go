// Package cookbook is a guide to building ACP clients, proxies, and agents with
// acp-go. It ports the recipes of the reference Rust SDK's
// agent-client-protocol-cookbook crate that map onto acp-go's packages.
//
// The package has no API. Each recipe is one runnable example in its own
// example_*_test.go file, and go test runs every example against a
// deterministic agent, so the code cannot drift from the SDK. Read the example
// source for the code; the sections below explain each pattern, the ordering
// rules behind it, and where Go differs from Rust.
//
// # Recipes
//
// Clients:
//
//   - Example_oneShotPrompt: send one stable-v1 prompt over any transport.
//     Documents [github.com/BrokkAi/acp-go].
//   - Example_runner: the same with a launched agent process.
//     Documents [github.com/BrokkAi/acp-go/runner] and
//     [github.com/BrokkAi/acp-go/clienthost].
//   - Example_v2OneShotPrompt: one draft-v2 prompt, waiting for idle.
//     Documents [github.com/BrokkAi/acp-go/v2].
//   - Example_v2Runner: the same with a launched agent process.
//     Documents [github.com/BrokkAi/acp-go/v2/runner].
//   - Example_orderedApplicationDispatch: apply replay, responses, and
//     closure in one order on one goroutine.
//     Documents [github.com/BrokkAi/acp-go.Notifications] ordering.
//   - Example_v2SessionCoordination: share resume, replay, and close of
//     draft-v2 sessions within one application.
//     Documents [github.com/BrokkAi/acp-go/v2.Connection.ResumeSessionFromStart].
//   - Example_mcpServers and Example_v2McpServers: attach MCP servers to
//     sessions. Documents [github.com/BrokkAi/acp-go/mcp] and
//     [github.com/BrokkAi/acp-go/v2/mcp].
//
// Proxies:
//
//   - Example_proxyComponent: a proxy the reference conductor can run,
//     adding an MCP server to every session. Documents
//     [github.com/BrokkAi/acp-go/proxyrouter].
//
// Agents:
//
//   - Example_buildingAnAgent: initialize, sessions, streamed updates, tool
//     calls, and permission requests. Documents
//     [github.com/BrokkAi/acp-go/agent].
//
// The examples connect to agents over [github.com/BrokkAi/acp-go/acptest.Pair],
// an in-memory duplex transport. A real client connects the same code to an
// agent process's stdin and stdout, as examples/minimal-client does, or lets
// a runner own the process.
//
// # One-shot prompt
//
// [github.com/BrokkAi/acp-go.Connect] runs one connection over any pair of
// streams and owns them until Close. Install request handlers and update
// callbacks there, before the first request: an agent may ask for permission
// during any prompt. InitializeWithInfo negotiates v1,
// NewSessionWithOptions checks the directory, MCP transports, and additional
// roots against the agent's capabilities, and PromptContent checks content
// blocks before sending and blocks until the turn ends with a stop reason.
//
// Update callbacks run on the connection's reader in wire order, and every
// update the agent sent before its session/prompt response has been handled
// when PromptContent returns, so the streamed text is complete at that point.
// The recipe refuses permission requests, as the reference one-shot client
// does; [github.com/BrokkAi/acp-go/agent.HandlePermissions] composes any typed
// permission host.
//
// [github.com/BrokkAi/acp-go/runner.Runner] is the process-owning version: it
// launches the agent, serves filesystem and terminal requests through the
// workspace-confined [github.com/BrokkAi/acp-go/clienthost.Host], writes a
// private JSONL transcript, and refuses permission requests unless
// Config.AutoApprove is set. A [github.com/BrokkAi/acp-go/runner.SetupError]
// names the step that failed before the prompt reached the agent.
//
// # Draft-v2 one-shot prompt
//
// A draft-v2 session/prompt response only acknowledges that the agent inserted
// the user message and returns its ID. The answer is complete at the
// session's next idle state update after it reports running, and the session
// stays open until the client closes it.
//
// Install a [github.com/BrokkAi/acp-go/v2.SessionTracker] and a
// [github.com/BrokkAi/acp-go/v2.CancellablePermissions] host when the
// connection is created: v2 updates and interactive requests may arrive before
// setup responses. [github.com/BrokkAi/acp-go/v2.SessionHandle.Prompt] resets
// the session's work projection before sending, so updates queued from
// earlier work cannot complete the new turn, and WaitForIdle returns the
// projected text and stop reason. state_update describes session-wide
// foreground state and v2 updates carry no prompt ID, so an idle update is not
// attributable to one particular prompt.
//
// [github.com/BrokkAi/acp-go/v2/runner.Runner] is the process-owning version,
// and cancels permission requests unless Config.Permissions installs a host.
//
// # Building an agent
//
// Implement [github.com/BrokkAi/acp-go/agent.Agent] (Initialize, NewSession,
// and Prompt) and serve it with [github.com/BrokkAi/acp-go/agent.New]; in a
// command, pass os.Stdin and os.Stdout to Serve. The runtime discovers
// optional methods through narrow interfaces such as
// [github.com/BrokkAi/acp-go/agent.SessionLoader], answers method-not-found
// for the rest, and rejects optional methods the agent did not advertise.
//
// During Prompt, [github.com/BrokkAi/acp-go/agent.SessionUpdater] emits
// session/update notifications for the enclosing session, and
// [github.com/BrokkAi/acp-go/agent.Client] reaches the client's permission,
// filesystem, terminal, and elicitation methods; the runtime rejects host
// methods the client did not advertise. session/cancel cancels the prompt's context,
// and the agent then ends the turn with the cancelled stop reason. Report a
// tool call before requesting permission for it, so the client can show it.
//
// Draft-v2 agents implement [github.com/BrokkAi/acp-go/v2/agent.Agent], whose
// Prompt must return the inserted user message's ID and may keep sending
// updates after it returns.
//
// Components share one entry point, Serve(ctx, in, out): the agent runtimes,
// [github.com/BrokkAi/acp-go/agentrouter.Agent], and
// [github.com/BrokkAi/acp-go/proxyrouter.Proxy] all use it, which is what
// makes them composable. Request handling composes the same way:
// [github.com/BrokkAi/acp-go.Handler] values wrap one another, as
// [github.com/BrokkAi/acp-go/agent.HandlePermissions] and
// [github.com/BrokkAi/acp-go/unstable.Handle] do.
//
// # Ordered application dispatch
//
// A notification callback returning means acp-go delivered the update, not
// that the application applied it. When callbacks hand updates to another
// goroutine, put request results and connection closure on the same FIFO, and
// let one consumer apply events in queue order.
//
// This works because of the ordering contract of
// [github.com/BrokkAi/acp-go.Notifications]: callbacks run on the connection's
// reader in wire order, before a later response is delivered. When a call
// returns, every notification that preceded its response on the wire has
// already been queued, so a result queued after the call lands behind them. For
// a v2 resume, whose replay precedes its response, the consumer applies the
// whole replay before it sees the resume result.
//
// Go has no counterpart to Rust's on_receiving_result, a response callback run
// in dispatch order. A notification that follows the response on the wire
// may therefore be queued before the result. The result still follows every
// update that preceded it, but its queue position does not separate replayed
// from live updates.
//
// Callbacks must return promptly and must not call back into their
// connection. Blocking a callback for queue space applies backpressure to the
// reader and is safe only while the consumer never waits for traffic on the
// same connection. When the peer closes the stream, Done closes after every
// notification read from it has been handled, so a closure queued from Done
// follows all updates; calls still pending fail with the connection's error,
// and their results may arrive after closure. Treat closure as terminal.
//
// # Draft-v2 session coordination
//
// This recipe is a prototype of application-owned coordination, like the Rust
// SDK's v2_session_coordination example; it is not SDK API. One coordinator
// goroutine owns every session operation. Application calls and SDK callbacks
// only send it messages, and it never waits for the network: requests run on
// their own goroutines and report back through the same FIFO as the updates,
// which gives the dispatch ordering above. Its policy:
//
//   - The first Open resumes the session with replay from the start, and
//     concurrent Opens join that resume and share one view.
//   - Releasing the last lease closes the session. An Open during a close waits
//     for it and then resumes afresh.
//   - An Open whose context ends gives up its place. A resume that every
//     waiter abandoned keeps its slot but drops its replay, and the session is
//     closed when the response arrives, before any queued Open resumes it.
//   - A failed resume fails its waiters and resumes again for queued Opens. A
//     failed close leaves the remote state uncertain, so the session is
//     blocked and later Opens fail.
//   - Queued application commands are handled before wire events, so a
//     release already sent is honored before a response is exposed.
//   - When the application finishes, the connection closes without waiting
//     for outstanding closes, and open views are marked disconnected.
//
// The view is a lossless update log, not a message reducer.
//
// # Proxy components
//
// The reference conductor chains proxies between a client and an agent. It
// initializes each proxy with _proxy/initialize, whose parameters and result
// are the v1 initialize request and response, and delivers client traffic
// unwrapped. Traffic to and from a proxy's successor travels inside
// _proxy/successor envelopes carrying the inner method, params, and _meta.
// The pinned schema has no _proxy/* methods, so the recipe hand-models the
// envelope from the Rust SDK 2.2.0 and acp-go ships no helper for it; the
// conductor itself is recorded as won't port (#24). Run a Go proxy under the
// reference agent-client-protocol-conductor.
//
// [github.com/BrokkAi/acp-go/proxyrouter.Router] hands each connection to the
// implementation configured for the exact _proxy/initialize version, replays
// that first frame unchanged, and rejects other versions without downgrading.
// The recipe's v1 proxy forwards everything and appends an MCP server
// declaration to session/new and session/load, preserving fields it does not
// model.
//
// Notification callbacks must not write to their own connection, so the proxy
// queues notifications and sends them from a goroutine. Before it forwards a
// response or a request, it flushes the queue for that direction, so a v1
// client still receives every session/update of a turn before the turn's
// response. A cancelled client request cancels the forwarded call, which
// sends its own $/cancel_request for that hop. acp-go runs inbound requests
// concurrently, so a notification that follows a request, such as
// session/cancel right after session/prompt, can overtake it on its way to the
// successor.
//
// The example's testConductor only exists so the recipe runs under go test.
//
// # Attaching MCP servers
//
// Stable v1 needs no opt-in: pass servers built with
// [github.com/BrokkAi/acp-go.NewStdioMCPServer],
// [github.com/BrokkAi/acp-go.NewHTTPMCPServer], or
// [github.com/BrokkAi/acp-go.NewSSEMCPServer] in
// [github.com/BrokkAi/acp-go.NewSessionOptions]. Stdio servers are baseline;
// HTTP and SSE servers are checked against the agent's mcpCapabilities before
// anything is sent.
//
// Importing [github.com/BrokkAi/acp-go/mcp] opts in to the unstable transports,
// including native MCP-over-ACP. Its gates read the unstable initialize
// response, and the stable facade does not decode those flags, so the recipe
// captures the raw initialize result and decodes it into both shapes. That
// bypasses InitializeWithInfo, so it checks the negotiated version itself. A
// native acp server makes the agent reach the MCP server over the same ACP
// connection with mcp/connect, mcp/message, and mcp/disconnect. acp-go does
// not serve those methods yet (#39), so an application that declares one
// answers them in its own handler.
//
// Draft v2 attaches servers only through [github.com/BrokkAi/acp-go/v2/mcp];
// the core v2 facade refuses them. Every transport, stdio included, needs the
// agent's session.mcp capability, and a stdio command must be an absolute
// path.
//
// # Recipes that do not port
//
// The Rust global_mcp_server, per_session_mcp_server, and filtering_tools
// recipes build MCP servers inside a proxy with the rmcp SDK and serve them
// over MCP-over-ACP. acp-go is standard-library-only, so it has no MCP server
// builder, and the MCP-over-ACP methods are open in #39. The proxy recipe
// shows the ACP half: adding a server declaration to session setup.
//
// running_proxies_with_conductor has no Go counterpart, because the conductor
// is won't port (#24). connecting_as_client describes Rust's session builder;
// in Go, the typed methods on the connection are that API.
package cookbook
