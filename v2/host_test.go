package v2

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

func TestHandleClientHostDispatchesTypedRequests(t *testing.T) {
	permissionRequest := `{"sessionId":"s","title":"Run tests","options":[
		{"optionId":"allow","name":"Allow","kind":"allow_once"}
	]}`
	elicitationRequest := `{
		"mode":"url",
		"sessionId":"s",
		"elicitationId":"el",
		"url":"https://example.test/login",
		"message":"Log in"
	}`
	seen := ""
	handler := HandleClientHost(
		permissionFunc(func(context.Context, schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
			seen = "permission"
			return CancelPermission(), nil
		}),
		elicitationFunc(func(context.Context, schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error) {
			seen = "elicitation"
			return DeclineElicitation(), nil
		}),
	)

	if _, err := handler(context.Background(), schema.SessionRequestPermissionMethodName, json.RawMessage(permissionRequest)); err != nil {
		t.Fatal(err)
	}
	if seen != "permission" {
		t.Fatalf("seen = %q", seen)
	}
	if _, err := handler(context.Background(), schema.ElicitationCreateMethodName, json.RawMessage(elicitationRequest)); err != nil {
		t.Fatal(err)
	}
	if seen != "elicitation" {
		t.Fatalf("seen after elicitation = %q", seen)
	}
	_, err := handler(context.Background(), "future/request", json.RawMessage(`{}`))
	var rpcErr *acp.RPCError
	if err == nil {
		t.Fatal("expected unsupported method")
	}
	if _, ok := err.(*acp.RPCError); !ok {
		t.Fatalf("error = %T", err)
	}
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("code = %d", rpcErr.Code)
	}
}

func TestElicitationHelpersAndNotificationAdapters(t *testing.T) {
	completed := ""
	session := ""
	notifications := CombineNotifications(
		ElicitationComplete(func(notification schema.CompleteElicitationNotification) error {
			completed = string(notification.ElicitationID)
			return nil
		}),
		SessionUpdates(func(update Update) error {
			session = string(update.SessionID)
			return nil
		}),
	)
	if err := notifications(schema.ElicitationCompleteMethodName, json.RawMessage(`{"elicitationId":"el"}`)); err != nil {
		t.Fatal(err)
	}
	if err := notifications(schema.SessionUpdateMethodName, json.RawMessage(`{
		"sessionId":"s",
		"update":{"sessionUpdate":"agent_message","messageId":"m"}
	}`)); err != nil {
		t.Fatal(err)
	}
	if completed != "el" || session != "s" {
		t.Fatalf("completed=%q session=%q", completed, session)
	}
	if err := notifications("future/notification", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("unknown notification errored: %v", err)
	}

	encoded, err := json.Marshal(AcceptElicitation(map[string]schema.ElicitationContentValue{
		"ticket": "ACP-1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"action":"accept","content":{"ticket":"ACP-1"}}` {
		t.Fatalf("accepted elicitation = %s", encoded)
	}
}

type permissionFunc func(context.Context, schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error)

func (f permissionFunc) RequestPermission(ctx context.Context, request schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
	return f(ctx, request)
}

type elicitationFunc func(context.Context, schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error)

func (f elicitationFunc) CreateElicitation(ctx context.Context, request schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error) {
	return f(ctx, request)
}
