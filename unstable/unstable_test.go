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
	schema "github.com/BrokkAi/acp-go/schema/unstable"
	acpunstable "github.com/BrokkAi/acp-go/unstable"
)

type recorded struct {
	method string
	params json.RawMessage
}

// fixture runs one in-memory ACP connection whose peer records each request and
// answers with the result respond returns for that method.
func fixture(t *testing.T, respond func(method string) any) (*acp.Connection, chan recorded) {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	connection := acp.Connect(local, local, nil, nil)
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

func providersInitialization() schema.InitializeResponse {
	return schema.InitializeResponse{AgentCapabilities: &schema.AgentCapabilities{
		Providers: &schema.ProvidersCapabilities{},
	}}
}

func forkInitialization() schema.InitializeResponse {
	return schema.InitializeResponse{AgentCapabilities: &schema.AgentCapabilities{
		SessionCapabilities: &schema.SessionCapabilities{Fork: &schema.SessionForkCapabilities{}},
	}}
}

func TestProviderFacadesGateAndEncode(t *testing.T) {
	ctx := context.Background()
	t.Run("capability gate", func(t *testing.T) {
		connection, requests := fixture(t, func(string) any { return map[string]any{} })
		if _, err := acpunstable.ListProviders(ctx, connection, schema.InitializeResponse{}); err == nil {
			t.Fatal("providers/list without the capability was allowed")
		}
		select {
		case request := <-requests:
			t.Fatalf("capability gate wrote %s to the wire", request.method)
		default:
		}
	})

	t.Run("list set disable", func(t *testing.T) {
		connection, requests := fixture(t, func(method string) any {
			if method == schema.ProvidersListMethodName {
				return schema.ListProvidersResponse{Providers: []schema.ProviderInfo{{ProviderID: "alpha"}}}
			}
			return map[string]any{}
		})
		initialization := providersInitialization()
		providers, err := acpunstable.ListProviders(ctx, connection, initialization)
		if err != nil {
			t.Fatal(err)
		}
		if len(providers.Providers) != 1 || providers.Providers[0].ProviderID != "alpha" {
			t.Fatalf("providers = %+v", providers.Providers)
		}
		if request := <-requests; request.method != schema.ProvidersListMethodName {
			t.Fatalf("method = %s", request.method)
		}
		if _, err := acpunstable.SetProvider(ctx, connection, initialization, schema.SetProviderRequest{}); err == nil || !strings.Contains(err.Error(), "provider ID is required") {
			t.Fatalf("empty provider ID error = %v", err)
		}
		if _, err := acpunstable.SetProvider(ctx, connection, initialization, schema.SetProviderRequest{ProviderID: "alpha", APIType: "openai", BaseURL: "https://example.test"}); err != nil {
			t.Fatal(err)
		}
		request := <-requests
		if request.method != schema.ProvidersSetMethodName || !strings.Contains(string(request.params), `"providerId":"alpha"`) {
			t.Fatalf("providers/set frame = %s %s", request.method, request.params)
		}
		if _, err := acpunstable.DisableProvider(ctx, connection, initialization, "alpha"); err != nil {
			t.Fatal(err)
		}
		if request := <-requests; request.method != schema.ProvidersDisableMethodName {
			t.Fatalf("method = %s", request.method)
		}
	})
}

func TestForkSessionGatesAndEncodes(t *testing.T) {
	ctx := context.Background()
	connection, requests := fixture(t, func(string) any {
		return schema.ForkSessionResponse{SessionID: "forked"}
	})
	if _, err := acpunstable.ForkSession(ctx, connection, schema.InitializeResponse{}, schema.ForkSessionRequest{Cwd: hostRoot + "/tmp", SessionID: "old"}); err == nil {
		t.Fatal("session/fork without the capability was allowed")
	}
	initialization := forkInitialization()
	for name, request := range map[string]schema.ForkSessionRequest{
		"missing session": {Cwd: hostRoot + "/tmp"},
		"relative cwd":    {Cwd: "relative", SessionID: "old"},
	} {
		if _, err := acpunstable.ForkSession(ctx, connection, initialization, request); err == nil {
			t.Fatalf("%s was allowed", name)
		}
	}
	select {
	case request := <-requests:
		t.Fatalf("validation wrote %s to the wire", request.method)
	default:
	}
	result, err := acpunstable.ForkSession(ctx, connection, initialization, schema.ForkSessionRequest{Cwd: hostRoot + "/tmp", SessionID: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "forked" {
		t.Fatalf("forked session = %q", result.SessionID)
	}
	if request := <-requests; request.method != schema.SessionForkMethodName {
		t.Fatalf("method = %s", request.method)
	}
	// Issue #47: a Windows client must be able to fork into a POSIX agent
	// working directory even though filepath.IsAbs rejects "/" there.
	if _, err := acpunstable.ForkSession(ctx, connection, initialization, schema.ForkSessionRequest{Cwd: "/", SessionID: "old"}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.method != schema.SessionForkMethodName {
		t.Fatalf("method = %s", request.method)
	}
	// Issue #47: the POSIX-to-Windows direction and mixed additional
	// directories must also pass through the unstable fork facade.
	if _, err := acpunstable.ForkSession(ctx, connection, initialization, schema.ForkSessionRequest{
		Cwd: `C:\agent\workspace`, SessionID: "old",
		AdditionalDirectories: []string{"/", `C:\agent\extra`},
	}); err != nil {
		t.Fatal(err)
	}
	if request := <-requests; request.method != schema.SessionForkMethodName {
		t.Fatalf("method = %s", request.method)
	}
}

type testHandler struct{}

func (testHandler) ListProviders(context.Context, schema.ListProvidersRequest) (schema.ListProvidersResponse, error) {
	return schema.ListProvidersResponse{Providers: []schema.ProviderInfo{{ProviderID: "dispatched"}}}, nil
}

func (testHandler) SetProvider(context.Context, schema.SetProviderRequest) (schema.SetProviderResponse, error) {
	return schema.SetProviderResponse{}, nil
}

func (testHandler) DisableProvider(context.Context, schema.DisableProviderRequest) (schema.DisableProviderResponse, error) {
	return schema.DisableProviderResponse{}, nil
}

func (testHandler) ForkSession(context.Context, schema.ForkSessionRequest) (schema.ForkSessionResponse, error) {
	return schema.ForkSessionResponse{SessionID: "dispatched"}, nil
}

func TestHandleDispatchesUnstableMethods(t *testing.T) {
	next := func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		return "next:" + method, nil
	}
	handler := acpunstable.Handle(next, testHandler{})
	ctx := context.Background()

	result, err := handler(ctx, schema.ProvidersListMethodName, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if providers, ok := result.(schema.ListProvidersResponse); !ok || len(providers.Providers) != 1 {
		t.Fatalf("providers/list dispatch = %#v", result)
	}
	result, err = handler(ctx, schema.SessionForkMethodName, json.RawMessage(`{"sessionId":"s","cwd":"`+hostRoot+`/tmp"}`))
	if err != nil {
		t.Fatal(err)
	}
	if fork, ok := result.(schema.ForkSessionResponse); !ok || fork.SessionID != "dispatched" {
		t.Fatalf("session/fork dispatch = %#v", result)
	}
	result, err = handler(ctx, "session/new", json.RawMessage(`{}`))
	if err != nil || result != "next:session/new" {
		t.Fatalf("fallthrough = %#v, %v", result, err)
	}

	empty := acpunstable.Handle(nil, struct{}{})
	if _, err := empty(ctx, schema.ProvidersListMethodName, json.RawMessage(`{}`)); err == nil {
		t.Fatal("missing provider handler answered providers/list")
	}
	var rpcError *acp.RPCError
	if _, err := empty(ctx, "session/new", json.RawMessage(`{}`)); !errors.As(err, &rpcError) || rpcError.Code != -32601 {
		t.Fatalf("unhandled method error = %v", err)
	}
	if _, err := acpunstable.Handle(nil, testHandler{})(ctx, schema.ProvidersSetMethodName, json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed params were accepted")
	}
}

func nesInitialization() schema.InitializeResponse {
	return schema.InitializeResponse{AgentCapabilities: &schema.AgentCapabilities{
		Nes: &schema.NesCapabilities{},
	}}
}

func TestNesFacadesGateAndEncode(t *testing.T) {
	ctx := context.Background()
	t.Run("capability gate", func(t *testing.T) {
		connection, requests := fixture(t, func(string) any { return map[string]any{} })
		if _, err := acpunstable.StartNes(ctx, connection, schema.InitializeResponse{}, schema.StartNesRequest{}); err == nil {
			t.Fatal("nes/start without the capability was allowed")
		}
		if err := acpunstable.AcceptNes(ctx, connection, schema.InitializeResponse{}, schema.AcceptNesNotification{SessionID: "s", ID: "i"}); err == nil {
			t.Fatal("nes/accept without the capability was allowed")
		}
		select {
		case request := <-requests:
			t.Fatalf("capability gate wrote %s to the wire", request.method)
		default:
		}
	})

	t.Run("requests and notifications", func(t *testing.T) {
		connection, requests := fixture(t, func(method string) any {
			if method == schema.NesStartMethodName {
				return schema.StartNesResponse{SessionID: "nes-1"}
			}
			return map[string]any{}
		})
		initialization := nesInitialization()
		started, err := acpunstable.StartNes(ctx, connection, initialization, schema.StartNesRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if started.SessionID != "nes-1" {
			t.Fatalf("NES session = %q", started.SessionID)
		}
		if request := <-requests; request.method != schema.NesStartMethodName {
			t.Fatalf("method = %s", request.method)
		}
		if _, err := acpunstable.SuggestNes(ctx, connection, initialization, schema.SuggestNesRequest{SessionID: "nes-1", URI: "file:///tmp/x"}); err != nil {
			t.Fatal(err)
		}
		if request := <-requests; request.method != schema.NesSuggestMethodName {
			t.Fatalf("method = %s", request.method)
		}
		if _, err := acpunstable.SuggestNes(ctx, connection, initialization, schema.SuggestNesRequest{SessionID: "nes-1", URI: "relative"}); err == nil {
			t.Fatal("relative document URI was allowed")
		}
		if err := acpunstable.AcceptNes(ctx, connection, initialization, schema.AcceptNesNotification{SessionID: "nes-1", ID: "sug"}); err != nil {
			t.Fatal(err)
		}
		if request := <-requests; request.method != schema.NesAcceptMethodName {
			t.Fatalf("method = %s", request.method)
		}
		if err := acpunstable.RejectNes(ctx, connection, initialization, schema.RejectNesNotification{SessionID: "nes-1", ID: "sug"}); err != nil {
			t.Fatal(err)
		}
		if request := <-requests; request.method != schema.NesRejectMethodName {
			t.Fatalf("method = %s", request.method)
		}
		if _, err := acpunstable.CloseNes(ctx, connection, initialization, schema.CloseNesRequest{SessionID: "nes-1"}); err != nil {
			t.Fatal(err)
		}
		if request := <-requests; request.method != schema.NesCloseMethodName {
			t.Fatalf("method = %s", request.method)
		}
	})
}

type nesTestHandler struct {
	accepted bool
}

func (h *nesTestHandler) StartNes(context.Context, schema.StartNesRequest) (schema.StartNesResponse, error) {
	return schema.StartNesResponse{SessionID: "dispatched"}, nil
}

func (h *nesTestHandler) SuggestNes(context.Context, schema.SuggestNesRequest) (schema.SuggestNesResponse, error) {
	return schema.SuggestNesResponse{}, nil
}

func (h *nesTestHandler) AcceptNes(context.Context, schema.AcceptNesNotification) error {
	h.accepted = true
	return nil
}

func (h *nesTestHandler) RejectNes(context.Context, schema.RejectNesNotification) error { return nil }

func (h *nesTestHandler) CloseNes(context.Context, schema.CloseNesRequest) (schema.CloseNesResponse, error) {
	return schema.CloseNesResponse{}, nil
}

func TestHandleDispatchesNes(t *testing.T) {
	ctx := context.Background()
	handler := &nesTestHandler{}
	dispatch := acpunstable.Handle(func(context.Context, string, json.RawMessage) (any, error) {
		return "next", nil
	}, handler)
	result, err := dispatch(ctx, schema.NesStartMethodName, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if started, ok := result.(schema.StartNesResponse); !ok || started.SessionID != "dispatched" {
		t.Fatalf("nes/start dispatch = %#v", result)
	}
	if result, err := dispatch(ctx, "session/new", json.RawMessage(`{}`)); err != nil || result != "next" {
		t.Fatalf("fallthrough = %#v, %v", result, err)
	}

	notifications := acpunstable.HandleNesNotifications(nil, handler)
	if err := notifications(schema.NesAcceptMethodName, json.RawMessage(`{"sessionId":"s","id":"i"}`)); err != nil {
		t.Fatal(err)
	}
	if !handler.accepted {
		t.Fatal("nes/accept was not dispatched")
	}
	if err := notifications(schema.NesRejectMethodName, json.RawMessage(`{`)); err == nil {
		t.Fatal("malformed nes/reject notification was accepted")
	}
	if err := notifications("session/update", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("unrelated notification = %v", err)
	}
	if _, err := acpunstable.Handle(nil, struct{}{})(ctx, schema.NesStartMethodName, json.RawMessage(`{}`)); err == nil {
		t.Fatal("missing NES handler answered nes/start")
	}
}
