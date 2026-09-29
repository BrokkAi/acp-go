package cookbook_test

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/runner"
)

// runPrompt launches one v1 agent process, sends one prompt, and returns the
// agent's answer. The runner owns the process, the confined clienthost
// workspace, the permission policy, and a private JSONL transcript.
func runPrompt(ctx context.Context, command []string, environment map[string]string, workspace, state, prompt string) (string, error) {
	answer, err := runner.Runner{Config: runner.Config{
		Directory:      workspace,
		StateDirectory: state,
		Agent: runner.AgentConfig{
			Command:     command,
			Environment: environment,
		},
		ClientInfo: acp.ClientInfo{Name: "cookbook-runner", Version: "0.1.0"},
		// AutoApprove stays false: permission requests are refused unless the
		// application explicitly authorizes unattended operation.
	}}.Execute(ctx, prompt)
	var setup *runner.SetupError
	if errors.As(err, &setup) {
		// Setup failures happen before the prompt reaches the agent, so
		// retrying with the same configuration cannot help.
		return "", fmt.Errorf("agent setup failed during %s: %w", setup.Phase, setup.Err)
	}
	return answer, err
}

func Example_runner() {
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

	// fixtureAgentProcess relaunches this test binary as a deterministic agent.
	// Pass your agent's argv instead, for example []string{"my-agent", "--acp"}.
	command, environment := fixtureAgentProcess("v1")
	answer, err := runPrompt(context.Background(), command, environment, workspace, state, "Summarize the workspace.")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(answer)
	// Output: Summarize the workspace.
}
