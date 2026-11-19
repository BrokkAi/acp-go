package unstable_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go"
	stable "github.com/BrokkAi/acp-go/schema"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
	acpunstable "github.com/BrokkAi/acp-go/unstable"
)

func optionsInitialization() schema.InitializeResponse {
	return schema.InitializeResponse{AgentCapabilities: &schema.AgentCapabilities{
		SessionCapabilities: &schema.SessionCapabilities{
			Options: &schema.SessionOptionsCapabilities{},
			Resume:  &schema.SessionResumeCapabilities{},
		},
	}}
}

// TestSessionOptionsGatesAndSends covers session/options (schema crate 1.17,
// Rust agent-client-protocol 5.2): it needs sessionCapabilities.options, an
// absolute cwd, and for a session ID non-blank text and session/load or
// session/resume support; initial configOptions on the setup requests need
// the same capability.
func TestSessionOptionsGatesAndSends(t *testing.T) {
	ctx := context.Background()
	connection, requests := fixture(t, func(method string) any {
		if method == schema.SessionNewMethodName {
			return map[string]any{"sessionId": "new"}
		}
		return map[string]any{"configOptions": []any{}}
	})
	session := schema.SessionId("s")
	blank := schema.SessionId(" ")
	noResume := optionsInitialization()
	noResume.AgentCapabilities.SessionCapabilities.Resume = nil
	for name, tc := range map[string]struct {
		initialization schema.InitializeResponse
		request        schema.SessionOptionsRequest
		want           string
	}{
		"not advertised":       {schema.InitializeResponse{}, schema.SessionOptionsRequest{Cwd: "/w"}, "did not advertise session/options"},
		"relative cwd":         {optionsInitialization(), schema.SessionOptionsRequest{Cwd: "w"}, "must be absolute"},
		"blank session":        {optionsInitialization(), schema.SessionOptionsRequest{Cwd: "/w", SessionID: &blank}, "session ID"},
		"session without load": {noResume, schema.SessionOptionsRequest{Cwd: "/w", SessionID: &session}, "session/load or session/resume"},
		"additional dirs":      {optionsInitialization(), schema.SessionOptionsRequest{Cwd: "/w", AdditionalDirectories: []string{"/x"}}, "additionalDirectories"},
	} {
		if _, err := acpunstable.SessionOptions(ctx, connection, tc.initialization, tc.request); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	initial := []schema.InitialSessionConfigOption{{ConfigID: "model", ValueID: &schema.InitialSessionConfigOptionValueID{Value: "fast"}}}
	if _, err := acpunstable.NewSession(ctx, connection, schema.InitializeResponse{}, schema.NewSessionRequest{Cwd: "/w", ConfigOptions: initial}); err == nil || !strings.Contains(err.Error(), "initial configOptions") {
		t.Fatalf("initial configOptions without the capability: %v", err)
	}
	requireNothingWritten(t, requests)

	if _, err := acpunstable.SessionOptions(ctx, connection, optionsInitialization(), schema.SessionOptionsRequest{Cwd: "/w", SessionID: &session}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.method != schema.SessionOptionsMethodName || string(request.params) != `{"cwd":"/w","sessionId":"s"}` {
		t.Fatalf("session/options frame = %s %s", request.method, request.params)
	}
	if _, err := acpunstable.NewSession(ctx, connection, optionsInitialization(), schema.NewSessionRequest{Cwd: "/w", ConfigOptions: initial}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; !strings.Contains(string(request.params), `"configOptions":[{"configId":"model","value":"fast"}]`) {
		t.Fatalf("session/new frame = %s", request.params)
	}
}

type optionsAgent struct{}

func (optionsAgent) SessionOptions(_ context.Context, request schema.SessionOptionsRequest) (schema.SessionOptionsResponse, error) {
	if request.SessionID != nil {
		return schema.SessionOptionsResponse{}, acp.SessionNotFoundError(stable.SessionId(*request.SessionID))
	}
	return schema.SessionOptionsResponse{ConfigOptions: []schema.SessionConfigOption{}}, nil
}

func TestHandleServesSessionOptions(t *testing.T) {
	ctx := context.Background()
	handler := acpunstable.Handle(nil, optionsAgent{})
	served, err := handler(ctx, schema.SessionOptionsMethodName, json.RawMessage(`{"cwd":"/w"}`))
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(served); string(encoded) != `{"configOptions":[]}` {
		t.Fatalf("session/options answered %s", encoded)
	}
	_, err = handler(ctx, schema.SessionOptionsMethodName, json.RawMessage(`{"cwd":"/w","sessionId":"gone"}`))
	if rpc := rpcError(t, err); rpc.Code != -32002 {
		t.Fatalf("unknown session = %+v", rpc)
	}
	_, err = handler(ctx, schema.SessionOptionsMethodName, json.RawMessage(`{}`))
	if rpc := rpcError(t, err); rpc.Code != -32602 || !strings.Contains(string(rpc.Data), "missing field `cwd`") {
		t.Fatalf("missing cwd = %+v (data %s)", rpc, rpc.Data)
	}
	// Without an OptionsHandler the method is not served.
	_, err = acpunstable.Handle(nil, struct{}{})(ctx, schema.SessionOptionsMethodName, json.RawMessage(`{"cwd":"/w"}`))
	if rpc := rpcError(t, err); rpc.Code != -32601 {
		t.Fatalf("without a handler = %+v", rpc)
	}
	// Advertise does not announce the capability, which also covers initial
	// configOptions on session setup requests.
	announced, err := acpunstable.Advertise(optionsAgent{})(stable.InitializeResponse{ProtocolVersion: 1, AgentCapabilities: &stable.AgentCapabilities{SessionCapabilities: &stable.SessionCapabilities{}}})
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(announced); strings.Contains(string(encoded), `"options"`) {
		t.Fatalf("Advertise announced session/options: %s", encoded)
	}
}
