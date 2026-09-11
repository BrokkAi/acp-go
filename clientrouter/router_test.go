package clientrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	schema1 "github.com/BrokkAi/acp-go/schema"
	schema2 "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

type testV1Client struct {
	session string
}

func (c testV1Client) Serve(ctx context.Context, connection *acp.Connection) error {
	capabilities := acp.Capabilities{}
	capabilities.Session = acp.ConfigOptionsClientCapabilities(true)
	initialization, err := connection.InitializeWithInfo(ctx, capabilities, acp.ClientInfo{
		Name: "connector-client", Version: "1.0",
	})
	if err != nil {
		return err
	}
	if initialization.ProtocolVersion != acp.Version {
		return fmt.Errorf("v1 client negotiated %d", initialization.ProtocolVersion)
	}
	_, err = connection.NewSession(ctx, "/repo")
	return err
}

func (testV1Client) RequestHandler() acp.Handler            { return nil }
func (testV1Client) NotificationHandler() acp.Notifications { return nil }

type testV2Client struct {
	session string
	future  bool
}

func (c testV2Client) Serve(ctx context.Context, connection *acpv2.Connection) error {
	if c.future {
		return connection.Call(ctx, schema2.InitializeMethodName, map[string]any{
			"protocolVersion": acpv2.Version,
			"info": map[string]any{
				"name":    "connector-client",
				"version": "1.0",
				"future":  true,
			},
		}, nil)
	}
	initialization, err := connection.InitializeWithInfo(ctx, acpv2.Capabilities{}, acpv2.ClientInfo{
		Name: "connector-client", Version: "1.0",
	})
	if err != nil {
		return err
	}
	if initialization.ProtocolVersion != acpv2.Version {
		return fmt.Errorf("v2 client negotiated %d", initialization.ProtocolVersion)
	}
	_, err = connection.NewSessionWithOptions(ctx, initialization, "/repo", acpv2.NewSessionOptions{})
	return err
}

func (testV2Client) RequestHandler() acp.Handler            { return nil }
func (testV2Client) NotificationHandler() acp.Notifications { return nil }

func agentFactory(protocol uint16, opens *atomic.Int32) AgentConnection {
	return func() (io.ReadWriteCloser, error) {
		local, peer := net.Pipe()
		opens.Add(1)
		deadline := time.Now().Add(5 * time.Second)
		_ = local.SetDeadline(deadline)
		_ = peer.SetDeadline(deadline)
		handler := func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
			switch method {
			case schema2.InitializeMethodName:
				var params struct{ ProtocolVersion uint16 }
				if err := json.Unmarshal(raw, &params); err != nil {
					return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
				}
				if params.ProtocolVersion != protocol && !(protocol == 1 && params.ProtocolVersion >= 2) {
					return nil, &acp.RPCError{Code: -32600, Message: fmt.Sprintf("unexpected protocol %d", params.ProtocolVersion)}
				}
				if protocol == 1 {
					return schema1.InitializeResponse{
						ProtocolVersion: acp.Version,
						AgentInfo:       &schema1.Implementation{Name: "fixture-agent", Version: "1"},
					}, nil
				}
				return schema2.InitializeResponse{
					ProtocolVersion: acpv2.Version,
					Info:            schema2.Implementation{Name: "fixture-agent", Version: "1"},
					Capabilities:    &schema2.AgentCapabilities{Session: &schema2.SessionCapabilities{}},
				}, nil
			case schema1.SessionNewMethodName:
				if protocol == 1 {
					return schema1.NewSessionResponse{SessionID: "v1-session"}, nil
				}
				return schema2.NewSessionResponse{SessionID: "v2-session"}, nil
			default:
				return nil, &acp.RPCError{Code: -32601}
			}
		}
		_ = acp.Connect(local, local, handler, nil)
		return peer, nil
	}
}

func TestClientRouterRoutesToV2(t *testing.T) {
	var opens atomic.Int32
	err := New().
		WithV1(func() V1Client { t.Fatal("v1 client must not start"); return nil }).
		WithV2(func() V2Client { return testV2Client{} }).
		Connect(context.Background(), agentFactory(2, &opens))
	if err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 1 {
		t.Fatalf("agent connections = %d", opens.Load())
	}
}

func TestClientRouterRoutesDirectlyToV1(t *testing.T) {
	var opens atomic.Int32
	err := New().
		WithV1(func() V1Client { return testV1Client{} }).
		Connect(context.Background(), agentFactory(1, &opens))
	if err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 1 {
		t.Fatalf("agent connections = %d", opens.Load())
	}
}

func TestClientRouterReusesMatchingV1FallbackConnection(t *testing.T) {
	var opens atomic.Int32
	err := New().
		WithV1(func() V1Client { return testV1Client{} }).
		WithV2(func() V2Client { return testV2Client{} }).
		Connect(context.Background(), agentFactory(1, &opens))
	if err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 1 {
		t.Fatalf("matching fallback reconnected: agent connections = %d", opens.Load())
	}
}

func TestClientRouterReconnectsWhenV2HasFutureFields(t *testing.T) {
	var opens atomic.Int32
	err := New().
		WithV1(func() V1Client { return testV1Client{} }).
		WithV2(func() V2Client { return testV2Client{future: true} }).
		Connect(context.Background(), agentFactory(1, &opens))
	if err != nil {
		t.Fatal(err)
	}
	if opens.Load() != 2 {
		t.Fatalf("non-matching fallback did not reconnect: agent connections = %d", opens.Load())
	}
}

func TestClientRouterDoesNotRetryV2Rejection(t *testing.T) {
	var opens atomic.Int32
	rejectingAgent := func() (io.ReadWriteCloser, error) {
		local, peer := net.Pipe()
		opens.Add(1)
		go func() {
			defer peer.Close()
			decoder := json.NewDecoder(local)
			var request struct {
				ID json.RawMessage `json:"id"`
			}
			if err := decoder.Decode(&request); err != nil {
				return
			}
			_ = json.NewEncoder(local).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"error": map[string]any{
					"code": -32000, "message": "auth_required",
				},
			})
		}()
		return peer, nil
	}
	err := New().
		WithV1(func() V1Client { t.Fatal("v1 must not retry after rejection"); return nil }).
		WithV2(func() V2Client { return testV2Client{} }).
		Connect(context.Background(), rejectingAgent)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32000 {
		t.Fatalf("error = %v", err)
	}
	if opens.Load() != 1 {
		t.Fatalf("rejected v2 initialize retried: connections = %d", opens.Load())
	}
}
