package acp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/schema"
)

func TestElicitationHostRoundTrip(t *testing.T) {
	requests := 0
	handler := HandleElicitation(nil, func(ctx context.Context, request schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error) {
		requests++
		if request.Form == nil || request.Form.Session == nil || request.Form.Session.SessionID != "session" {
			t.Fatalf("unexpected elicitation request: %+v", request)
		}
		return AcceptElicitation(map[string]schema.ElicitationContentValue{"name": "Ada"}), nil
	})
	completions := make(chan schema.CompleteElicitationNotification, 1)
	notifications := CombineNotifications(
		ElicitationComplete(func(notification schema.CompleteElicitationNotification) error {
			completions <- notification
			return nil
		}),
	)
	_, peer := pipeClient(t, handler, notifications)
	peerDone := make(chan error, 1)
	go func() {
		defer close(peerDone)
		encoder := json.NewEncoder(peer)
		decoder := json.NewDecoder(peer)
		request := packet{
			Version: "2.0",
			ID:      json.RawMessage(`"elicitation"`),
			Method:  schema.ElicitationCreateMethodName,
			Params: json.RawMessage(`{
				"mode": "form",
				"message": "Who should review?",
				"sessionId": "session",
				"schema": {"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}
			}`),
		}
		if err := encoder.Encode(request); err != nil {
			peerDone <- err
			return
		}
		var response packet
		if err := decoder.Decode(&response); err != nil {
			peerDone <- err
			return
		}
		var got, want any
		if err := json.Unmarshal(response.Result, &got); err != nil {
			peerDone <- err
			return
		}
		if err := json.Unmarshal([]byte(`{"action":"accept","content":{"name":"Ada"}}`), &want); err != nil {
			peerDone <- err
			return
		}
		if !reflect.DeepEqual(got, want) {
			peerDone <- errors.New("unexpected elicitation response")
			return
		}
		complete := packet{
			Version: "2.0",
			Method:  schema.ElicitationCompleteMethodName,
			Params:  json.RawMessage(`{"elicitationId":"elicitation"}`),
		}
		if err := encoder.Encode(complete); err != nil {
			peerDone <- err
			return
		}
	}()
	select {
	case notification := <-completions:
		if notification.ElicitationID != "elicitation" {
			t.Fatalf("unexpected completion: %+v", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("elicitation completion was not delivered")
	}
	if requests != 1 {
		t.Fatalf("handled %d elicitation requests, want 1", requests)
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
}

func TestElicitationOutcomeConstructors(t *testing.T) {
	tests := []struct {
		name string
		call func() schema.CreateElicitationResponse
		want string
	}{
		{"accept", func() schema.CreateElicitationResponse { return AcceptElicitation(nil) }, `{"action":"accept"}`},
		{"decline", DeclineElicitation, `{"action":"decline"}`},
		{"cancel", CancelElicitation, `{"action":"cancel"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.call())
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(test.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %s, want %s", encoded, test.want)
			}
		})
	}
}
