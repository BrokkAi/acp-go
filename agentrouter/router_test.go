package agentrouter

import (
	"bufio"
	"encoding/json"
	"testing"

	"github.com/BrokkAi/acp-go/acptest"
	schema1 "github.com/BrokkAi/acp-go/schema"
	schema2 "github.com/BrokkAi/acp-go/schema/v2"
)

// startRouter serves router over the shared in-memory pipe harness.
func startRouter(t *testing.T, router *Agent) *bufio.ReadWriter {
	t.Helper()
	return acptest.Serve(t, router.Serve)
}

func TestRouterRoutesFutureVersionToV2(t *testing.T) {
	requests := make(chan schema2.InitializeRequest, 1)
	rw := startRouter(t, New().
		WithV1(&acptest.V1Agent{Requests: nil}).
		WithV2(&acptest.V2Agent{Requests: requests}))
	acptest.WriteInitialize(t, rw, map[string]any{
		"protocolVersion":        3,
		"info":                   map[string]any{"name": "future-client", "version": "3.0"},
		"_futureInitializeField": map[string]any{"future": true},
	})
	var response struct {
		Result struct {
			ProtocolVersion uint16 `json:"protocolVersion"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rw).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil {
		t.Fatalf("router error: %s", response.Error.Message)
	}
	if response.Result.ProtocolVersion != 2 {
		t.Fatalf("selected protocol = %d", response.Result.ProtocolVersion)
	}
	select {
	case request := <-requests:
		if request.ProtocolVersion != 2 || request.Info.Name != "future-client" {
			t.Fatalf("canonical v2 request = %+v", request)
		}
	default:
		t.Fatal("v2 implementation was not called")
	}
}

func TestRouterCanonicalizesV2ToConfiguredV1(t *testing.T) {
	requests := make(chan schema1.InitializeRequest, 1)
	rw := startRouter(t, New().WithV1(&acptest.V1Agent{Requests: requests}))
	acptest.WriteInitialize(t, rw, map[string]any{
		"protocolVersion": 2,
		"capabilities": map[string]any{
			"auth":        map[string]any{"terminal": map[string]any{}},
			"elicitation": map[string]any{"form": map[string]any{}, "url": map[string]any{}},
		},
		"info": map[string]any{"name": "v2-client", "version": "2.0"},
	})
	var response struct {
		Result struct {
			ProtocolVersion uint16 `json:"protocolVersion"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rw).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil {
		t.Fatalf("router error: %s", response.Error.Message)
	}
	if response.Result.ProtocolVersion != 1 {
		t.Fatalf("selected protocol = %d", response.Result.ProtocolVersion)
	}
	select {
	case request := <-requests:
		if request.ProtocolVersion != 1 || request.ClientInfo == nil || request.ClientInfo.Name != "v2-client" {
			t.Fatalf("canonical v1 request = %+v", request)
		}
		caps := request.ClientCapabilities
		if caps == nil || caps.Auth == nil || caps.Auth.Terminal == nil || !*caps.Auth.Terminal ||
			caps.Elicitation == nil || caps.Session == nil || caps.Session.ConfigOptions == nil ||
			caps.Session.ConfigOptions.Boolean == nil {
			t.Fatalf("canonical v1 capabilities = %+v", caps)
		}
	default:
		t.Fatal("v1 implementation was not called")
	}
}

func TestRouterPreservesFramesBufferedAfterInitialize(t *testing.T) {
	requests := make(chan schema2.InitializeRequest, 1)
	rw := startRouter(t, New().WithV2(&acptest.V2Agent{Requests: requests}))
	initialize, err := json.Marshal(map[string]any{
		"protocolVersion": 2,
		"info":            map[string]any{"name": "client", "version": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": json.RawMessage(initialize)}
	second := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/list", "params": map[string]any{}}
	for _, frame := range []map[string]any{first, second} {
		encoded, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rw.Write(append(encoded, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for range 2 {
		var response struct {
			ID     int `json:"id"`
			Result any `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(rw).Decode(&response); err != nil {
			t.Fatal(err)
		}
		seen[response.ID] = true
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("response IDs = %+v", seen)
	}
}

func TestRouterRejectsMalformedProtocolVersion(t *testing.T) {
	rw := startRouter(t, New().WithV2(&acptest.V2Agent{Requests: make(chan schema2.InitializeRequest, 1)}))
	acptest.WriteInitialize(t, rw, map[string]any{
		"protocolVersion": 100000,
		"info":            map[string]any{"name": "client", "version": "1"},
	})
	var response struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rw).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != -32600 {
		t.Fatalf("response = %+v", response)
	}
}

func TestRouterValidatesInitializeBeforeBatchDispatch(t *testing.T) {
	requests := make(chan schema2.InitializeRequest, 1)
	rw := startRouter(t, New().WithV2(&acptest.V2Agent{Requests: requests}))
	batch, err := json.Marshal([]map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 2}},
		{"jsonrpc": "2.0", "id": 2, "method": "session/list", "params": map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Write(append(batch, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	var responses []struct {
		ID    int `json:"id"`
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rw).Decode(&responses); err != nil {
		t.Fatal(err)
	}
	if len(responses) != 2 || responses[0].ID != 1 || responses[1].ID != 2 ||
		responses[0].Error == nil || responses[1].Error == nil ||
		responses[0].Error.Code != -32602 || responses[1].Error.Code != -32602 {
		t.Fatalf("responses = %+v", responses)
	}
	select {
	case <-requests:
		t.Fatal("invalid initialize handler ran")
	default:
	}
}

func TestRouterAcceptsInitialBatchAndPreservesFutureNotification(t *testing.T) {
	requests := make(chan schema2.InitializeRequest, 1)
	rw := startRouter(t, New().WithV2(&acptest.V2Agent{Requests: requests}))
	batch, err := json.Marshal([]map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
			"protocolVersion": 2,
			"info":            map[string]any{"name": "batch-client", "version": "1"},
		}},
		{"jsonrpc": "2.0", "method": "_future/notification", "params": map[string]any{"preserved": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.Write(append(batch, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	var responses []struct {
		ID     int `json:"id"`
		Result *struct {
			ProtocolVersion uint16 `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.NewDecoder(rw).Decode(&responses); err != nil {
		t.Fatal(err)
	}
	if len(responses) != 1 || responses[0].ID != 1 || responses[0].Result == nil || responses[0].Result.ProtocolVersion != 2 {
		t.Fatalf("responses = %+v", responses)
	}
	select {
	case request := <-requests:
		if request.Info.Name != "batch-client" {
			t.Fatalf("initialize request = %+v", request)
		}
	default:
		t.Fatal("initialize handler did not run")
	}
}
