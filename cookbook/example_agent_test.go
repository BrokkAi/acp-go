package cookbook_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/schema"
)

// releaseAgent is a stable-v1 agent. agent.Agent requires Initialize,
// NewSession, and Prompt; the runtime discovers optional methods through
// narrow interfaces such as agent.SessionLoader and agent.SessionCloser, and
// answers method-not-found for any the agent does not implement.
type releaseAgent struct {
	sessions atomic.Uint64
}

func (a *releaseAgent) Initialize(context.Context, agent.Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion:   acp.Version,
		AgentInfo:         &schema.Implementation{Name: "cookbook-release-agent", Version: "0.1.0"},
		AgentCapabilities: &schema.AgentCapabilities{},
	}, nil
}

func (a *releaseAgent) NewSession(context.Context, agent.Client, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	return schema.NewSessionResponse{
		SessionID: schema.SessionId(fmt.Sprintf("release-%d", a.sessions.Add(1))),
	}, nil
}

// Prompt streams progress, reports a tool call, and asks permission before
// running it. The runtime adds the session ID to every update and cancels ctx
// when the client sends session/cancel.
func (a *releaseAgent) Prompt(ctx context.Context, client agent.Client, request schema.PromptRequest, updates agent.SessionUpdater) (schema.PromptResponse, error) {
	say := func(text string) error {
		return updates.Update(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
			Content: schema.ContentBlock{Text: &schema.TextContent{Text: text}},
		}})
	}
	if err := say("Preparing the release. "); err != nil {
		return schema.PromptResponse{}, err
	}

	// Report the tool call before asking about it, so the client can show it.
	title := "git tag v1.2.0"
	kind, pending := schema.ToolKindExecute, schema.ToolCallStatusPending
	if err := updates.Update(schema.SessionUpdate{ToolCall: &schema.ToolCall{
		ToolCallID: "tag-release", Title: title, Kind: &kind, Status: &pending,
	}}); err != nil {
		return schema.PromptResponse{}, err
	}
	permission, err := client.RequestPermission(ctx, schema.RequestPermissionRequest{
		SessionID: request.SessionID,
		ToolCall:  schema.ToolCallUpdate{ToolCallID: "tag-release", Title: &title, Kind: &kind, Status: &pending},
		Options: []schema.PermissionOption{
			{OptionID: "allow", Name: "Allow once", Kind: schema.PermissionOptionKindAllowOnce},
			{OptionID: "reject", Name: "Reject", Kind: schema.PermissionOptionKindRejectOnce},
		},
	})
	if ctx.Err() != nil {
		return schema.PromptResponse{StopReason: schema.StopReasonCancelled}, nil
	}
	if err != nil {
		return schema.PromptResponse{}, err
	}

	outcome, message := schema.ToolCallStatusFailed, "Skipped the tag."
	if selected := permission.Outcome.Selected; selected != nil && selected.OptionID == "allow" {
		outcome, message = schema.ToolCallStatusCompleted, "Tagged v1.2.0."
	}
	if err := updates.Update(schema.SessionUpdate{ToolCallUpdate: &schema.ToolCallUpdate{
		ToolCallID: "tag-release", Status: &outcome,
	}}); err != nil {
		return schema.PromptResponse{}, err
	}
	if err := say(message); err != nil {
		return schema.PromptResponse{}, err
	}
	return schema.PromptResponse{StopReason: schema.StopReasonEndTurn}, nil
}

// approveOnce is an interactive client's permission host reduced to a rule:
// it selects the first allow-once option and otherwise cancels.
type approveOnce struct{}

func (approveOnce) RequestPermission(_ context.Context, request schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
	fmt.Println("permission requested:", *request.ToolCall.Title)
	for _, option := range request.Options {
		if option.Kind == schema.PermissionOptionKindAllowOnce {
			return schema.RequestPermissionResponse{Outcome: schema.RequestPermissionOutcome{
				Selected: &schema.SelectedPermissionOutcome{OptionID: option.OptionID},
			}}, nil
		}
	}
	return schema.RequestPermissionResponse{Outcome: schema.RequestPermissionOutcome{
		Cancelled: &schema.RequestPermissionOutcomeCancelled{},
	}}, nil
}

func Example_buildingAnAgent() {
	ctx := context.Background()
	link := acptest.NewPair()
	// Serve is the whole agent process: in main, pass os.Stdin and os.Stdout.
	go agent.New(&releaseAgent{}).Serve(ctx, link.B, link.B)

	var text strings.Builder
	connection := acp.Connect(link.A, link.A,
		agent.HandlePermissions(nil, approveOnce{}),
		acp.SessionUpdates(func(update acp.Update) error {
			switch {
			case update.Update.ToolCall != nil:
				fmt.Println("tool call:", update.Update.ToolCall.Title, *update.Update.ToolCall.Status)
			case update.Update.ToolCallUpdate != nil:
				fmt.Println("tool call update:", *update.Update.ToolCallUpdate.Status)
			case update.Update.AgentMessageChunk != nil && update.Update.AgentMessageChunk.Content.Text != nil:
				text.WriteString(update.Update.AgentMessageChunk.Content.Text.Text)
			}
			return nil
		}),
	)
	defer connection.Close()

	directory, err := os.Getwd()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	initialization, err := connection.InitializeWithInfo(ctx, acp.Capabilities{}, acp.ClientInfo{
		Name: "cookbook-client", Version: "0.1.0",
	})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	session, err := connection.NewSessionWithOptions(ctx, initialization, directory, acp.NewSessionOptions{})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	reason, err := connection.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent("Cut the release.")})
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	_ = connection.Close()
	fmt.Println(text.String())
	fmt.Println("stop reason:", reason)
	// Output:
	// tool call: git tag v1.2.0 pending
	// permission requested: git tag v1.2.0
	// tool call update: completed
	// Preparing the release. Tagged v1.2.0.
	// stop reason: end_turn
}
