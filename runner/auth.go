package runner

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/osrun"
	"github.com/BrokkAi/acp-go/schema"
)

// TerminalAuthMethod returns the terminal-type authentication method the agent
// advertised under id. It reports an error when id names a protocol-driven
// agent method or was not advertised at all, so a caller can tell "log in
// interactively" apart from "call acp.Connection.Authenticate".
func TerminalAuthMethod(init acp.Initialization, id string) (schema.AuthMethodTerminal, error) {
	if id == "" {
		return schema.AuthMethodTerminal{}, errors.New("authentication method is required")
	}
	for i := range init.AuthMethods {
		method := &init.AuthMethods[i]
		switch {
		case method.Terminal != nil && string(method.Terminal.ID) == id:
			return *method.Terminal, nil
		case method.Agent != nil && string(method.Agent.ID) == id:
			return schema.AuthMethodTerminal{}, fmt.Errorf("authentication %s is protocol-driven; call acp.Connection.Authenticate", id)
		}
	}
	return schema.AuthMethodTerminal{}, fmt.Errorf("agent did not advertise authentication method %q", id)
}

// TerminalAuthCommand builds the interactive login invocation for a terminal
// authentication method: the configured agent command with the method's args
// appended, and the configured environment with the method's env overrides
// applied. It returns a copy and does not launch anything, so callers can run
// the login process themselves.
func TerminalAuthCommand(agent AgentConfig, method schema.AuthMethodTerminal) ([]string, map[string]string, error) {
	if len(agent.Command) == 0 || agent.Command[0] == "" {
		return nil, nil, errors.New("agent command is required")
	}
	command := make([]string, 0, len(agent.Command)+len(method.Args))
	command = append(command, agent.Command...)
	command = append(command, method.Args...)
	environment := make(map[string]string, len(agent.Environment)+len(method.Env))
	for key, value := range agent.Environment {
		environment[key] = value
	}
	for key, value := range method.Env {
		environment[key] = value
	}
	return command, environment, nil
}

// RunTerminalAuth runs the interactive login process for a terminal
// authentication method, attached to this process's standard streams so the
// user can complete the login in the terminal, and waits for it to exit.
//
// Use it after initialize advertises a terminal method:
//
//	init, err := connection.Initialize(ctx, acp.Capabilities{
//		Auth: acp.TerminalAuthCapabilities(),
//	})
//	method, err := runner.TerminalAuthMethod(init, "terminal-login")
//	err = runner.RunTerminalAuth(ctx, config, method)
//	// Reconnect and initialize again; the agent is now authenticated.
//
// Do not call acp.Connection.Authenticate for a terminal method: the protocol
// forbids sending its ID, and the login state comes from this process instead.
// Config.Execute performs its own initialization, so call it after this helper
// returns to start the authenticated session.
func RunTerminalAuth(ctx context.Context, config Config, method schema.AuthMethodTerminal) error {
	command, environment, err := TerminalAuthCommand(config.Agent, method)
	if err != nil {
		return err
	}
	cmd := osrun.StartCommand(ctx, config.Directory, command, environment)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("terminal authentication %s: %w", method.ID, err)
	}
	return nil
}
