package unstable_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpunstable "github.com/BrokkAi/acp-go/v2/unstable"
)

func optionsInitialization() schema.InitializeResponse {
	return schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{Session: &schema.SessionCapabilities{
		Options: &schema.SessionOptionsCapabilities{},
	}}}
}

// TestSessionOptionsGatesAndSends is the draft-v2 counterpart of the v1
// session/options facade test: the capability, an absolute cwd, a non-blank
// session ID, and the capability for initial configOptions on session setup.
func TestSessionOptionsGatesAndSends(t *testing.T) {
	ctx := context.Background()
	connection, requests := rawPeer(t, func(method string) string {
		if method == schema.SessionNewMethodName {
			return `{"sessionId":"new"}`
		}
		return `{"configOptions":[],"availableCommands":[]}`
	})
	blank := schema.SessionId(" ")
	baseline := schema.InitializeResponse{Capabilities: &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}}}
	for name, tc := range map[string]struct {
		initialization schema.InitializeResponse
		request        schema.SessionOptionsRequest
		want           string
	}{
		"not advertised":  {baseline, schema.SessionOptionsRequest{Cwd: "/w"}, "did not advertise session/options"},
		"relative cwd":    {optionsInitialization(), schema.SessionOptionsRequest{Cwd: "w"}, "must be absolute"},
		"blank session":   {optionsInitialization(), schema.SessionOptionsRequest{Cwd: "/w", SessionID: &blank}, "session ID"},
		"additional dirs": {optionsInitialization(), schema.SessionOptionsRequest{Cwd: "/w", AdditionalDirectories: []schema.AbsolutePath{"/x"}}, "additionalDirectories"},
	} {
		if _, err := acpunstable.SessionOptions(ctx, connection, tc.initialization, tc.request); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	initial := []schema.InitialSessionConfigOption{{ConfigID: "model", ID: &schema.InitialSessionConfigOptionID{Value: "fast"}}}
	if _, err := acpunstable.NewSession(ctx, connection, baseline, schema.NewSessionRequest{Cwd: "/w", ConfigOptions: initial}); err == nil || !strings.Contains(err.Error(), "initial configOptions") {
		t.Fatalf("initial configOptions without the capability: %v", err)
	}
	select {
	case request := <-requests:
		t.Fatalf("a refused call wrote %s", request.method)
	default:
	}

	session := schema.SessionId("s")
	if _, err := acpunstable.SessionOptions(ctx, connection, optionsInitialization(), schema.SessionOptionsRequest{Cwd: "/w", SessionID: &session}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.method != schema.SessionOptionsMethodName || string(request.params) != `{"cwd":"/w","sessionId":"s"}` {
		t.Fatalf("session/options frame = %s %s", request.method, request.params)
	}
	if _, err := acpunstable.NewSession(ctx, connection, optionsInitialization(), schema.NewSessionRequest{Cwd: "/w", ConfigOptions: initial}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; !strings.Contains(string(request.params), `"configOptions":[{"configId":"model","type":"id","value":"fast"}]`) {
		t.Fatalf("session/new frame = %s", request.params)
	}
}

type optionsAgent struct{}

func (optionsAgent) SessionOptions(context.Context, schema.SessionOptionsRequest) (schema.SessionOptionsResponse, error) {
	return schema.SessionOptionsResponse{AvailableCommands: []schema.AvailableCommand{
		{Kind: schema.AvailableCommandKindSkill, Skill: &schema.Skill{Name: "review", Description: "Review changes"}},
	}}, nil
}

func TestHandleServesSessionOptions(t *testing.T) {
	ctx := context.Background()
	handler := acpunstable.Handle(nil, optionsAgent{})
	served, err := handler(ctx, schema.SessionOptionsMethodName, json.RawMessage(`{"cwd":"/w"}`))
	if err != nil {
		t.Fatal(err)
	}
	if encoded, _ := json.Marshal(served); string(encoded) != `{"availableCommands":[{"type":"skill","name":"review","description":"Review changes"}]}` {
		t.Fatalf("session/options answered %s", encoded)
	}
	// v2 rejects an initial value whose type is not a string, as the
	// reference SDK's hand-written decoder does.
	var decoded schema.NewSessionRequest
	if err := json.Unmarshal([]byte(`{"cwd":"/w","configOptions":[{"configId":"m","type":0,"value":"x"}]}`), &decoded); err == nil || err.Error() != "`type` must be a string" {
		t.Fatalf("numeric initial value type: %v", err)
	}
}
