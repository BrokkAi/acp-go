package v2

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

func TestResumeSessionFromStartAppliesReplayBeforeResponse(t *testing.T) {
	tracker := NewSessionTracker()
	var serverConnection *acp.Connection
	client, serverConnection := pipeClientWithServerNotifications(t,
		func(_ context.Context, method string, raw json.RawMessage) (any, error) {
			switch method {
			case schema.InitializeMethodName:
				return sessionInitialization(), nil
			case schema.SessionResumeMethodName:
				var request schema.ResumeSessionRequest
				if err := json.Unmarshal(raw, &request); err != nil {
					return nil, err
				}
				if request.SessionID != "replay-session" || request.ReplayFrom == nil || request.ReplayFrom.Start == nil {
					return nil, &acp.RPCError{Code: -32602, Message: "replay request is not from start"}
				}
				for _, update := range []schema.SessionUpdate{
					{StateUpdate: &schema.StateUpdate{Running: &schema.RunningStateUpdate{}}},
					{AgentMessageChunk: &schema.ContentChunk{
						MessageID: "replay",
						Content:   schema.ContentBlock{Text: &schema.TextContent{Text: "replayed text"}},
					}},
					{StateUpdate: &schema.StateUpdate{Idle: &schema.IdleStateUpdate{}}},
				} {
					if err := serverConnection.Notify(context.Background(), schema.SessionUpdateMethodName, Update{
						SessionID: request.SessionID,
						Update:    update,
					}); err != nil {
						return nil, err
					}
				}
				return schema.ResumeSessionResponse{}, nil
			default:
				return nil, &acp.RPCError{Code: -32601}
			}
		},
		tracker.TrackerNotifications(nil),
		nil,
	)
	ctx := context.Background()
	initialization, err := client.InitializeWithInfo(ctx, Capabilities{}, ClientInfo{Name: "resume-client", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ResumeSessionFromStart(ctx, initialization, "replay-session", "/repo", nil); err != nil {
		t.Fatal(err)
	}
	work := tracker.Work("replay-session")
	select {
	case <-work.Done():
		if work.Result().Text != "replayed text" {
			t.Fatalf("replay text = %q", work.Result().Text)
		}
	default:
		t.Fatal("resume response returned before replay updates were applied")
	}
}
