package agent

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

type echoAgent struct {
	cancelled chan schema.CancelSessionNotification
}

func (a *echoAgent) Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion: version,
		Info:            schema.Implementation{Name: "echo-v2", Version: "test"},
		Capabilities:    &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}},
	}, nil
}

func (a *echoAgent) NewSession(_ context.Context, _ Client, request schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	return schema.NewSessionResponse{SessionID: "v2-session"}, nil
}

func (a *echoAgent) Prompt(_ context.Context, _ Client, request schema.PromptRequest, updates SessionUpdater) (schema.PromptResponse, error) {
	message := "no text"
	if len(request.Prompt) == 1 && request.Prompt[0].Text != nil {
		message = request.Prompt[0].Text.Text
	}
	content := []schema.ContentBlock{{Text: &schema.TextContent{Text: message}}}
	for _, update := range []schema.SessionUpdate{
		{UserMessage: &schema.UserMessage{MessageID: "user"}},
		{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}},
		{AgentMessage: &schema.AgentMessage{
			MessageID: "agent",
			Content:   schema.Nullable[[]schema.ContentBlock]{Set: true, Value: content},
		}},
		{StateUpdate: &schema.StateUpdate{Idle: &schema.IdleStateUpdate{}}},
	} {
		if err := updates.Update(update); err != nil {
			return schema.PromptResponse{}, err
		}
	}
	return schema.PromptResponse{}, nil
}

func (a *echoAgent) ListSessions(context.Context, Client, schema.ListSessionsRequest) (schema.ListSessionsResponse, error) {
	return schema.ListSessionsResponse{Sessions: []schema.SessionInfo{{SessionID: "v2-session", Cwd: "/repo"}}}, nil
}

func (a *echoAgent) ResumeSession(context.Context, Client, schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	return schema.ResumeSessionResponse{}, nil
}

func (a *echoAgent) CloseSession(context.Context, Client, schema.CloseSessionRequest) (schema.CloseSessionResponse, error) {
	return schema.CloseSessionResponse{}, nil
}

func (a *echoAgent) CancelSession(notification schema.CancelSessionNotification) error {
	a.cancelled <- notification
	return nil
}

func startRuntime(t *testing.T, implementation Agent, notifications acp.Notifications) *acpv2.Connection {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- New(implementation).Serve(context.Background(), peer, peer)
	}()
	client := acpv2.Connect(local, local, nil, notifications)
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.Close()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	return client
}

func TestRuntimeServesBaselineV2Lifecycle(t *testing.T) {
	updates := make(chan acpv2.Update, 4)
	cancelled := make(chan schema.CancelSessionNotification, 1)
	client := startRuntime(t, &echoAgent{cancelled: cancelled}, acpv2.SessionUpdates(func(update acpv2.Update) error {
		updates <- update
		return nil
	}))
	ctx := context.Background()
	initialization, err := client.InitializeWithInfo(ctx, acpv2.Capabilities{}, acpv2.ClientInfo{Name: "test-client", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSessionWithOptions(ctx, initialization, "/repo", acpv2.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Prompt(ctx, initialization, session, "hello v2"); err != nil {
		t.Fatal(err)
	}
	var sawAgentMessage, sawIdle bool
	for i := 0; i < 4; i++ {
		select {
		case update := <-updates:
			sawAgentMessage = sawAgentMessage || update.Update.AgentMessage != nil
			sawIdle = sawIdle || (update.Update.StateUpdate != nil && update.Update.StateUpdate.Idle != nil)
		case <-time.After(time.Second):
			t.Fatal("missing session update")
		}
	}
	if !sawAgentMessage || !sawIdle {
		t.Fatalf("missing expected updates: agent=%t idle=%t", sawAgentMessage, sawIdle)
	}
	if err := client.CancelSession(ctx, session.SessionID); err != nil {
		t.Fatal(err)
	}
	select {
	case notification := <-cancelled:
		if notification.SessionID != session.SessionID {
			t.Fatalf("cancelled session = %q", notification.SessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel notification was not delivered")
	}
	list, err := client.ListSessions(ctx, initialization, schema.ListSessionsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].SessionID != "v2-session" {
		t.Fatalf("sessions = %+v", list.Sessions)
	}
	if _, err := client.ResumeSession(ctx, initialization, schema.ResumeSessionRequest{
		SessionID: session.SessionID, Cwd: "/repo",
	}); err != nil {
		t.Fatal(err)
	}
	if err := client.CloseSession(ctx, initialization, session.SessionID); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRejectsInvalidAdvertisements(t *testing.T) {
	t.Run("missing sessions", func(t *testing.T) {
		client := startRuntime(t, &noSessionAgent{echoAgent: &echoAgent{}}, nil)
		_, err := client.Initialize(context.Background(), acpv2.Capabilities{})
		if err == nil {
			t.Fatal("runtime accepted an agent without session capabilities")
		}
	})
}

func TestRuntimeRejectsV1InitializeOnV2Endpoint(t *testing.T) {
	client := startRuntime(t, &echoAgent{cancelled: make(chan schema.CancelSessionNotification, 1)}, nil)
	err := client.Call(context.Background(), schema.InitializeMethodName, schema.InitializeRequest{
		ProtocolVersion: 1,
		Info:            schema.Implementation{Name: "client", Version: "1"},
	}, nil)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32600 || !strings.Contains(rpcErr.Message, "only supports ACP protocol version 2") {
		t.Fatalf("mismatched initialize error = %v", err)
	}
}

func TestRuntimeGatesClientElicitationByCapability(t *testing.T) {
	client := startRuntime(t, &gatingAgent{echoAgent: &echoAgent{}}, nil)
	ctx := context.Background()
	initialization, err := client.InitializeWithInfo(ctx, acpv2.Capabilities{}, acpv2.ClientInfo{Name: "test", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSessionWithOptions(ctx, initialization, "/repo", acpv2.NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Prompt(ctx, initialization, session, "hello")
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("prompt error = %v", err)
	}
}

type noSessionAgent struct{ *echoAgent }

func (a *noSessionAgent) Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{ProtocolVersion: version}, nil
}

type gatingAgent struct{ *echoAgent }

func (a *gatingAgent) Prompt(ctx context.Context, client Client, request schema.PromptRequest, _ SessionUpdater) (schema.PromptResponse, error) {
	_, err := client.CreateElicitation(ctx, schema.CreateElicitationRequest{
		URL: &schema.ElicitationUrlMode{Session: &schema.ElicitationSessionScope{SessionID: request.SessionID}},
	})
	return schema.PromptResponse{}, err
}
