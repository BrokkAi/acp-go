package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
)

func startExtendedRuntime(t *testing.T, runtime *Runtime) *acp.Connection {
	t.Helper()
	agentSide, clientSide := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = agentSide.SetDeadline(deadline)
	_ = clientSide.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() { done <- runtime.Serve(context.Background(), agentSide, agentSide) }()
	t.Cleanup(func() {
		_ = clientSide.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	return acp.Connect(clientSide, clientSide, nil, nil)
}

func TestRuntimeServesExtensionsBehindTheInitializationGate(t *testing.T) {
	ctx := context.Background()
	calls := make(chan string, 4)
	runtime := New(testAgent{})
	runtime.Extensions = func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		calls <- method
		return map[string]string{"served": method}, nil
	}
	connection := startExtendedRuntime(t, runtime)

	err := connection.Call(ctx, "_vendor/extension", map[string]any{}, nil)
	requireInvalidRequest(t, err, "ACP initialization must complete before `_vendor/extension` can be used")
	select {
	case method := <-calls:
		t.Fatalf("extension served %s before initialize", method)
	default:
	}

	initializeClient(t, connection, acp.Capabilities{})
	var result map[string]string
	if err := connection.Call(ctx, "_vendor/extension", map[string]any{}, &result); err != nil || result["served"] != "_vendor/extension" {
		t.Fatalf("extension result = %v, %v", result, err)
	}
	if err := newSessionCall(ctx, connection); err != nil {
		t.Fatal(err)
	}
	if method := <-calls; method != "_vendor/extension" {
		t.Fatalf("extension served %s", method)
	}
	select {
	case method := <-calls:
		t.Fatalf("extension also served %s", method)
	default:
	}
}

func TestRuntimeWithoutExtensionsAnswersMethodNotFound(t *testing.T) {
	connection := startExtendedRuntime(t, New(testAgent{}))
	initializeClient(t, connection, acp.Capabilities{})
	var rpcErr *acp.RPCError
	if err := connection.Call(context.Background(), "_vendor/extension", map[string]any{}, nil); !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("unknown method error = %v", err)
	}
}

func TestRuntimeSendsAndValidatesTheExtendedInitializeResult(t *testing.T) {
	ctx := context.Background()
	runtime := New(testAgent{})
	runtime.InitializeResult = func(response schema.InitializeResponse) (any, error) {
		return map[string]any{
			"protocolVersion":   response.ProtocolVersion,
			"agentCapabilities": map[string]any{"_vendor": map[string]any{"feature": true}},
		}, nil
	}
	connection := startExtendedRuntime(t, runtime)
	var raw json.RawMessage
	if err := connection.Call(ctx, schema.InitializeMethodName, schema.InitializeRequest{ProtocolVersion: acp.Version}, &raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"_vendor":{"feature":true}`) {
		t.Fatalf("initialize result = %s", raw)
	}

	for name, hook := range map[string]func(schema.InitializeResponse) (any, error){
		"hook error": func(schema.InitializeResponse) (any, error) { return nil, errors.New("cannot extend") },
		"wrong version": func(response schema.InitializeResponse) (any, error) {
			response.ProtocolVersion = 2
			return response, nil
		},
		"not an initialize response": func(schema.InitializeResponse) (any, error) { return "initialized", nil },
	} {
		runtime := New(testAgent{})
		runtime.InitializeResult = hook
		connection := startExtendedRuntime(t, runtime)
		if err := connection.Call(ctx, schema.InitializeMethodName, schema.InitializeRequest{ProtocolVersion: acp.Version}, nil); err == nil {
			t.Fatalf("%s: initialize succeeded", name)
		}
	}
}
