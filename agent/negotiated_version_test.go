package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/schema"
)

// askingAgent asks for permission while it initializes.
type askingAgent struct {
	testAgent
}

func (a askingAgent) Initialize(ctx context.Context, client Client, request schema.InitializeRequest) (schema.InitializeResponse, error) {
	if _, err := client.RequestPermission(ctx, schema.RequestPermissionRequest{SessionID: "s"}); err != nil {
		return schema.InitializeResponse{}, err
	}
	return a.testAgent.Initialize(ctx, client, request)
}

// TestRuntimeSendsBeforeInitializationLikeTheUncheckedV1Agent: the reference
// SDK's v1 agent does not gate its own requests on initialization.
func TestRuntimeSendsBeforeInitializationLikeTheUncheckedV1Agent(t *testing.T) {
	asked := make(chan string, 1)
	connection := startRuntime(t, askingAgent{}, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		asked <- method
		return json.RawMessage(`{"outcome":{"outcome":"cancelled"}}`), nil
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := connection.Call(ctx, schema.InitializeMethodName, schema.InitializeRequest{ProtocolVersion: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if method := <-asked; method != schema.SessionRequestPermissionMethodName {
		t.Fatalf("asked %s", method)
	}
}

// TestRuntimeSetsTheNegotiatedVersionOnTheResponse: the reference SDK
// overwrites the agent's protocolVersion with the negotiated one, so a v1
// agent that answers with another version still answers v1.
func TestRuntimeSetsTheNegotiatedVersionOnTheResponse(t *testing.T) {
	implementation := newGatedInitializeAgent()
	connection := startRuntime(t, implementation, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan json.RawMessage, 1)
	errs := make(chan error, 1)
	go func() {
		var raw json.RawMessage
		errs <- connection.Call(ctx, schema.InitializeMethodName, schema.InitializeRequest{ProtocolVersion: 1}, &raw)
		result <- raw
	}()
	<-implementation.entered
	implementation.release <- initializeOutcome{response: schema.InitializeResponse{ProtocolVersion: 7}}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	var selected struct {
		ProtocolVersion json.RawMessage `json:"protocolVersion"`
	}
	if err := json.Unmarshal(<-result, &selected); err != nil || string(selected.ProtocolVersion) != "1" {
		t.Fatalf("protocolVersion = %s, %v", selected.ProtocolVersion, err)
	}
	if err := newSessionCall(ctx, connection); err != nil {
		t.Fatalf("session/new after initialize: %v", err)
	}
}
