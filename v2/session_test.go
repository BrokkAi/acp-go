package v2

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

func TestCancellablePermissionsResolveCancellationOutcome(t *testing.T) {
	entered := make(chan struct{})
	permissions := NewCancellablePermissions(permissionFunc(func(ctx context.Context, request schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
		close(entered)
		<-ctx.Done()
		return schema.RequestPermissionResponse{}, ctx.Err()
	}))
	request := schema.RequestPermissionRequest{
		SessionID: "session",
		Title:     "Run command",
		Options: []schema.PermissionOption{{
			OptionID: "allow", Name: "Allow", Kind: schema.PermissionOptionKindAllowOnce,
		}},
	}
	response := make(chan schema.RequestPermissionResponse, 1)
	go func() {
		result, err := permissions.RequestPermission(context.Background(), request)
		if err != nil {
			panic(err)
		}
		response <- result
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("permission handler did not start")
	}
	if count := permissions.CancelPermissionRequests("session"); count != 1 {
		t.Fatalf("cancelled permissions = %d", count)
	}
	select {
	case result := <-response:
		if result.Outcome.Cancelled == nil {
			t.Fatalf("cancelled outcome = %+v", result.Outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled permission did not resolve")
	}
}

func TestSessionHandleCommandsAndCancellation(t *testing.T) {
	tracker := NewSessionTracker()
	var serverConnection *acp.Connection
	promptCount := 0
	client, serverConnection := pipeClientWithServerNotifications(t,
		func(_ context.Context, method string, raw json.RawMessage) (any, error) {
			switch method {
			case schema.InitializeMethodName:
				return sessionInitialization(), nil
			case schema.SessionNewMethodName:
				return schema.NewSessionResponse{SessionID: "command-session"}, nil
			case schema.SessionPromptMethodName:
				promptCount++
				if err := serverConnection.Notify(context.Background(), schema.SessionUpdateMethodName, Update{
					SessionID: "command-session",
					Update: schema.SessionUpdate{StateUpdate: &schema.StateUpdate{
						Running: &schema.RunningStateUpdate{},
					}},
				}); err != nil {
					return nil, err
				}
				if promptCount == 1 {
					if err := serverConnection.Notify(context.Background(), schema.SessionUpdateMethodName, Update{
						SessionID: "command-session",
						Update: schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
							MessageID: "message",
							Content:   schema.ContentBlock{Text: &schema.TextContent{Text: "hello session handle"}},
						}},
					}); err != nil {
						return nil, err
					}
					reason := schema.StopReasonEndTurn
					if err := serverConnection.Notify(context.Background(), schema.SessionUpdateMethodName, Update{
						SessionID: "command-session",
						Update: schema.SessionUpdate{StateUpdate: &schema.StateUpdate{
							Idle: &schema.IdleStateUpdate{StopReason: &reason},
						}},
					}); err != nil {
						return nil, err
					}
				}
				return schema.PromptResponse{}, nil
			case schema.SessionSetConfigOptionMethodName:
				var request schema.SetSessionConfigOptionRequest
				if err := json.Unmarshal(raw, &request); err != nil {
					return nil, err
				}
				if request.SessionID != "command-session" || request.ConfigID != "verbose" ||
					request.Boolean == nil || !request.Boolean.Value {
					return nil, errors.New("unexpected config request")
				}
				return schema.SetSessionConfigOptionResponse{
					ConfigOptions: []schema.SessionConfigOption{{
						ConfigID: "verbose",
						Name:     "Verbose",
						Boolean:  &schema.SessionConfigBoolean{CurrentValue: true},
					}},
				}, nil
			case schema.SessionCloseMethodName:
				return schema.CloseSessionResponse{}, nil
			default:
				return nil, &acp.RPCError{Code: -32601}
			}
		},
		tracker.TrackerNotifications(nil),
		func(method string, raw json.RawMessage) error {
			if method != schema.SessionCancelMethodName {
				return nil
			}
			var notification schema.CancelSessionNotification
			if err := json.Unmarshal(raw, &notification); err != nil {
				return err
			}
			reason := schema.StopReasonCancelled
			return serverConnection.Notify(context.Background(), schema.SessionUpdateMethodName, Update{
				SessionID: notification.SessionID,
				Update: schema.SessionUpdate{StateUpdate: &schema.StateUpdate{
					Idle: &schema.IdleStateUpdate{StopReason: &reason},
				}},
			})
		},
	)

	ctx := context.Background()
	initialization, err := client.InitializeWithInfo(ctx, Capabilities{}, ClientInfo{Name: "command-client", Version: "1"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSessionWithOptions(ctx, initialization, "/repo", NewSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := NewSessionHandle(client, initialization, session, SessionHandleOptions{Tracker: tracker})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Prompt(ctx, "hello"); err != nil {
		t.Fatal(err)
	}
	result, err := handle.WaitForIdle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "hello session handle" || result.StopReason == nil || *result.StopReason != schema.StopReasonEndTurn {
		t.Fatalf("first work result = %+v", result)
	}

	options, err := handle.SetConfigOptionBoolean(ctx, "verbose", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 1 || options[0].ConfigID != "verbose" || options[0].Boolean == nil || !options[0].Boolean.CurrentValue {
		t.Fatalf("options = %+v", options)
	}

	if err := handle.Prompt(ctx, "long work"); err != nil {
		t.Fatal(err)
	}
	cancelled, err := handle.CancelActiveWorkAndWait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.StopReason == nil || *cancelled.StopReason != schema.StopReasonCancelled {
		t.Fatalf("cancelled result = %+v", cancelled)
	}
	if err := handle.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func pipeClientWithServerNotifications(t *testing.T, handler acp.Handler, clientNotifications acp.Notifications, serverNotifications acp.Notifications) (*Connection, *acp.Connection) {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	server := acp.Connect(peer, peer, handler, serverNotifications)
	client := Connect(local, local, nil, clientNotifications)
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		_ = peer.Close()
	})
	return client, server
}
