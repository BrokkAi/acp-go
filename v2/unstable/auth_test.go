package unstable_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	"github.com/BrokkAi/acp-go/v2/agent"
	acpunstable "github.com/BrokkAi/acp-go/v2/unstable"
)

func authStatusInitialization() schema.InitializeResponse {
	return schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{
		Auth: &schema.AgentAuthCapabilities{Status: &schema.AuthStatusCapabilities{}},
	}}
}

func set[T any](value T) schema.Nullable[T] { return schema.Nullable[T]{Set: true, Value: value} }

func TestAuthStatusGatesBeforeTheWire(t *testing.T) {
	ctx := context.Background()
	connection, requests := rawPeer(t, func(string) string { return `{"state":"authenticated"}` })
	for _, initialization := range []schema.InitializeResponse{
		{},
		{Capabilities: &schema.AgentCapabilities{}},
		{Capabilities: &schema.AgentCapabilities{Auth: &schema.AgentAuthCapabilities{}}},
	} {
		if _, err := acpunstable.AuthStatus(ctx, connection, initialization, schema.AuthStatusRequest{}); err == nil || !strings.Contains(err.Error(), "auth/status") {
			t.Fatalf("AuthStatus without the capability = %v", err)
		}
	}
	select {
	case request := <-requests:
		t.Fatalf("a refused call wrote %s to the wire", request.method)
	default:
	}

	response, err := acpunstable.AuthStatus(ctx, connection, authStatusInitialization(), schema.AuthStatusRequest{})
	if err != nil || response.State != schema.AuthStateAuthenticated {
		t.Fatalf("AuthStatus = %+v, %v", response, err)
	}
	if request := <-requests; request.method != schema.AuthStatusMethodName || string(request.params) != `{}` {
		t.Fatalf("wrote %s %s", request.method, request.params)
	}
}

func TestAuthStatusDecodesTheResponse(t *testing.T) {
	ctx := context.Background()
	results := make(chan string, 1)
	connection, requests := rawPeer(t, func(string) string { return <-results })
	for _, c := range []struct {
		wire string
		want schema.AuthStatusResponse
	}{
		{`{"state":"unauthenticated"}`, schema.AuthStatusResponse{State: schema.AuthStateUnauthenticated}},
		{`{"state":"authenticated","accounts":[{"label":"Pro plan","active":true,"email":"user@example.com"}]}`, schema.AuthStatusResponse{
			State:    schema.AuthStateAuthenticated,
			Accounts: []schema.AuthAccount{{Label: "Pro plan", Active: set(true), Email: set("user@example.com")}},
		}},
		{`{"state":"authenticated","accounts":[{"label":"OpenAI API key","active":true},{"label":"Vertex AI"},{"label":"GitHub Copilot","name":"octocat"}]}`, schema.AuthStatusResponse{
			State: schema.AuthStateAuthenticated,
			Accounts: []schema.AuthAccount{
				{Label: "OpenAI API key", Active: set(true)},
				{Label: "Vertex AI"},
				{Label: "GitHub Copilot", Name: set("octocat")},
			},
		}},
		// An unknown state is preserved; clients treat it as unknown.
		{`{"state":"_vendor_pending"}`, schema.AuthStatusResponse{State: "_vendor_pending"}},
		// Display information is lenient: a malformed entry is skipped.
		{`{"state":"not_required","accounts":[{"name":"no label"},{"label":"kept","active":"yes"}]}`, schema.AuthStatusResponse{
			State:    schema.AuthStateNotRequired,
			Accounts: []schema.AuthAccount{{Label: "kept"}},
		}},
	} {
		results <- c.wire
		response, err := acpunstable.AuthStatus(ctx, connection, authStatusInitialization(), schema.AuthStatusRequest{})
		<-requests
		if err != nil || !reflect.DeepEqual(response, c.want) {
			t.Fatalf("%s = %+v, %v; want %+v", c.wire, response, err, c.want)
		}
	}
	for _, wire := range []string{`{}`, `{"state":null}`, `{"state":5}`} {
		results <- wire
		if _, err := acpunstable.AuthStatus(ctx, connection, authStatusInitialization(), schema.AuthStatusRequest{}); err == nil {
			t.Fatalf("%s decoded without a state", wire)
		}
		<-requests
	}
}

