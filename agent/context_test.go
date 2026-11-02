package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
)

// A session/load override streams its replay through the connection's Client
// from ClientFromContext, and the client receives every update, in order,
// before the response, as it does from the stable LoadSession.
func TestRuntimeOverrideStreamsUpdatesThroughTheContextClient(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	var stable Client
	runtime := New(clientRecordingAgent{record: func(client Client) { stable = client }})
	runtime.Overrides = map[string]acp.Handler{
		schema.SessionLoadMethodName: func(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
			client, ok := ClientFromContext(ctx)
			if !ok {
				return nil, errors.New("override has no client on its context")
			}
			if client != stable {
				return nil, errors.New("override client is not the one the stable dispatch passes")
			}
			var request schema.LoadSessionRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				return nil, err
			}
			for _, text := range []string{"first", "second"} {
				if err := client.Notify(ctx, schema.SessionUpdateMethodName, schema.SessionNotification{
					SessionID: request.SessionID,
					Update:    schema.SessionUpdate{UserMessageChunk: &schema.ContentChunk{Content: acp.NewTextContent(text)}},
				}); err != nil {
					return nil, err
				}
			}
			return schema.LoadSessionResponse{}, nil
		},
	}
	runtime.Extensions = func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		if client, ok := ClientFromContext(ctx); !ok || client != stable {
			return nil, errors.New("extension has no connection client on its context")
		}
		return map[string]any{}, nil
	}
	runtime.InitializeResult = func(response schema.InitializeResponse) (any, error) {
		loadSession := true
		response.AgentCapabilities = &schema.AgentCapabilities{LoadSession: &loadSession}
		return response, nil
	}
	agentSide, clientSide := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = agentSide.SetDeadline(deadline)
	_ = clientSide.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), agentSide, agentSide) }()
	t.Cleanup(func() {
		_ = clientSide.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	connection := acp.Connect(clientSide, clientSide, nil, acp.SessionUpdates(func(update acp.Update) error {
		chunk := update.Update.UserMessageChunk
		if chunk == nil || chunk.Content.Text == nil {
			return errors.New("unexpected update")
		}
		record(string(update.SessionID) + ":" + chunk.Content.Text.Text)
		return nil
	}))
	initializeClient(t, connection, acp.Capabilities{})
	if _, ok := ClientFromContext(ctx); ok {
		t.Fatal("ClientFromContext found a client outside a request")
	}

	request := schema.LoadSessionRequest{SessionID: "s", Cwd: hostRoot, MCPServers: []schema.McpServer{}}
	if err := connection.Call(ctx, schema.SessionLoadMethodName, request, nil); err != nil {
		t.Fatalf("overridden session/load: %v", err)
	}
	record("response")
	if err := connection.Call(ctx, "_vendor/extension", map[string]any{}, nil); err != nil {
		t.Fatalf("extension: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(events, ","); got != "s:first,s:second,response" {
		t.Fatalf("client saw %s, want s:first,s:second,response", got)
	}
}

// clientRecordingAgent records the Client the runtime passes to Initialize,
// the same one its stable dispatch passes to every Agent method.
type clientRecordingAgent struct {
	testAgent
	record func(Client)
}

func (a clientRecordingAgent) Initialize(ctx context.Context, client Client, request schema.InitializeRequest) (schema.InitializeResponse, error) {
	a.record(client)
	return a.testAgent.Initialize(ctx, client, request)
}
