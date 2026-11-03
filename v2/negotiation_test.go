package v2_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	"github.com/BrokkAi/acp-go/agent"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	agentv2 "github.com/BrokkAi/acp-go/v2/agent"
)

// serve runs an agent runtime on one end of a pipe and returns the other.
func serve(t *testing.T, run func(context.Context, net.Conn) error) net.Conn {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() { done <- run(context.Background(), peer) }()
	t.Cleanup(func() {
		_ = local.Close()
		_ = peer.Close()
		<-done
	})
	return local
}

func TestV2ClientReceivesV1SelectionAsLowerVersion(t *testing.T) {
	conn := serve(t, func(ctx context.Context, peer net.Conn) error {
		return agent.New(&acptest.V1Agent{}).Serve(ctx, peer, peer)
	})
	client := acpv2.Connect(conn, conn, nil, nil)
	defer client.Close()
	ctx := context.Background()
	info := acpv2.ClientInfo{Name: "v2-client", Version: "1"}

	outcome, err := client.SendInitialize(ctx, acpv2.Capabilities{}, info)
	if err != nil {
		t.Fatal(err)
	}
	mismatch := outcome.LowerVersion
	if outcome.Initialized != nil || mismatch == nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	if mismatch.Requested != 2 || mismatch.Selected() != 1 || mismatch.Response.V1 == nil ||
		mismatch.Response.V1.AgentInfo == nil || mismatch.Response.V1.AgentInfo.Name != "v1-router-agent" {
		t.Fatalf("mismatch = %+v", mismatch)
	}

	// The connection stays uninitialized and open: InitializeWithInfo can
	// try again, and reports the same selection as a typed error.
	_, err = client.InitializeWithInfo(ctx, acpv2.Capabilities{}, info)
	var typed *acp.InitializationVersionMismatch
	if !errors.As(err, &typed) || typed.Selected() != 1 {
		t.Fatalf("InitializeWithInfo error = %v", err)
	}
	if err.Error() != "requested ACP protocol version 2 but peer selected 1" {
		t.Fatalf("error text = %q", err)
	}
	if client.Err() != nil {
		t.Fatalf("connection closed: %v", client.Err())
	}
}

func TestV1ClientGetsUnsupportedProtocolVersionFromV2Agent(t *testing.T) {
	conn := serve(t, func(ctx context.Context, peer net.Conn) error {
		return agentv2.New(&acptest.V2Agent{}).Serve(ctx, peer, peer)
	})
	client := acp.Connect(conn, conn, nil, nil)
	defer client.Close()
	_, err := client.Initialize(context.Background(), acp.Capabilities{})
	if !acp.IsUnsupportedProtocolVersion(err) {
		t.Fatalf("error = %v, want -32022", err)
	}
	data, ok := acpv2.UnsupportedProtocolVersionData(err)
	if !ok || data.Requested != 1 || len(data.Supported) != 1 || data.Supported[0] != 2 {
		t.Fatalf("data = %+v, ok %t", data, ok)
	}
}

func TestV2ClientCanRetryAfterInitializeError(t *testing.T) {
	calls := 0
	local, peer := net.Pipe()
	server := acp.Connect(peer, peer, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		calls++
		switch calls {
		case 1:
			return nil, acpv2.UnsupportedProtocolVersionError(2, []schema.ProtocolVersion{3})
		case 2:
			return map[string]any{"protocolVersion": 3}, nil
		default:
			return schema.InitializeResponse{
				ProtocolVersion: acpv2.Version,
				Info:            schema.Implementation{Name: "agent", Version: "1"},
			}, nil
		}
	}, nil)
	defer server.Close()
	client := acpv2.Connect(local, local, nil, nil)
	defer client.Close()
	ctx := context.Background()
	info := acpv2.ClientInfo{Name: "client", Version: "1"}

	_, err := client.SendInitialize(ctx, acpv2.Capabilities{}, info)
	if data, ok := acpv2.UnsupportedProtocolVersionData(err); !ok || data.Supported[0] != 3 {
		t.Fatalf("first initialize error = %v", err)
	}
	_, err = client.SendInitialize(ctx, acpv2.Capabilities{}, info)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32600 || rpcErr.Message != "Invalid request" ||
		string(rpcErr.Data) != `"unsupported ACP protocol version 3; this endpoint only supports ACP protocol version 2"` {
		t.Fatalf("second initialize error = %v", err)
	}
	outcome, err := client.SendInitialize(ctx, acpv2.Capabilities{}, info)
	if err != nil || outcome.Initialized == nil || outcome.Initialized.Info.Name != "agent" {
		t.Fatalf("third initialize = %+v, %v", outcome, err)
	}
}

