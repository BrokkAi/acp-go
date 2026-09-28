package acp

import "github.com/BrokkAi/acp-go/schema"

// TerminalAuthCapabilities advertises support for terminal-type authentication
// methods through clientCapabilities.auth.terminal.
//
// Advertise it only when the application can reproduce the configured agent
// invocation in an interactive terminal, because the agent may then answer
// initialize with a method whose authentication happens outside this
// connection. See https://agentclientprotocol.com/protocol/v1/authentication
// and runner.RunTerminalAuth, which runs that interactive login and leaves the
// application to reconnect and initialize again.
func TerminalAuthCapabilities() *schema.AuthCapabilities {
	return &schema.AuthCapabilities{Terminal: boolPtr(true)}
}
