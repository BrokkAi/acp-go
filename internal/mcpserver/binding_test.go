package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/rpcctl"
)

// testCodec carries the binding over plain structs with the wire shape both
// ACP versions share.
var testCodec = Codec{
	Method: "mcp/message",
	Decode: func(raw json.RawMessage) (Envelope, error) {
		var request struct {
			ServerID  string          `json:"serverId"`
			RequestID string          `json:"requestId"`
			Method    string          `json:"method"`
			Params    json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return Envelope{}, err
		}
		if string(request.Params) == "null" {
			request.Params = nil
		}
		return Envelope{ServerID: request.ServerID, RequestID: request.RequestID, Method: request.Method, Params: request.Params}, nil
	},
	Response: func(outcome Outcome) (any, error) {
		if outcome.Error != nil {
			return map[string]any{"error": outcome.Error}, nil
		}
		return map[string]any{"result": outcome.Result}, nil
	},
	Notification: func(serverID, requestID, method string, params json.RawMessage) (any, error) {
		return map[string]any{"serverId": serverID, "requestId": requestID, "method": method, "params": params}, nil
	},
}

func envelope(serverID, requestID, method string, params any) json.RawMessage {
	encoded, _ := json.Marshal(map[string]any{"serverId": serverID, "requestId": requestID, "method": method, "params": params})
	return encoded
}

func nativeParams() map[string]any {
	return map[string]any{"_meta": map[string]any{MetaProtocolVersion: ProtocolVersion, MetaClientCapabilities: map[string]any{}}}
}

func wantRPCError(t *testing.T, err error, want string) {
	t.Helper()
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want %s", err, want)
	}
	encoded, encodeErr := json.Marshal(rpcErr)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if string(encoded) != want {
		t.Fatalf("error = %s\nwant    %s", encoded, want)
	}
}