// TestV2ClientRejectsAnInvalidSelectedVersionLikeTheReferenceSDK: a
// response without a valid protocolVersion is the reference initiator's
// -32602.
func TestV2ClientRejectsAnInvalidSelectedVersionLikeTheReferenceSDK(t *testing.T) {
	for _, result := range []string{`{}`, `{"protocolVersion":null}`, `{"protocolVersion":"2"}`, `{"protocolVersion":-1}`} {
		local, peer := net.Pipe()
		server := acp.Connect(peer, peer, func(context.Context, string, json.RawMessage) (any, error) {
			return json.RawMessage(result), nil
		}, nil)
		client := acpv2.Connect(local, local, nil, nil)
		_, err := client.SendInitialize(context.Background(), acpv2.Capabilities{}, acpv2.ClientInfo{Name: "client", Version: "1"})
		var rpcErr *acp.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 || rpcErr.Message != "Invalid params" ||
			string(rpcErr.Data) != `"initialize.protocolVersion must be a valid ACP protocol version"` {
			t.Fatalf("%s: error = %v", result, err)
		}
		_ = client.Close()
		_ = server.Close()
	}
}

func TestErrorDataHelpers(t *testing.T) {
	unsupported := acpv2.UnsupportedProtocolVersionError(1, nil)
	encoded, err := json.Marshal(unsupported)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"code":-32022,"message":"Unsupported protocol version","data":{"requested":1,"supported":[]}}` {
		t.Fatalf("encoded = %s", encoded)
	}

	notFound := acpv2.SessionNotFoundError("sess_1")
	encoded, err = json.Marshal(notFound)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"code":-32002,"message":"Resource not found","data":{"sessionId":"sess_1"}}` {
		t.Fatalf("encoded = %s", encoded)
	}
	wrapped := fmt.Errorf("load: %w", notFound)
	if data, ok := acpv2.SessionNotFoundData(wrapped); !ok || data.SessionID != "sess_1" {
		t.Fatalf("session data = %+v, ok %t", data, ok)
	}
	if !acp.IsResourceNotFound(wrapped) {
		t.Fatal("IsResourceNotFound lost the wrapped error")
	}

	for _, test := range []struct {
		name string
		err  error
	}{
		{"other code", &acp.RPCError{Code: -32002, Message: "m", Data: json.RawMessage(`{"requested":1,"supported":[2]}`)}},
		{"no data", &acp.RPCError{Code: -32022, Message: "m"}},
		{"null data", &acp.RPCError{Code: -32022, Message: "m", Data: json.RawMessage(`null`)}},
		{"other shape", &acp.RPCError{Code: -32022, Message: "m", Data: json.RawMessage(`"text"`)}},
		{"not an RPC error", errors.New("plain")},
	} {
		if _, ok := acpv2.UnsupportedProtocolVersionData(test.err); ok {
			t.Fatalf("%s: decoded as unsupported protocol version data", test.name)
		}
	}
	if _, ok := acpv2.SessionNotFoundData(&acp.RPCError{Code: -32002, Message: "m", Data: json.RawMessage(`{"uri":"file:///x"}`)}); ok {
		t.Fatal("resource data decoded as session data")
	}

	// SchemaError is the explicit typed boundary: extra members are dropped,
	// null and omitted data are both nil, and unknown shapes stay raw.
	for _, test := range []struct {
		name  string
		input *acp.RPCError
		check func(*testing.T, schema.Error)
	}{
		{"omitted", &acp.RPCError{Code: -32603, Message: "m", Extra: map[string]json.RawMessage{"x": json.RawMessage(`1`)}}, func(t *testing.T, e schema.Error) {
			if e.Data != nil || e.Code != schema.ErrorCodeInternalError {
				t.Fatalf("error = %+v", e)
			}
		}},
		{"null", &acp.RPCError{Code: -32603, Message: "m", Data: json.RawMessage(`null`)}, func(t *testing.T, e schema.Error) {
			if e.Data != nil {
				t.Fatalf("data = %+v", e.Data)
			}
		}},
		{"unsupported", unsupported, func(t *testing.T, e schema.Error) {
			if e.Data == nil || e.Data.UnsupportedProtocolVersion == nil || e.Data.UnsupportedProtocolVersion.Requested != 1 {
				t.Fatalf("data = %+v", e.Data)
			}
		}},
		{"other", &acp.RPCError{Code: 1, Message: "m", Data: json.RawMessage(`{"detail":[1,2]}`)}, func(t *testing.T, e schema.Error) {
			if e.Data == nil || e.Data.Other == nil || string(e.Data.Other.Raw) != `{"detail":[1,2]}` {
				t.Fatalf("data = %+v", e.Data)
			}
		}},
	} {
		converted, err := acpv2.SchemaError(test.input)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		test.check(t, converted)
	}
	if _, err := acpv2.SchemaError(&acp.RPCError{Code: 1 << 40}); err == nil {
		t.Fatal("SchemaError accepted a code outside int32")
	}
}
