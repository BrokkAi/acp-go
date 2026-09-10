package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

func TestInboundMixedBatchReturnsOneResponseArray(t *testing.T) {
	notifications := make(chan string, 1)
	c, peer := pipeClient(t, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		if method != "test/echo" {
			return nil, &RPCError{Code: -32601}
		}
		var params struct{ Message string }
		if err := json.Unmarshal(raw, &params); err != nil || params.Message == "" {
			return nil, &RPCError{Code: -32602, Message: "invalid params"}
		}
		if params.Message == "handler error" {
			return nil, errors.New("handler error")
		}
		return map[string]string{"result": "echo: " + params.Message}, nil
	}, func(method string, raw json.RawMessage) error {
		var params struct{ Message string }
		if err := json.Unmarshal(raw, &params); err != nil {
			return err
		}
		notifications <- params.Message
		return nil
	})
	_ = c
	batch, err := json.Marshal([]any{
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "test/echo", "params": map[string]string{"message": "request sibling"}},
		map[string]any{"jsonrpc": "2.0", "method": "test/notify", "params": map[string]string{"message": "notification sibling"}},
		17,
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "test/echo", "params": map[string]string{"wrong_field": "invalid"}},
		map[string]any{"jsonrpc": "2.0", "id": 3, "method": "test/echo", "params": map[string]string{"message": "handler error"}},
		map[string]any{"jsonrpc": "2.0", "id": 99, "result": nil, "error": map[string]any{"code": -32603, "message": "both"}},
		map[string]any{"jsonrpc": "1.0", "id": 100, "method": "test/echo", "params": map[string]string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(peer, "%s\n", batch); err != nil {
		t.Fatal(err)
	}
	select {
	case notification := <-notifications:
		if notification != "notification sibling" {
			t.Fatalf("notification = %q", notification)
		}
	case <-time.After(time.Second):
		t.Fatal("notification was not delivered")
	}

	var responses []map[string]json.RawMessage
	if err := json.NewDecoder(peer).Decode(&responses); err != nil {
		t.Fatal(err)
	}
	if len(responses) != 5 {
		t.Fatalf("responses = %+v", responses)
	}
	assertBatchResponse(t, responses[0], "1", "", "")
	assertBatchResponse(t, responses[1], "null", "-32600", "")
	assertBatchResponse(t, responses[2], "2", "-32602", "")
	assertBatchResponse(t, responses[3], "3", "-32603", "")
	assertBatchResponse(t, responses[4], "null", "-32600", "")
}

func assertBatchResponse(t *testing.T, response map[string]json.RawMessage, id, code, resultContains string) {
	t.Helper()
	if string(response["id"]) != id {
		t.Fatalf("id = %s, want %s", response["id"], id)
	}
	if code != "" {
		var rpcError struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal(response["error"], &rpcError); err != nil {
			t.Fatal(err)
		}
		if rpcError.Code != -32600 && fmt.Sprint(rpcError.Code) != code {
			t.Fatalf("code = %d, want %s", rpcError.Code, code)
		}
	}
}

