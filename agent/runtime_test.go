package agent

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
)

type testAgent struct {
	prompt func(context.Context, Client, schema.PromptRequest, SessionUpdater) (schema.PromptResponse, error)
}

func (a testAgent) Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{ProtocolVersion: acp.Version}, nil
}

func (a testAgent) NewSession(context.Context, Client, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	return schema.NewSessionResponse{SessionID: "session"}, nil
}

func (a testAgent) Prompt(ctx context.Context, client Client, request schema.PromptRequest, updates SessionUpdater) (schema.PromptResponse, error) {
	if a.prompt == nil {
		if err := updates.Update(schema.SessionUpdate{
			AgentMessageChunk: &schema.ContentChunk{Content: acp.NewTextContent("working")},
		}); err != nil {
			return schema.PromptResponse{}, err
		}
		return schema.PromptResponse{StopReason: schema.StopReasonEndTurn}, nil
	}
	return a.prompt(ctx, client, request, updates)
}

func startRuntime(t *testing.T, implementation Agent, clientHost acp.Handler, notifications acp.Notifications) *acp.Connection {
	t.Helper()
	agentSide, clientSide := net.Pipe()
	runtime := New(implementation)
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), agentSide, agentSide) }()
	t.Cleanup(func() {
		_ = clientSide.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("agent runtime: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	return acp.Connect(clientSide, clientSide, clientHost, notifications)
}

func initializeClient(t *testing.T, connection *acp.Connection, capabilities acp.Capabilities) acp.Initialization {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initialization, err := connection.Initialize(ctx, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	return initialization
}

func TestRuntimeServesMandatoryLifecycleAndStreamsUpdates(t *testing.T) {
	updates := make(chan acp.Update, 1)
	connection := startRuntime(t, testAgent{}, nil, acp.SessionUpdates(func(update acp.Update) error {
		updates <- update
		return nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initialization := initializeClient(t, connection, acp.Capabilities{})
	session, err := connection.NewSession(ctx, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	reason, err := connection.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent("hello")})
	if err != nil || reason != schema.StopReasonEndTurn {
		t.Fatalf("prompt: %v (%s)", err, reason)
	}
	select {
	case update := <-updates:
		if update.SessionID != session.SessionID || update.Update.AgentMessageChunk == nil {
			t.Fatalf("unexpected update: %+v", update)
		}
	default:
		t.Fatal("session update was not delivered")
	}
}

type loadingAgent struct {
	testAgent
	loaded chan schema.LoadSessionRequest
}

type unadvertisedResumingAgent struct{ testAgent }

func (unadvertisedResumingAgent) ResumeSession(context.Context, Client, schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	return schema.ResumeSessionResponse{}, nil
}

func (loadingAgent) Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	enabled := true
	return schema.InitializeResponse{
		ProtocolVersion:   acp.Version,
		AgentCapabilities: &schema.AgentCapabilities{LoadSession: &enabled},
	}, nil
}

func (a loadingAgent) LoadSession(_ context.Context, _ Client, request schema.LoadSessionRequest) (schema.LoadSessionResponse, error) {
	a.loaded <- request
	return schema.LoadSessionResponse{}, nil
}

func TestRuntimeDispatchesOptionalMethodsAndReportsUnsupportedMethods(t *testing.T) {
	loaded := make(chan schema.LoadSessionRequest, 1)
	connection := startRuntime(t, loadingAgent{loaded: loaded}, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initializeClient(t, connection, acp.Capabilities{})

	var loadedResult schema.LoadSessionResponse
	if err := connection.Call(ctx, schema.SessionLoadMethodName, schema.LoadSessionRequest{
		SessionID: "old", Cwd: "/tmp",
	}, &loadedResult); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-loaded:
		if request.SessionID != "old" {
			t.Fatalf("loaded session = %+v", request)
		}
	default:
		t.Fatal("optional loader was not called")
	}

	err := connection.Call(ctx, schema.SessionResumeMethodName, schema.ResumeSessionRequest{
		SessionID: "old", Cwd: "/tmp",
	}, nil)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("unsupported method error = %v", err)
	}
}

func TestRuntimeRequiresAndAllowsInitializeOnlyOnce(t *testing.T) {
	connection := startRuntime(t, testAgent{}, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := connection.Call(ctx, schema.SessionNewMethodName, schema.NewSessionRequest{
		Cwd: "/tmp", MCPServers: []schema.McpServer{},
	}, nil)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32600 || rpcErr.Message != "agent is not initialized" {
		t.Fatalf("uninitialized error = %v", err)
	}
	initializeClient(t, connection, acp.Capabilities{})
	_, err = connection.Initialize(ctx, acp.Capabilities{})
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32600 || rpcErr.Message != "agent is already initialized" {
		t.Fatalf("duplicate initialize error = %v", err)
	}
}

func TestRuntimeGatesOptionalMethodsOnAgentCapabilities(t *testing.T) {
	connection := startRuntime(t, unadvertisedResumingAgent{}, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initializeClient(t, connection, acp.Capabilities{})
	err := connection.Call(ctx, schema.SessionResumeMethodName, schema.ResumeSessionRequest{
		SessionID: "old", Cwd: "/tmp",
	}, nil)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 || rpcErr.Message != "agent does not implement method: session/resume" {
		t.Fatalf("capability gate error = %v", err)
	}
}

type cancellingAgent struct {
	testAgent
	entered   chan struct{}
	cancelled chan struct{}
}

func (a cancellingAgent) Prompt(ctx context.Context, _ Client, _ schema.PromptRequest, _ SessionUpdater) (schema.PromptResponse, error) {
	close(a.entered)
	<-ctx.Done()
	return schema.PromptResponse{}, ctx.Err()
}

func (a cancellingAgent) CancelSession(context.Context, schema.CancelNotification) error {
	a.cancelled <- struct{}{}
	return nil
}

func TestSessionCancelNotificationCancelsActivePrompt(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	connection := startRuntime(t, cancellingAgent{entered: entered, cancelled: cancelled}, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	initializeClient(t, connection, acp.Capabilities{})
	session, err := connection.NewSession(ctx, "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	promptDone := make(chan error, 1)
	go func() {
		_, err := connection.Prompt(ctx, session, "wait")
		promptDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("prompt did not start")
	}
	if err := connection.CancelSession(ctx, session.SessionID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-promptDone:
		var rpcErr *acp.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32800 {
			t.Fatalf("cancelled prompt error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("prompt was not cancelled")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("session cancellation hook was not called")
	}
}

func TestAgentClientMethodsAreGatedByAdvertisedCapabilities(t *testing.T) {
	prompt := func(ctx context.Context, client Client, request schema.PromptRequest, updates SessionUpdater) (schema.PromptResponse, error) {
		file, err := client.ReadTextFile(ctx, schema.ReadTextFileRequest{
			SessionID: request.SessionID, Path: "/tmp/source.txt",
		})
		if err != nil {
			return schema.PromptResponse{}, err
		}
		if err := updates.Update(schema.SessionUpdate{
			AgentMessageChunk: &schema.ContentChunk{Content: acp.NewTextContent(file.Content)},
		}); err != nil {
			return schema.PromptResponse{}, err
		}
		return schema.PromptResponse{StopReason: schema.StopReasonEndTurn}, nil
	}

	t.Run("advertised", func(t *testing.T) {
		updates := make(chan acp.Update, 1)
		connection := startRuntime(t, testAgent{prompt: prompt}, HandleFilesystem(nil, testClientHost{}), acp.SessionUpdates(func(update acp.Update) error {
			updates <- update
			return nil
		}))
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		initialization := initializeClient(t, connection, acp.WorkspaceCapabilities(true, false, false))
		session, err := connection.NewSession(ctx, "/tmp")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent("read")}); err != nil {
			t.Fatal(err)
		}
		select {
		case update := <-updates:
			if update.Update.AgentMessageChunk == nil || update.Update.AgentMessageChunk.Content.Text == nil || update.Update.AgentMessageChunk.Content.Text.Text != "read" {
				t.Fatalf("unexpected filesystem-backed update: %+v", update)
			}
		default:
			t.Fatal("filesystem-backed update was not delivered")
		}
	})

	t.Run("not advertised", func(t *testing.T) {
		connection := startRuntime(t, testAgent{prompt: prompt}, HandleFilesystem(nil, testClientHost{}), nil)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		initialization := initializeClient(t, connection, acp.Capabilities{})
		session, err := connection.NewSession(ctx, "/tmp")
		if err != nil {
			t.Fatal(err)
		}
		_, err = connection.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent("read")})
		var rpcErr *acp.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 || rpcErr.Message != "client did not advertise fs/read_text_file support" {
			t.Fatalf("capability gate error = %v", err)
		}
	})
}
