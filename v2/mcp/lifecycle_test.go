package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// agentSide wraps a fake agent's handler so it answers the client's
// initialize, as a v2 client connection initializes before any other traffic.
func agentSide(handler acp.Handler) acp.Handler {
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if method == schema.InitializeMethodName {
			return schema.InitializeResponse{
				ProtocolVersion: acpv2.Version,
				Info:            schema.Implementation{Name: "agent", Version: "1"},
				Capabilities:    &schema.AgentCapabilities{Session: &schema.SessionCapabilities{}},
			}, nil
		}
		if handler == nil {
			return nil, &acp.RPCError{Code: -32601}
		}
		return handler(ctx, method, raw)
	}
}

// initializeClient completes a client connection's initialization against a
// peer served by agentSide.
func initializeClient(t *testing.T, client *acpv2.Connection) {
	t.Helper()
	if _, err := client.Initialize(context.Background(), acpv2.Capabilities{}); err != nil {
		t.Fatal(err)
	}
}