func TestEmptyBatchAndNotificationOnlyBatch(t *testing.T) {
	c, peer := pipeClient(t, func(context.Context, string, json.RawMessage) (any, error) {
		return map[string]string{"ok": "yes"}, nil
	}, nil)
	_ = c
	if _, err := io.WriteString(peer, "[]\n"); err != nil {
		t.Fatal(err)
	}
	var invalid map[string]json.RawMessage
	if err := json.NewDecoder(peer).Decode(&invalid); err != nil {
		t.Fatal(err)
	}
	if string(invalid["id"]) != "null" {
		t.Fatalf("empty batch response = %+v", invalid)
	}

	notificationBatch, err := json.Marshal([]map[string]any{
		{"jsonrpc": "2.0", "method": "notify/one", "params": map[string]string{}},
		{"jsonrpc": "2.0", "method": "notify/two", "params": map[string]string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(peer, "%s\n", notificationBatch); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(peer, `{"jsonrpc":"2.0","id":9,"method":"after","params":{}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	var standalone map[string]json.RawMessage
	if err := json.NewDecoder(peer).Decode(&standalone); err != nil {
		t.Fatal(err)
	}
	if string(standalone["id"]) != "9" {
		t.Fatalf("standalone response = %+v", standalone)
	}
}

func TestResponseBatchRoutesPendingCalls(t *testing.T) {
	c, peer := pipeClient(t, nil, nil)
	requests := make(chan []packet, 1)
	go func() {
		var packets []packet
		if err := json.NewDecoder(peer).Decode(&packets); err != nil {
			return
		}
		requests <- packets
		responses := make([]packet, len(packets))
		for i, request := range packets {
			responses[len(packets)-1-i] = packet{
				Version: "2.0", ID: request.ID,
				Result: json.RawMessage(fmt.Sprintf(`{"value":%q}`, string(request.ID))),
			}
		}
		_ = json.NewEncoder(peer).Encode(responses)
	}()
	var first, second struct{ Value string }
	errs := c.CallBatch(context.Background(), []BatchCall{
		{Method: "first", Params: nil, Result: &first},
		{Method: "second", Params: nil, Result: &second},
	})
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if first.Value == second.Value {
		t.Fatalf("responses were crossed: %+v %+v", first, second)
	}
}

func TestInboundBatchProcessesInitializeBeforeSibling(t *testing.T) {
	initialized := false
	c, peer := pipeClient(t, func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case "initialize":
			initialized = true
			return map[string]any{"protocolVersion": 1}, nil
		case "after/initialize":
			if !initialized {
				return nil, errors.New("sibling ran before initialize")
			}
			return map[string]string{"ok": "yes"}, nil
		default:
			return nil, &RPCError{Code: -32601}
		}
	}, nil)
	_ = c
	initializeBatch, err := json.Marshal([]map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]string{}},
		{"jsonrpc": "2.0", "id": 2, "method": "after/initialize", "params": map[string]string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(peer, "%s\n", initializeBatch); err != nil {
		t.Fatal(err)
	}
	var responses []map[string]json.RawMessage
	if err := json.NewDecoder(peer).Decode(&responses); err != nil {
		t.Fatal(err)
	}
	if len(responses) != 2 || responses[1]["error"] != nil {
		t.Fatalf("responses = %+v", responses)
	}
}

func TestIncompleteBatchDoesNotBlockStandaloneRequest(t *testing.T) {
	release := make(chan struct{})
	c, peer := pipeClient(t, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "blocked" {
			<-release
		}
		return map[string]string{"method": method}, nil
	}, nil)
	_ = c
	batch, err := json.Marshal([]map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "blocked", "params": map[string]string{}},
		{"jsonrpc": "2.0", "id": 2, "method": "immediate", "params": map[string]string{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(peer, "%s\n", batch); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(peer, `{"jsonrpc":"2.0","id":3,"method":"standalone","params":{}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	var standalone map[string]json.RawMessage
	if err := json.NewDecoder(peer).Decode(&standalone); err != nil {
		t.Fatal(err)
	}
	if string(standalone["id"]) != "3" {
		t.Fatalf("standalone response = %+v", standalone)
	}
	close(release)
	var responses []map[string]json.RawMessage
	if err := json.NewDecoder(peer).Decode(&responses); err != nil {
		t.Fatal(err)
	}
	if len(responses) != 2 {
		t.Fatalf("batch responses = %+v", responses)
	}
}

func TestBatchDuplicateRequestIDsKeepSeparateSlots(t *testing.T) {
	results := make(chan string, 2)
	c, peer := pipeClient(t, func(_ context.Context, _ string, raw json.RawMessage) (any, error) {
		var params struct{ Message string }
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		results <- params.Message
		return map[string]string{"result": params.Message}, nil
	}, nil)
	_ = c
	batch, err := json.Marshal([]map[string]any{
		{"jsonrpc": "2.0", "id": 21, "method": "echo", "params": map[string]string{"message": "first"}},
		{"jsonrpc": "2.0", "id": 21, "method": "echo", "params": map[string]string{"message": "second"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(peer, "%s\n", batch); err != nil {
		t.Fatal(err)
	}
	var responses []map[string]json.RawMessage
	if err := json.NewDecoder(peer).Decode(&responses); err != nil {
		t.Fatal(err)
	}
	if len(responses) != 2 || string(responses[0]["id"]) != "21" || string(responses[1]["id"]) != "21" {
		t.Fatalf("responses = %+v", responses)
	}
	seen := map[string]bool{<-results: true, <-results: true}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("duplicate requests were not dispatched: %+v", seen)
	}
}
