package cookbook_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/schema"
)

// askAgent is a complete stable-v1 one-shot client: initialize, open one
// session, send one prompt, and return the text the agent streamed during
// that turn. It owns the connection it opens over in and out.
func askAgent(ctx context.Context, in io.ReadCloser, out io.WriteCloser, directory, prompt string) (string, error) {
	var text strings.Builder
	// Update callbacks run on the connection's reader in wire order, and every
	// update the agent sent before its session/prompt response has been handled
	// by the time PromptContent returns.
	updates := acp.SessionUpdates(func(update acp.Update) error {
		if chunk := update.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
			text.WriteString(chunk.Content.Text.Text)
		}
		return nil
	})
	// Install request handlers before the first request: agents may ask for
	// permission during any prompt. This non-interactive client refuses.
	connection := acp.Connect(in, out, agent.HandlePermissions(nil, refusePermissions{}), updates)
	defer connection.Close()

	initialization, err := connection.InitializeWithInfo(ctx, acp.Capabilities{}, acp.ClientInfo{
		Name: "cookbook-client", Version: "0.1.0",
	})
	if err != nil {
		return "", err
	}
	session, err := connection.NewSessionWithOptions(ctx, initialization, directory, acp.NewSessionOptions{})
	if err != nil {
		return "", err
	}
	// PromptContent checks content blocks against the advertised prompt
	// capabilities before sending, then blocks until the turn ends.
	reason, err := connection.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent(prompt)})
	if err != nil {
		return "", err
	}
	// Close joins the reader, so no late update can race with reading text.
	_ = connection.Close()
	if reason != schema.StopReasonEndTurn {
		return text.String(), fmt.Errorf("agent stopped with %s", reason)
	}
	return text.String(), nil
}

// refusePermissions answers every permission request with the protocol's
// cancelled outcome, the policy of the reference one-shot client.
type refusePermissions struct{}

func (refusePermissions) RequestPermission(context.Context, schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
	return schema.RequestPermissionResponse{Outcome: schema.RequestPermissionOutcome{
		Cancelled: &schema.RequestPermissionOutcomeCancelled{},
	}}, nil
}

func Example_oneShotPrompt() {
	ctx := context.Background()
	// An in-memory agent stands in for a launched agent process; the runner
	// recipe shows the process-owning version.
	link := acptest.NewPair()
	go agent.New(&acptest.TestAgent{}).Serve(ctx, link.B, link.B)

	directory, err := os.Getwd()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	text, err := askAgent(ctx, link.A, link.A, directory, "Hello from the Go cookbook.")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(text)
	// Output: Hello from the Go cookbook.
}