func TestBindingOuterErrorsMatchTheReference(t *testing.T) {
	binding := NewBinding(nil, testCodec)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	registration, err := binding.Register("tools", ServiceFunc(func(ctx context.Context, request Request, _ *RequestContext) (Outcome, error) {
		if request.Method == "raw/canceled" {
			// context.Canceled without cancellation is a backend failure.
			return Outcome{}, context.Canceled
		}
		started <- struct{}{}
		<-release
		return ResultOutcome(true)
	}), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, err = binding.Handle(ctx, json.RawMessage(`{"serverId":7}`))
	// An envelope that does not decode answers json_cast_params' -32602.
	wantRPCError(t, err, `{"code":-32602,"message":"Invalid params","data":{"error":"json: cannot unmarshal number into Go struct field .serverId of type string","json":{"serverId":7},"phase":"deserialization"}}`)
	_, err = binding.Handle(ctx, envelope("nobody", "a", "tools/list", nil))
	wantRPCError(t, err, `{"code":-33001,"message":"MCP server registration unavailable"}`)
	_, err = binding.Handle(ctx, envelope("tools", "c", "raw/canceled", nil))
	wantRPCError(t, err, `{"code":-33002,"message":"MCP backend failure","data":"context canceled"}`)

	done := make(chan error, 1)
	go func() {
		_, err := binding.Handle(ctx, envelope("tools", "busy", "tools/call", nil))
		done <- err
	}()
	<-started
	_, err = binding.Handle(ctx, envelope("tools", "busy", "tools/call", nil))
	wantRPCError(t, err, `{"code":-32602,"message":"Invalid params","data":"duplicate active MCP requestId"}`)
	registration.Close()
	close(release)
	wantRPCError(t, <-done, `{"code":-33001,"message":"MCP provider removed"}`)
	// A released ID may be registered again; the reference keeps no record.
	if _, err := binding.Register("tools", registration.service, false); err != nil {
		t.Fatal(err)
	}
}

func TestBindingValidatesNativeMetadata(t *testing.T) {
	binding := NewBinding(nil, testCodec)
	registration, err := binding.RegisterNew(NewToolServer("tools", "", NewToolRegistry()).Service())
	if err != nil {
		t.Fatal(err)
	}
	inner := func(params any) string {
		t.Helper()
		response, err := binding.Handle(context.Background(), envelope(registration.ServerID(), "r", "tools/list", params))
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(response)
		return string(encoded)
	}
	for params, want := range map[string]string{
		`{}`: `{"error":{"code":-32602,"message":"Invalid params","data":"inner params._meta must be an object"}}`,
		`{"_meta":{"io.modelcontextprotocol/protocolVersion":null,"io.modelcontextprotocol/clientCapabilities":{}}}`:         `{"error":{"code":-32602,"message":"Invalid params","data":"inner params._meta requires io.modelcontextprotocol/protocolVersion"}}`,
		`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/clientCapabilities":{}}}`: `{"error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":"2025-11-25","supported":["2026-07-28"]}}}`,
		`{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`:                                                 `{"error":{"code":-32602,"message":"Invalid params","data":"inner params._meta requires io.modelcontextprotocol/clientCapabilities object"}}`,
	} {
		if got := inner(json.RawMessage(params)); got != want {
			t.Errorf("%s\n got %s\nwant %s", params, got, want)
		}
	}
	if _, err := projectDiscovery(Outcome{Result: json.RawMessage(`{"supportedVersions":["2026-07-28",null]}`)}); err == nil {
		t.Fatal("a non-string supported version was accepted")
	}
}

func TestBindingDoesNotStartCancelledWork(t *testing.T) {
	binding := NewBinding(nil, testCodec)
	executed := false
	if _, err := binding.Register("tools", ServiceFunc(func(context.Context, Request, *RequestContext) (Outcome, error) {
		executed = true
		return ResultOutcome(true)
	}), false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := binding.Handle(ctx, envelope("tools", "late", "tools/call", nil))
	wantRPCError(t, err, `{"code":-32800,"message":"Request cancelled"}`)
	if executed {
		t.Fatal("work cancelled before execution was started")
	}
}

func TestBindingSendsNoResponseAfterInputEOF(t *testing.T) {
	local, peer := net.Pipe()
	connection := acp.Connect(local, local, nil, nil)
	defer connection.Close()
	binding := NewBinding(connection, testCodec)
	if _, err := binding.Register("tools", ServiceFunc(func(context.Context, Request, *RequestContext) (Outcome, error) {
		t.Error("work after EOF was started")
		return ResultOutcome(true)
	}), false); err != nil {
		t.Fatal(err)
	}
	_ = peer.Close()
	<-connection.InputClosed()
	if _, err := binding.Handle(context.Background(), envelope("tools", "eof", "tools/call", nil)); !errors.Is(err, rpcctl.ErrNoResponse) {
		t.Fatalf("error = %v, want no response", err)
	}
}

func TestRegistrationCloseIsNotBlockedByAStalledTransport(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	// The peer never reads, so the writer stalls and the write queue fills.
	connection := acp.Connect(local, local, nil, nil)
	defer connection.Close()
	binding := NewBinding(connection, testCodec)
	notifying := make(chan struct{})
	finished := make(chan error, 1)
	registration, err := binding.Register("tools", ServiceFunc(func(ctx context.Context, _ Request, rc *RequestContext) (Outcome, error) {
		close(notifying)
		for {
			if err := rc.Notify(context.Background(), "notifications/progress", map[string]any{"n": 1}); err != nil {
				finished <- err
				return Outcome{}, ctx.Err()
			}
		}
	}), false)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = binding.Handle(context.Background(), envelope("tools", "flood", "tools/call", nil)) }()
	<-notifying
	time.Sleep(50 * time.Millisecond) // let the queue fill and block
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- binding.UnregisterAndWait(ctx, registration.ServerID()) }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("UnregisterAndWait = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("removal blocked on the stalled transport")
	}
	if err := <-finished; !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("notify error = %v", err)
	}
	// Cancelling queued notifications must not tear the connection down.
	select {
	case <-connection.Done():
		t.Fatalf("connection stopped: %v", connection.Err())
	case <-time.After(400 * time.Millisecond):
	}
}
