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
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// A session/resume override reaches the connection's Client through
// ClientFromContext, the one the stable dispatch passes to the Agent, and the
// client receives the updates it sends, in order, before the response.
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
	runtime := New(&clientRecordingAgent{record: func(client Client) { stable = client }})
	runtime.Overrides = map[string]acp.Handler{
		schema.SessionResumeMethodName: func(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
			client, ok := ClientFromContext(ctx)
			if !ok || client != stable {
				return nil, errors.New("override has no connection client on its context")
			}
			var request schema.ResumeSessionRequest
			if err := json.Unmarshal(raw, &request); err != nil {
				return nil, err
			}
			for _, update := range []schema.SessionUpdate{
				{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}},
				{StateUpdate: &schema.StateUpdate{Idle: &schema.IdleStateUpdate{}}},
			} {
				if err := client.Notify(ctx, schema.SessionUpdateMethodName, schema.UpdateSessionNotification{SessionID: request.SessionID, Update: update}); err != nil {
					return nil, err
				}
			}
			return schema.ResumeSessionResponse{}, nil
		},
	}
	runtime.Extensions = func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		if client, ok := ClientFromContext(ctx); !ok || client != stable {
			return nil, errors.New("extension has no connection client on its context")
		}
		return map[string]any{}, nil
	}
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), peer, peer) }()
	client := acpv2.Connect(local, local, nil, func(method string, raw json.RawMessage) error {
		if method != schema.SessionUpdateMethodName {
			return nil
		}
		var notification schema.UpdateSessionNotification
		if err := json.Unmarshal(raw, &notification); err != nil {
			return err
		}
		state := "other"
		if update := notification.Update.StateUpdate; update != nil && update.Running != nil {
			state = "running"
		} else if update != nil && update.Idle != nil {
			state = "idle"
		}
		record(string(notification.SessionID) + ":" + state)
		return nil
	})
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	if _, err := client.Initialize(ctx, acpv2.Capabilities{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := ClientFromContext(ctx); ok {
		t.Fatal("ClientFromContext found a client outside a request")
	}

	if err := client.Call(ctx, schema.SessionResumeMethodName, map[string]any{"sessionId": "s", "cwd": hostRoot}, nil); err != nil {
		t.Fatalf("overridden session/resume: %v", err)
	}
	record("response")
	if err := client.Call(ctx, "_vendor/extension", map[string]any{}, nil); err != nil {
		t.Fatalf("extension: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(events, ","); got != "s:running,s:idle,response" {
		t.Fatalf("client saw %s, want s:running,s:idle,response", got)
	}
}

// clientRecordingAgent records the Client the runtime passes to Initialize,
// the same one its stable dispatch passes to every Agent method.
type clientRecordingAgent struct {
	echoAgent
	record func(Client)
}

func (a *clientRecordingAgent) Initialize(ctx context.Context, client Client, request schema.InitializeRequest) (schema.InitializeResponse, error) {
	a.record(client)
	return a.echoAgent.Initialize(ctx, client, request)
}
