package acptest_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	"github.com/BrokkAi/acp-go/agent"
	schema "github.com/BrokkAi/acp-go/schema"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	agentv2 "github.com/BrokkAi/acp-go/v2/agent"
)

// TestPairDrivesV1Agent runs the deterministic test agent end to end over the
// in-memory transport, with a typed prompt command producing pinned updates.
func TestPairDrivesV1Agent(t *testing.T) {
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(pair.Close)
	implementation := &acptest.TestAgent{Capabilities: &schema.AgentCapabilities{}}
	go func() { _ = agent.New(implementation).Serve(context.Background(), pair.B.Reader(), pair.B.Writer()) }()

	updates := make(chan acp.Update, 4)
	client := pair.A.Connect(nil, acp.SessionUpdates(func(update acp.Update) error {
		updates <- update
		return nil
	}))
	defer client.Close()

	ctx := context.Background()
	initialization, err := client.Initialize(ctx, acp.WorkspaceCapabilities(false, false, false))
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	session, err := client.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	if session.SessionID != "test-session" {
		t.Fatalf("session ID = %q", session.SessionID)
	}
	command := acptest.Command{Thought: "considering", Message: "hello from the harness"}
	reason, err := client.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent(command.Prompt())})
	if err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	if reason != schema.StopReasonEndTurn {
		t.Fatalf("stop reason = %q", reason)
	}
	thought := <-updates
	if thought.Update.AgentThoughtChunk == nil || thought.Update.AgentThoughtChunk.Content.Text.Text != "considering" {
		t.Fatalf("thought update = %+v", thought)
	}
	message := <-updates
	if message.Update.AgentMessageChunk == nil || message.Update.AgentMessageChunk.Content.Text.Text != "hello from the harness" {
		t.Fatalf("message update = %+v", message)
	}
}

// TestPairDrivesV2Agent covers the same transport with a draft-v2 runtime.
func TestPairDrivesV2Agent(t *testing.T) {
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(pair.Close)
	go func() {
		_ = agentv2.New(&acptest.V2Agent{}).Serve(context.Background(), pair.B.Reader(), pair.B.Writer())
	}()

	client := acpv2.Connect(pair.A.Reader(), pair.A.Writer(), nil, nil)
	defer client.Close()
	ctx := context.Background()
	initialization, err := client.InitializeWithInfo(ctx, acpv2.Capabilities{}, acpv2.ClientInfo{Name: "acptest", Version: "1"})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	session, err := client.NewSessionWithOptions(ctx, initialization, t.TempDir(), acpv2.NewSessionOptions{})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	if session.SessionID != "v2" {
		t.Fatalf("session ID = %q", session.SessionID)
	}
}

// TestServeAndFramingHelpers covers the raw router-style harness.
func TestServeAndFramingHelpers(t *testing.T) {
	rw := acptest.Serve(t, func(_ context.Context, in io.ReadCloser, out io.WriteCloser) error {
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.NewDecoder(in).Decode(&request); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(map[string]any{
			"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"protocolVersion": 1},
		})
	})
	acptest.WriteInitialize(t, rw, map[string]any{
		"protocolVersion": 1,
		"clientInfo":      map[string]any{"name": "client", "version": "1"},
	})
	var response struct {
		Result struct {
			ProtocolVersion uint16 `json:"protocolVersion"`
		} `json:"result"`
	}
	acptest.ReadResponse(t, rw, &response)
	if response.Result.ProtocolVersion != 1 {
		t.Fatalf("protocol version = %d", response.Result.ProtocolVersion)
	}
}

func TestCommandPromptRoundTrip(t *testing.T) {
	command := acptest.Command{Message: "hi", StopReason: schema.StopReasonMaxTokens}
	if parsed := acptest.ParseCommand(command.Prompt()); parsed != command {
		t.Fatalf("round trip = %+v", parsed)
	}
	if plain := acptest.ParseCommand("just text"); plain.Message != "just text" {
		t.Fatalf("plain prompt = %+v", plain)
	}
}
