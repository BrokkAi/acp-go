package unstable_test

import (
	"context"
	"encoding/json"
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
		SessionCapabilities: &schema.SessionCapabilities{
			Fork:                  &schema.SessionForkCapabilities{},
			AdditionalDirectories: &schema.SessionAdditionalDirectoriesCapabilities{},
		},
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
		"missing session":               {Cwd: hostRoot + "/tmp"},
		"relative cwd":                  {Cwd: "relative", SessionID: "old"},
		"relative additional directory": {Cwd: "/", SessionID: "old", AdditionalDirectories: []string{"extra"}},
	} {
		if _, err := acpunstable.ForkSession(ctx, connection, initialization, request); err == nil {
			t.Fatalf("%s was allowed", name)
		}
	}
	// The protocol requires sessionCapabilities.additionalDirectories before
	// a client sends additional directories.
	withoutAdditional := forkInitialization()
	withoutAdditional.AgentCapabilities.SessionCapabilities.AdditionalDirectories = nil
	if _, err := acpunstable.ForkSession(ctx, connection, withoutAdditional, schema.ForkSessionRequest{
		Cwd: "/", SessionID: "old", AdditionalDirectories: []string{"/extra"},
	}); err == nil || !strings.Contains(err.Error(), "additionalDirectories") {
		t.Fatalf("additional directories without the capability error = %v", err)
	}
	if _, err := acpunstable.ForkSession(ctx, connection, withoutAdditional, schema.ForkSessionRequest{Cwd: "/", SessionID: "old"}); err != nil {
		t.Fatalf("fork without additional directories needs no capability: %v", err)
	}
	if request := <-requests; request.method != schema.SessionForkMethodName {
		t.Fatalf("method = %s", request.method)
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