// authAgent serves auth/status, and pushes auth/status_update from a vendor
// sign-in method as an agent would after its credentials change.
type authAgent struct {
	*acptest.V2Agent
	state schema.AuthState
}

func (a *authAgent) AuthStatus(context.Context, schema.AuthStatusRequest) (schema.AuthStatusResponse, error) {
	return schema.AuthStatusResponse{State: a.state, Accounts: []schema.AuthAccount{{Label: "Key", Active: set(true)}}}, nil
}

// signIn is an Extensions handler that sends auth/status_update through the
// client the runtime puts on the request context, before it answers.
func signIn(ctx context.Context, method string, _ json.RawMessage) (any, error) {
	if method != "_vendor/sign_in" {
		data, _ := json.Marshal(method)
		return nil, &acp.RPCError{Code: -32601, Message: "Method not found", Data: data}
	}
	client, ok := agent.ClientFromContext(ctx)
	if !ok {
		return nil, &acp.RPCError{Code: -32603, Message: "no client"}
	}
	err := acpunstable.NotifyAuthStatusUpdate(ctx, client, schema.AuthStatusUpdateNotification{
		State:    schema.AuthStateAuthenticated,
		Accounts: []schema.AuthAccount{{Label: "Signed in"}},
	})
	if err != nil {
		return nil, &acp.RPCError{Code: -32603, Message: err.Error()}
	}
	return map[string]any{}, nil
}

