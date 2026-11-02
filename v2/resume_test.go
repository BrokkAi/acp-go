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
	if _, err := client.ResumeSessionFromStart(ctx, initialization, "replay-session", hostRoot+"/repo", nil); err != nil {
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

// A from-start replay rebuilds the session's state instead of merging into
// what an earlier replay left: keys and entities the new history no longer
// has do not survive, and the timeline is not duplicated.
func TestTrackerResumeFromStartRebuildsProjection(t *testing.T) {
	tracker := NewSessionTracker()
	histories := [][]string{
		{
			`{"sessionUpdate":"tool_call_update","toolCallId":"t","title":"Old","_meta":{"stale":true,"a":1}}`,
			`{"sessionUpdate":"agent_message_chunk","messageId":"old","content":{"type":"text","text":"gone"}}`,
		},
		{
			`{"sessionUpdate":"tool_call_update","toolCallId":"t","_meta":{"a":2}}`,
		},
	}
	replays := 0
	var serverConnection *acp.Connection
	client, serverConnection := pipeClientWithServerNotifications(t,
		func(_ context.Context, method string, raw json.RawMessage) (any, error) {
			switch method {
			case schema.InitializeMethodName:
				return sessionInitialization(), nil
			case schema.SessionResumeMethodName:
				request := decode[schema.ResumeSessionRequest](t, raw)
				for _, update := range histories[replays] {
					if err := serverConnection.Notify(context.Background(), schema.SessionUpdateMethodName,
						json.RawMessage(`{"sessionId":"`+string(request.SessionID)+`","update":`+update+`}`)); err != nil {
						return nil, err
					}
				}
				replays++
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
	for range histories {
		if _, err := tracker.ResumeSessionFromStart(ctx, client, initialization, "s", hostRoot+"/repo", nil); err != nil {
			t.Fatal(err)
		}
	}
	projection := tracker.Projection("s")
	call, _ := projection.ToolCall("t")
	assertWire(t, call, `{"toolCallId":"t","_meta":{"a":2}}`)
	if got := projection.Timeline(); len(got) != 1 || projection.Text() != "" {
		t.Fatalf("timeline after second replay = %v, text %q", got, projection.Text())
	}

	tracker.ResetProjection("s")
	if got := tracker.Projection("s").Timeline(); len(got) != 0 {
		t.Fatalf("ResetProjection kept %v", got)
	}
}
