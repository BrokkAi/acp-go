// Command drive-cli runs one unattended prompt through an external ACP CLI.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/runner"
)

func main() {
	command := flag.String("command", "", "space-separated ACP CLI command (required)")
	workingDirectory := flag.String("cwd", "", "absolute workspace directory (defaults to the current directory)")
	stateDirectory := flag.String("state", "", "transcript/state directory (defaults to a temporary directory)")
	prompt := flag.String("prompt", "", "prompt to send (required)")
	authMethod := flag.String("auth", "", "advertised agent authentication method")
	mode := flag.String("mode", "", "agent session mode")
	model := flag.String("model", "", "advertised model ID")
	effort := flag.String("effort", "", "advertised reasoning effort")
	autoApprove := flag.Bool("auto-approve", false, "select allow-once/allow-always permission options")
	flag.Parse()

	if *command == "" || *prompt == "" {
		fmt.Fprintln(os.Stderr, "usage: drive-cli -command 'agent --stdio' -prompt 'Inspect the workspace'")
		os.Exit(2)
	}
	if *workingDirectory == "" {
		absolute, err := filepath.Abs(".")
		if err != nil {
			panic(err)
		}
		workingDirectory = &absolute
	}
	if *stateDirectory == "" {
		temporary, err := os.MkdirTemp("", "acp-go-state-")
		if err != nil {
			panic(err)
		}
		stateDirectory = &temporary
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	fields := strings.Fields(*command)
	result, err := runner.Runner{
		Config: runner.Config{
			Directory:      *workingDirectory,
			StateDirectory: *stateDirectory,
			Agent: runner.AgentConfig{
				Command:    fields,
				AuthMethod: *authMethod,
				Mode:       *mode,
				Model:      *model,
				Effort:     *effort,
			},
			ClientInfo:  acp.ClientInfo{Name: "drive-cli", Version: "0.1.0"},
			AutoApprove: *autoApprove,
		},
	}.Execute(ctx, *prompt)
	if err != nil {
		panic(err)
	}
	fmt.Println(result)
}
