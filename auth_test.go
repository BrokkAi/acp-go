package acp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/BrokkAi/acp-go/schema"
)

var errInvalidAuthWire = errors.New("invalid authenticate wire frame")

func TestAuthenticateUsesGeneratedAuthMethodUnion(t *testing.T) {
	c, peer := pipeClient(t, nil, nil)
	peerDone := make(chan error, 1)
	go func() {
		defer close(peerDone)
		var request packet
		if err := json.NewDecoder(peer).Decode(&request); err != nil {
			peerDone <- err
			return
		}
		if request.Method != schema.AuthenticateMethodName || string(request.Params) != `{"methodId":"agent"}` {
			peerDone <- errInvalidAuthWire
			return
		}
		peerDone <- json.NewEncoder(peer).Encode(packet{Version: "2.0", ID: request.ID, Result: json.RawMessage(`{}`)})
	}()
	init := Initialization{AuthMethods: []schema.AuthMethod{
		{Agent: &schema.AuthMethodAgent{ID: "agent", Name: "Agent"}},
		{Terminal: &schema.AuthMethodTerminal{ID: "terminal", Name: "Terminal"}},
	}}
	if err := c.Authenticate(context.Background(), init, "agent"); err != nil {
		t.Fatal(err)
	}
	if err := c.Authenticate(context.Background(), init, "terminal"); err == nil {
		t.Fatal("terminal authentication was sent over the unattended connection")
	}
	if err := c.Authenticate(context.Background(), init, "missing"); err == nil {
		t.Fatal("unadvertised authentication method was accepted")
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
}
