package unstable_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	v2schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	acpunstable "github.com/BrokkAi/acp-go/v2/unstable"
)

type recorded struct {
	method string
	params json.RawMessage
}

func fixture(t *testing.T, respond func(method string) any) (*acpv2.Connection, chan recorded) {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	connection := acpv2.Connect(local, local, nil, nil)
	requests := make(chan recorded, 8)
	go func() {
		decoder := json.NewDecoder(peer)
		encoder := json.NewEncoder(peer)
		for {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := decoder.Decode(&request); err != nil {
				return
			}
			requests <- recorded{method: request.Method, params: request.Params}
			result, _ := json.Marshal(respond(request.Method))
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": json.RawMessage(result)})
		}
	}()
	t.Cleanup(func() {
		_ = peer.Close()
		_ = connection.Close()
	})
	return connection, requests
}

func nesInitialization() v2schema.InitializeResponse {
	return v2schema.InitializeResponse{Capabilities: &v2schema.AgentCapabilities{
		Nes: &v2schema.NesCapabilities{},
	}}
}

func TestNesFacadesGateAndEncode(t *testing.T) {
	ctx := context.Background()
	connection, requests := fixture(t, func(method string) any {
		if method == v2schema.NesStartMethodName {
			return v2schema.StartNesResponse{SessionID: "nes-1"}
		}
		return map[string]any{}
	})
	if _, err := acpunstable.StartNes(ctx, connection, v2schema.InitializeResponse{}, v2schema.StartNesRequest{}); err == nil {
		t.Fatal("nes/start without the capability was allowed")
	}
	select {
	case request := <-requests:
		t.Fatalf("capability gate wrote %s to the wire", request.method)
	default:
	}

	initialization := nesInitialization()
	started, err := acpunstable.StartNes(ctx, connection, initialization, v2schema.StartNesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if started.SessionID != "nes-1" {
		t.Fatalf("NES session = %q", started.SessionID)
	}
	if request := <-requests; request.method != v2schema.NesStartMethodName {
		t.Fatalf("method = %s", request.method)
	}
	if _, err := acpunstable.SuggestNes(ctx, connection, initialization, v2schema.SuggestNesRequest{SessionID: "nes-1", URI: "relative"}); err == nil {
		t.Fatal("relative document URI was allowed")
	}
	if err := acpunstable.AcceptNes(ctx, connection, initialization, v2schema.AcceptNesNotification{SessionID: "nes-1", SuggestionID: "sug"}); err != nil {
		t.Fatal(err)
	}
	request := <-requests
	if request.method != v2schema.NesAcceptMethodName || !strings.Contains(string(request.params), `"suggestionId":"sug"`) {
		t.Fatalf("nes/accept frame = %s %s", request.method, request.params)
	}
	if _, err := acpunstable.CloseNes(ctx, connection, initialization, v2schema.CloseNesRequest{SessionID: "nes-1"}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.method != v2schema.NesCloseMethodName {
		t.Fatalf("method = %s", request.method)
	}
}

type nesTestHandler struct {
	accepted bool
}

func (h *nesTestHandler) StartNes(context.Context, v2schema.StartNesRequest) (v2schema.StartNesResponse, error) {
	return v2schema.StartNesResponse{SessionID: "dispatched"}, nil
}

func (h *nesTestHandler) SuggestNes(context.Context, v2schema.SuggestNesRequest) (v2schema.SuggestNesResponse, error) {
	return v2schema.SuggestNesResponse{}, nil
}

func (h *nesTestHandler) AcceptNes(context.Context, v2schema.AcceptNesNotification) error {
	h.accepted = true
	return nil
}

func (h *nesTestHandler) RejectNes(context.Context, v2schema.RejectNesNotification) error {
	return nil
}

func (h *nesTestHandler) CloseNes(context.Context, v2schema.CloseNesRequest) (v2schema.CloseNesResponse, error) {
	return v2schema.CloseNesResponse{}, nil
}

func TestHandleDispatchesNes(t *testing.T) {
	ctx := context.Background()
	handler := &nesTestHandler{}
	dispatch := acpunstable.Handle(func(context.Context, string, json.RawMessage) (any, error) {
		return "next", nil
	}, handler)
	result, err := dispatch(ctx, v2schema.NesStartMethodName, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if started, ok := result.(v2schema.StartNesResponse); !ok || started.SessionID != "dispatched" {
		t.Fatalf("nes/start dispatch = %#v", result)
	}
	if _, err := acpunstable.Handle(nil, struct{}{})(ctx, v2schema.NesStartMethodName, json.RawMessage(`{}`)); err == nil {
		t.Fatal("missing NES handler answered nes/start")
	}
	notifications := acpunstable.HandleNesNotifications(nil, handler)
	if err := notifications(v2schema.NesAcceptMethodName, json.RawMessage(`{"sessionId":"s","suggestionId":"i"}`)); err != nil {
		t.Fatal(err)
	}
	if !handler.accepted {
		t.Fatal("nes/accept was not dispatched")
	}
	var rpcError *acp.RPCError
	if _, err := acpunstable.Handle(nil, handler)(ctx, "session/new", json.RawMessage(`{}`)); !errors.As(err, &rpcError) {
		t.Fatalf("unhandled method error = %v", err)
	}
}
