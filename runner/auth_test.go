package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
)

func TestTerminalAuthMethod(t *testing.T) {
	init := acp.Initialization{AuthMethods: []schema.AuthMethod{
		{Agent: &schema.AuthMethodAgent{ID: "agent-login", Name: "Agent"}},
		{Terminal: &schema.AuthMethodTerminal{ID: "terminal-login", Name: "Terminal", Args: []string{"--login"}}},
	}}
	method, err := TerminalAuthMethod(init, "terminal-login")
	if err != nil {
		t.Fatal(err)
	}
	if method.ID != "terminal-login" || len(method.Args) != 1 || method.Args[0] != "--login" {
		t.Fatalf("method = %#v", method)
	}
	for _, tc := range []struct{ name, id, want string }{
		{"agent method", "agent-login", "protocol-driven"},
		{"unknown", "nope", "did not advertise"},
		{"empty", "", "authentication method is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := TerminalAuthMethod(init, tc.id); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestTerminalAuthCommandAppendsArgsAndOverridesEnv(t *testing.T) {
	agent := AgentConfig{
		Command:     []string{"agent", "serve"},
		Environment: map[string]string{"KEEP": "yes", "ACP_INTERACTIVE_LOGIN": "base"},
	}
	method := schema.AuthMethodTerminal{
		Args: []string{"--login"},
		Env:  map[string]string{"ACP_INTERACTIVE_LOGIN": "1"},
	}
	command, environment, err := TerminalAuthCommand(agent, method)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(command, " ") != "agent serve --login" {
		t.Fatalf("command = %v", command)
	}
	if environment["KEEP"] != "yes" || environment["ACP_INTERACTIVE_LOGIN"] != "1" {
		t.Fatalf("environment = %v", environment)
	}
	if len(agent.Command) != 2 || len(agent.Environment) != 2 {
		t.Fatalf("builder mutated its input: %#v", agent)
	}
	if _, _, err := TerminalAuthCommand(AgentConfig{}, method); err == nil {
		t.Fatal("empty command accepted")
	}
}

func TestRunTerminalAuthLaunchesConfiguredCommand(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "login.txt")
	config := Config{
		Directory: t.TempDir(),
		Agent: AgentConfig{
			Command:     []string{"sh", "-c", `printf '%s|%s' "$1" "$ACP_INTERACTIVE_LOGIN" > "$MARKER"`, "sh"},
			Environment: map[string]string{"MARKER": marker, "ACP_INTERACTIVE_LOGIN": "base"},
		},
	}
	method := schema.AuthMethodTerminal{
		ID:   "terminal-login",
		Args: []string{"--login"},
		Env:  map[string]string{"ACP_INTERACTIVE_LOGIN": "1"},
	}
	if err := RunTerminalAuth(context.Background(), config, method); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "--login|1" {
		t.Fatalf("login process saw %q, want %q", data, "--login|1")
	}
}

func TestRunTerminalAuthReportsFailure(t *testing.T) {
	config := Config{Agent: AgentConfig{Command: []string{"sh", "-c", "exit 3"}}}
	err := RunTerminalAuth(context.Background(), config, schema.AuthMethodTerminal{ID: "terminal-login"})
	if err == nil || !strings.Contains(err.Error(), "terminal authentication terminal-login") {
		t.Fatalf("error = %v", err)
	}
}