// serveAuth runs the v2 runtime with signIn as its Extensions and Install,
// and returns a client connection whose notifications go to notifications.
func serveAuth(t *testing.T, implementation agent.Agent, notifications acp.Notifications) *acpv2.Connection {
	t.Helper()
	pair := acptest.NewPairWithDeadline(time.Now().Add(5 * time.Second))
	runtime := agent.New(implementation)
	runtime.Extensions = signIn
	if err := acpunstable.Install(runtime, implementation); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), pair.B, pair.B) }()
	client := acpv2.Connect(pair.A, pair.A, nil, notifications)
	t.Cleanup(func() {
		_ = client.Close()
		pair.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	return client
}

func advertisesAuthStatus(initialization schema.InitializeResponse) bool {
	capabilities := initialization.Capabilities
	return capabilities != nil && capabilities.Auth != nil && capabilities.Auth.Status != nil
}

func TestRuntimeServesAndPushesAuthStatus(t *testing.T) {
	ctx := context.Background()
	updates := make(chan schema.AuthStatusUpdateNotification, 4)
	var other []string
	notifications := acpv2.CombineNotifications(
		func(method string, _ json.RawMessage) error { other = append(other, method); return nil },
		acpunstable.AuthStatusUpdates(func(update schema.AuthStatusUpdateNotification) error {
			updates <- update
			return nil
		}),
	)
	connection := serveAuth(t, &authAgent{V2Agent: &acptest.V2Agent{}, state: schema.AuthStateUnauthenticated}, notifications)

	initialization := initialize(t, connection)
	if !advertisesAuthStatus(initialization) {
		t.Fatalf("Install did not announce auth.status: %+v", initialization.Capabilities)
	}
	response, err := acpunstable.AuthStatus(ctx, connection, initialization, schema.AuthStatusRequest{})
	if err != nil || response.State != schema.AuthStateUnauthenticated || len(response.Accounts) != 1 || response.Accounts[0].Label != "Key" {
		t.Fatalf("auth/status = %+v, %v", response, err)
	}

	// Notifications run before the response that follows them is delivered.
	if err := connection.Call(ctx, "_vendor/sign_in", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-updates:
		if update.State != schema.AuthStateAuthenticated || len(update.Accounts) != 1 || update.Accounts[0].Label != "Signed in" {
			t.Fatalf("auth/status_update = %+v", update)
		}
	default:
		t.Fatal("auth/status_update was not delivered before the sign-in response")
	}
	if len(other) != 1 || other[0] != schema.AuthStatusUpdateMethodName {
		t.Fatalf("composed notification handler saw %v", other)
	}
}

func TestAuthStatusDispatchAndMethodNotFound(t *testing.T) {
	ctx := context.Background()

	// An agent without AuthStatusHandler does not announce the capability,
	// and auth/status answers -32601 with the method name as data.
	updates := make(chan schema.AuthStatusUpdateNotification, 1)
	connection := serveAuth(t, &acptest.V2Agent{}, acpunstable.AuthStatusUpdates(func(update schema.AuthStatusUpdateNotification) error {
		updates <- update
		return nil
	}))
	initialization := initialize(t, connection)
	if advertisesAuthStatus(initialization) {
		t.Fatalf("auth.status announced without a handler: %+v", initialization.Capabilities)
	}
	rpc := rpcError(t, connection.Call(ctx, schema.AuthStatusMethodName, schema.AuthStatusRequest{}, nil))
	if rpc.Code != -32601 || rpc.Message != "Method not found" || string(rpc.Data) != `"auth/status"` {
		t.Fatalf("auth/status without a handler = %+v (data %s)", rpc, rpc.Data)
	}
	// Inside a request on a connection that did not announce auth.status,
	// the update is refused before it reaches the wire.
	rpc = rpcError(t, connection.Call(ctx, "_vendor/sign_in", map[string]any{}, nil))
	if !strings.Contains(rpc.Message, "did not advertise auth/status") {
		t.Fatalf("auth/status_update without the capability = %+v", rpc)
	}
	select {
	case update := <-updates:
		t.Fatalf("refused update reached the client: %+v", update)
	default:
	}

	handler := acpunstable.Handle(nil, &authAgent{state: "authenticated"})
	_, err := handler(ctx, schema.AuthStatusMethodName, json.RawMessage(`5`))
	if rpc := rpcError(t, err); rpc.Code != -32602 || !strings.Contains(string(rpc.Data), `"json":5`) {
		t.Fatalf("malformed params = %+v (data %s)", rpc, rpc.Data)
	}
	// Absent params, null, and a malformed _meta are the default request, as
	// in the reference SDK.
	for _, params := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`{"_meta":5}`)} {
		served, err := handler(ctx, schema.AuthStatusMethodName, params)
		if err != nil || served.(schema.AuthStatusResponse).State != schema.AuthStateAuthenticated {
			t.Fatalf("auth/status with params %s = %v, %v", params, served, err)
		}
	}
}

func TestNotifyAuthStatusUpdateChecks(t *testing.T) {
	ctx := context.Background()
	if err := acpunstable.NotifyAuthStatusUpdate(ctx, nil, schema.AuthStatusUpdateNotification{State: schema.AuthStateUnknown}); err == nil {
		t.Fatal("sent without a client")
	}
	client := &recordingClient{}
	// Outside a dispatched request there is no initialize response to check.
	if err := acpunstable.NotifyAuthStatusUpdate(ctx, client, schema.AuthStatusUpdateNotification{State: schema.AuthStateUnknown}); err != nil || len(client.methods) != 1 || client.methods[0] != schema.AuthStatusUpdateMethodName {
		t.Fatalf("NotifyAuthStatusUpdate = %v, sent %v", err, client.methods)
	}

	if err := acpunstable.AuthStatusUpdates(func(schema.AuthStatusUpdateNotification) error {
		t.Fatal("malformed update reached the handler")
		return nil
	})(schema.AuthStatusUpdateMethodName, json.RawMessage(`{"accounts":[]}`)); err == nil {
		t.Fatal("update without a state decoded")
	}
}

type recordingClient struct {
	agent.Client
	methods []string
}

func (c *recordingClient) Notify(_ context.Context, method string, _ any) error {
	c.methods = append(c.methods, method)
	return nil
}
