// Command minimal-client launches an ACP agent command, sends one prompt, and
// streams agent message chunks to stdout.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
)

func main() {
	command := flag.String("agent-command", "go run ./examples/minimal-agent", "space-separated ACP agent command")
	prompt := flag.String("prompt", "Say hello using ACP.", "prompt text")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	workingDirectory, err := filepath.Abs(".")
	if err != nil {
		panic(err)
	}
	agentCommand := strings.Fields(*command)
	cmd := exec.CommandContext(ctx, agentCommand[0], agentCommand[1:]...)
	cmd.Dir = workingDirectory
	stdin, err := cmd.StdinPipe()
	if err != nil {
		panic(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		panic(err)
	}
	if err := cmd.Start(); err != nil {
		panic(err)
	}
	defer func() {
		// connection.Close has closed the agent's stdin, which asks it to exit.
		// Give it a second, as the SDK runners do, before killing it.
		exited := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(exited)
		}()
		select {
		case <-exited:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
	}()

	connection := acp.Connect(stdout, stdin, nil, acp.SessionUpdates(func(update acp.Update) error {
		if chunk := update.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
			fmt.Print(chunk.Content.Text.Text)
		}
		return nil
	}))
	defer connection.Close()

	initialization, err := connection.InitializeWithInfo(ctx, acp.Capabilities{}, acp.ClientInfo{
		Name: "minimal-client", Version: "0.1.0",
	})
	if err != nil {
		panic(err)
	}
	session, err := connection.NewSession(ctx, workingDirectory)
	if err != nil {
		panic(err)
	}
	reason, err := connection.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent(*prompt)})
	if err != nil {
		panic(err)
	}
	if reason != schema.StopReasonEndTurn {
		panic("agent stopped with " + string(reason))
	}
	fmt.Println()
}
