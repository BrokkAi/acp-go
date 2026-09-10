package agentrouter

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/agent"
	schema1 "github.com/BrokkAi/acp-go/schema"
	schema2 "github.com/BrokkAi/acp-go/schema/v2"
	agent2 "github.com/BrokkAi/acp-go/v2/agent"
)

type v1TestAgent struct {
	requests chan schema1.InitializeRequest
}

func (a *v1TestAgent) Initialize(_ context.Context, _ agent.Client, request schema1.InitializeRequest) (schema1.InitializeResponse, error) {
	a.requests <- request
	return schema1.InitializeResponse{
		ProtocolVersion: 1,
		AgentInfo:       &schema1.Implementation{Name: "v1-router-agent", Version: "test"},
	}, nil
}

func (a *v1TestAgent) NewSession(context.Context, agent.Client, schema1.NewSessionRequest) (schema1.NewSessionResponse, error) {
	return schema1.NewSessionResponse{SessionID: "v1"}, nil
}

func (a *v1TestAgent) Prompt(context.Context, agent.Client, schema1.PromptRequest, agent.SessionUpdater) (schema1.PromptResponse, error) {
	return schema1.PromptResponse{StopReason: schema1.StopReasonEndTurn}, nil
}

type v2TestAgent struct {
	requests chan schema2.InitializeRequest
}

func (a *v2TestAgent) Initialize(_ context.Context, _ agent2.Client, request schema2.InitializeRequest) (schema2.InitializeResponse, error) {
	a.requests <- request
	return schema2.InitializeResponse{
		ProtocolVersion: 2,
		Info:            schema2.Implementation{Name: "v2-router-agent", Version: "test"},
		Capabilities:    &schema2.AgentCapabilities{Session: &schema2.SessionCapabilities{}},
	}, nil
}

func (a *v2TestAgent) NewSession(context.Context, agent2.Client, schema2.NewSessionRequest) (schema2.NewSessionResponse, error) {
	return schema2.NewSessionResponse{SessionID: "v2"}, nil
}

func (a *v2TestAgent) Prompt(context.Context, agent2.Client, schema2.PromptRequest, agent2.SessionUpdater) (schema2.PromptResponse, error) {
	return schema2.PromptResponse{}, nil
}

func (a *v2TestAgent) ListSessions(context.Context, agent2.Client, schema2.ListSessionsRequest) (schema2.ListSessionsResponse, error) {
	return schema2.ListSessionsResponse{}, nil
}

func (a *v2TestAgent) ResumeSession(context.Context, agent2.Client, schema2.ResumeSessionRequest) (schema2.ResumeSessionResponse, error) {
	return schema2.ResumeSessionResponse{}, nil
}

func (a *v2TestAgent) CloseSession(context.Context, agent2.Client, schema2.CloseSessionRequest) (schema2.CloseSessionResponse, error) {
	return schema2.CloseSessionResponse{}, nil
}

func (a *v2TestAgent) CancelSession(schema2.CancelSessionNotification) error { return nil }

func startRouter(t *testing.T, router *Agent) *bufio.ReadWriter {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() { done <- router.Serve(context.Background(), local, local) }()
	t.Cleanup(func() {
		_ = peer.Close()
		_ = local.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("router did not stop")
		}
	})
	return bufio.NewReadWriter(bufio.NewReader(peer), bufio.NewWriter(peer))
}

func writeInitialize(t *testing.T, rw *bufio.ReadWriter, params any) {
	t.Helper()
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(rw).Encode(struct {
		Version string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{Version: "2.0", ID: 1, Method: "initialize", Params: encoded}); err != nil {
		t.Fatal(err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
}

func TestRouterRoutesFutureVersionToV2(t *testing.T) {
	requests := make(chan schema2.InitializeRequest, 1)
	rw := startRouter(t, New().
		WithV1(&v1TestAgent{requests: nil}).
		WithV2(&v2TestAgent{requests: requests}))
	writeInitialize(t, rw, map[string]any{
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
	rw := startRouter(t, New().WithV1(&v1TestAgent{requests: requests}))
	writeInitialize(t, rw, map[string]any{
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
	rw := startRouter(t, New().WithV2(&v2TestAgent{requests: requests}))
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
	rw := startRouter(t, New().WithV2(&v2TestAgent{requests: make(chan schema2.InitializeRequest, 1)}))
	writeInitialize(t, rw, map[string]any{
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
	rw := startRouter(t, New().WithV2(&v2TestAgent{requests: requests}))
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
	rw := startRouter(t, New().WithV2(&v2TestAgent{requests: requests}))
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
