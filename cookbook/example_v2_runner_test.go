package cookbook_test

import (
	"context"
	"fmt"
	"os"

	acpv2 "github.com/BrokkAi/acp-go/v2"
	runnerv2 "github.com/BrokkAi/acp-go/v2/runner"
)

// runPromptV2 is the process-owning draft-v2 one-shot client. It ignores
// updates queued before the session reports running, projects the agent's
// messages, and returns at the next idle update. Permission requests are
// cancelled unless Config.Permissions installs a host.
func runPromptV2(ctx context.Context, command []string, environment map[string]string, workspace, state, prompt string) (runnerv2.Result, error) {
	return runnerv2.Runner{Config: runnerv2.Config{
		Directory:      workspace,
		StateDirectory: state,
		Agent: runnerv2.AgentConfig{
			Command:     command,
			Environment: environment,
		},
		ClientInfo: acpv2.ClientInfo{Name: "cookbook-runner", Version: "0.1.0"},
	}}.Execute(ctx, prompt)
}

func Example_v2Runner() {
	workspace, err := os.MkdirTemp("", "cookbook-workspace-")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer os.RemoveAll(workspace)
	state, err := os.MkdirTemp("", "cookbook-state-")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer os.RemoveAll(state)

	// Pass your draft-v2 agent's argv instead of the fixture process.
	command, environment := fixtureAgentProcess("v2")
	result, err := runPromptV2(context.Background(), command, environment, workspace, state, "Hello from the v2 runner.")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(result.Text)
	fmt.Println("accepted user message:", result.UserMessageID)
	// Output:
	// Echo: Hello from the v2 runner.
	// accepted user message: user-1
}
