// Command minimal-agent is the smallest useful ACP agent. Run it with stdio
// connected to any ACP client.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"

	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/schema"
)

type echoAgent struct {
	nextSession atomic.Uint64
}

func (a *echoAgent) Initialize(context.Context, agent.Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion: 1,
		AgentInfo:       &schema.Implementation{Name: "minimal-agent", Version: "0.1.0"},
	}, nil
}

func (a *echoAgent) NewSession(context.Context, agent.Client, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	return schema.NewSessionResponse{
		SessionID: schema.SessionId(fmt.Sprintf("session-%d", a.nextSession.Add(1))),
	}, nil
}

func (a *echoAgent) Prompt(_ context.Context, _ agent.Client, request schema.PromptRequest, updates agent.SessionUpdater) (schema.PromptResponse, error) {
	for _, block := range request.Prompt {
		if block.Text == nil {
			continue
		}
		update := schema.SessionUpdate{
			AgentMessageChunk: &schema.ContentChunk{Content: schema.ContentBlock{Text: block.Text}},
		}
		if err := updates.Update(update); err != nil {
			return schema.PromptResponse{}, err
		}
	}
	return schema.PromptResponse{StopReason: schema.StopReasonEndTurn}, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := agent.New(&echoAgent{}).Serve(ctx, os.Stdin, os.Stdout); err != nil {
		panic(err)
	}
}
