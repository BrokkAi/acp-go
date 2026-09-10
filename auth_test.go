package acp

import (
	"context"
	"testing"

	"github.com/BrokkAi/acp-go/schema"
)

func TestAuthenticateSendsOnlyAdvertisedAgentMethod(t *testing.T) {
	c, requests := singleRequestFixture(t, `{}`)
	init := Initialization{AuthMethods: []schema.AuthMethod{
		{Agent: &schema.AuthMethodAgent{ID: "agent", Name: "Agent"}},
		{Terminal: &schema.AuthMethodTerminal{ID: "terminal", Name: "Terminal"}},
	}}
	if err := c.Authenticate(context.Background(), init, "agent"); err != nil {
		t.Fatal(err)
	}
	request := receiveRequest(t, requests)
	if request.Method != schema.AuthenticateMethodName || string(request.rawParams) != `{"methodId":"agent"}` {
		t.Fatalf("unexpected authenticate request: %s %s", request.Method, request.rawParams)
	}
}

func TestAuthenticateRejectsWithoutWritingToWire(t *testing.T) {
	agent := Initialization{AuthMethods: []schema.AuthMethod{
		{Agent: &schema.AuthMethodAgent{ID: "agent", Name: "Agent"}},
		{Terminal: &schema.AuthMethodTerminal{ID: "terminal", Name: "Terminal"}},
	}}
	invalidUnion := Initialization{AuthMethods: []schema.AuthMethod{{}}}
	tests := []struct {
		name   string
		init   Initialization
		method string
		want   string
	}{
		{"empty method", agent, "", "authentication method is required"},
		{"empty advertised list", Initialization{}, "agent", `agent did not advertise authentication method "agent"`},
		{"both-nil union", invalidUnion, "agent", `agent did not advertise authentication method "agent"`},
		{"terminal method", agent, "terminal", "authentication terminal is terminal-based and must be completed outside this connection"},
		{"missing method", agent, "missing", `agent did not advertise authentication method "missing"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, peer := pipeClient(t, nil, nil)
			wireRequests := unexpectedRequestRecorder(t, peer)
			err := c.Authenticate(context.Background(), test.init, test.method)
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			select {
			case request := <-wireRequests:
				t.Fatalf("wrote %s request to wire: %s", request.Method, request.Params)
			default:
			}
		})
	}
}
