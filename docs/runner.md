# Process runner

`github.com/BrokkAi/acp-go/runner` runs an ACP v1 agent process end to end. It
adds a process lifecycle, workspace-confined client file operations, terminal
callbacks, streaming slog output, and private JSONL transcripts on top of the
[v1 client](client.md), using [`clienthost`](agent-runtime.md#client-hosts).
The draft-v2 counterpart is [`v2/runner`](draft-v2.md#one-shot-runner).

`Runner.Execute(ctx, prompt)` returns the complete agent text. Applications own
receipt parsing and workflow policy.

## Permissions

`Config.AutoApprove` defaults to `false`, and then every permission request is
answered with the cancelled outcome. Set it explicitly to `true` to approve
permission requests for unattended operation.

> [!WARNING]
> The runner is not an OS sandbox. Agents and terminal commands inherit the
> caller's permissions.

## Errors

`SetupError` distinguishes failures before a prompt from failures during work.
Its `Phase` field names the step that failed (`runner.PhaseSelectModel`,
`runner.PhaseSelectEffort`, and so on).

Some selection failures are typed for `errors.As`:

| Type | Meaning |
|---|---|
| `*acp.UnknownSelectionError` | The value is not offered; `Available` lists the advertised values |
| `*acp.UnsupportedSelectionError` | The agent advertises no such selector |
| `*acp.RPCError` | The agent returned a JSON-RPC error |

Other failures stay untyped, including an agent that does not confirm the
selection, malformed selector options, transport and context errors, and the
legacy `unknown session mode` error. A failure that matches none of these
types is therefore not necessarily an agent rejection.

## Agent process lifecycle

Both runners (`runner` and `v2/runner`) manage the agent process as the Rust
SDK's `AcpAgent` does.

- **After a successful run** they close the agent's stdin and give it one
  second to exit on its own. On Unix, an agent still running then gets
  `SIGTERM` to its whole process group, and `SIGKILL` two seconds later. On
  Windows its process tree is killed.
- **A failed or cancelled run** kills the process group at once.
- **Leftover processes:** whatever remains of the process group is killed once
  the agent exits, so wrapper launchers such as `npx` or `uvx` leave no
  orphans.
- **An agent that exits mid-run:** the runner kills its process group and ends
  the connection within a second, even if a descendant still holds stdout
  open.

An agent that exits with a failure status on its own, during the run or after
its stdin closes, is reported as `*runner.AgentExitError` with the exit status
and the stderr tail. A successful run still returns its result alongside that
error, and a setup failure keeps its `SetupError` phase.

## Terminal authentication

Terminal-type authentication methods run outside the protocol connection:

1. Advertise them with `acp.TerminalAuthCapabilities()`.
2. Resolve the advertised method with `runner.TerminalAuthMethod`.
3. Run the configured agent program interactively with
   `runner.RunTerminalAuth`. The method's `args` are appended and its `env`
   overrides the configured environment.
4. Reconnect and initialize again before `Runner.Execute`.

Never pass a terminal method ID to `Connection.Authenticate`.
