package acphttp_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/schema"
	"github.com/BrokkAi/acp-go/transport/http"
)

var (
	// largePrompt is longer than the 8 MiB line limit acp-go once had and
	// fits the 16 MiB POST body limit, which the reference SDK has too.
	largePrompt = strings.Repeat("p", 9<<20)
	// largeEvent is longer than the old 8 MiB line limit; what the agent
	// sends has no limit.
	largeEvent = strings.Repeat("e", 9<<20)
)

type largeFrameAgent struct{}

func (largeFrameAgent) Initialize(context.Context, agent.Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion: acp.Version,
		AgentInfo:       &schema.Implementation{Name: "large-frame-agent", Version: "test"},
		Meta:            schema.Meta{{Key: "blob", Value: largeEvent}},
	}, nil
}

func (largeFrameAgent) NewSession(context.Context, agent.Client, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	return schema.NewSessionResponse{SessionID: "large-session"}, nil
}

func (largeFrameAgent) Prompt(_ context.Context, _ agent.Client, request schema.PromptRequest, updates agent.SessionUpdater) (schema.PromptResponse, error) {
	if len(request.Prompt) != 1 || request.Prompt[0].Text == nil || request.Prompt[0].Text.Text != largePrompt {
		return schema.PromptResponse{}, fmt.Errorf("prompt did not arrive whole")
	}
	if err := updates.Update(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
		Content: schema.ContentBlock{Text: &schema.TextContent{Text: largeEvent}},
	}}); err != nil {
		return schema.PromptResponse{}, err
	}
	return schema.PromptResponse{StopReason: schema.StopReasonEndTurn}, nil
}

// TestHTTPTransportCarriesFramesOfAnySize sends a POST body over 8 MiB to an
// acp-go agent runtime, and has the client read an initialize response and an
// event-stream event over 8 MiB. Only the POST body is limited, as in the
// reference SDK.
func TestHTTPTransportCarriesFramesOfAnySize(t *testing.T) {
	acpServer := &acphttp.Server{Entry: agent.New(largeFrameAgent{})}
	defer acpServer.Close()
	server := httptest.NewServer(acpServer)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	updates := make(chan acp.Update, 1)
	client, err := acphttp.Dial(ctx, server.URL, nil, acp.SessionUpdates(func(update acp.Update) error {
		updates <- update
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	initialization, err := client.InitializeWithInfo(ctx, acp.Capabilities{}, acp.ClientInfo{Name: "client", Version: "1"})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	value, _ := initialization.Meta.Get("blob")
	if blob, _ := value.(string); blob != largeEvent {
		t.Fatalf("initialize _meta.blob is %d bytes", len(blob))
	}
	session, err := client.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	reason, err := client.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent(largePrompt)})
	if err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if reason != schema.StopReasonEndTurn {
		t.Fatalf("stop reason = %q", reason)
	}
	select {
	case update := <-updates:
		chunk := update.Update.AgentMessageChunk
		if chunk == nil || chunk.Content.Text == nil || chunk.Content.Text.Text != largeEvent {
			t.Fatal("the large session update did not arrive whole")
		}
	case <-ctx.Done():
		t.Fatal("the large session update was not delivered")
	}
}
