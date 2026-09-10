// Command v2-one-shot-client sends one ACP v2 prompt and waits for idle.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	acpv2 "github.com/BrokkAi/acp-go/v2"
	"github.com/BrokkAi/acp-go/v2/runner"
)

func main() {
	command := flag.String("command", "go run ./examples/minimal-agent-v2", "space-separated ACP v2 agent command")
	prompt := flag.String("prompt", "Say hello using draft ACP v2.", "prompt text")
	workingDirectory := flag.String("cwd", "", "absolute workspace directory (defaults to the current directory)")
	flag.Parse()

	directory := *workingDirectory
	if directory == "" {
		absolute, err := filepath.Abs(".")
		if err != nil {
			panic(err)
		}
		directory = absolute
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	result, err := runner.Runner{
		Config: runner.Config{
			Directory: directory,
			Agent: runner.AgentConfig{
				Command: strings.Fields(*command),
			},
			ClientInfo: acpv2.ClientInfo{Name: "v2-one-shot-client", Version: "0.3.0"},
		},
	}.Execute(ctx, *prompt)
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Text)
	if result.StopReason != nil {
		fmt.Fprintln(os.Stderr, "Session is idle:", *result.StopReason)
	}
}
