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
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

func startExtendedRuntime(t *testing.T, runtime *Runtime) *acpv2.Connection {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	serveDone := make(chan error, 1)
	go func() { serveDone <- runtime.Serve(context.Background(), peer, peer) }()
	client := acpv2.Connect(local, local, nil, nil)
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.Close()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	return client
}

func TestRuntimeServesExtensionsBehindTheInitializationGate(t *testing.T) {
	ctx := context.Background()
	calls := make(chan string, 4)
	runtime := New(&echoAgent{})
	runtime.Extensions = func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		calls <- method
		return map[string]string{"served": method}, nil
	}
	client := startExtendedRuntime(t, runtime)

	err := client.Call(ctx, "_vendor/extension", map[string]any{}, nil)
	requireInvalidRequest(t, err, "ACP initialization must complete before `_vendor/extension` can be used")
	select {
	case method := <-calls:
		t.Fatalf("extension served %s before initialize", method)
	default:
	}

	if _, err := client.Initialize(ctx, acpv2.Capabilities{}); err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := client.Call(ctx, "_vendor/extension", map[string]any{}, &result); err != nil || result["served"] != "_vendor/extension" {
		t.Fatalf("extension result = %v, %v", result, err)
	}
	// Runtime-owned methods never reach the extension.
	if _, err := client.NewSession(ctx, "/repo"); err != nil {
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
	ctx := context.Background()
	client := startExtendedRuntime(t, New(&echoAgent{}))
	if _, err := client.Initialize(ctx, acpv2.Capabilities{}); err != nil {
		t.Fatal(err)
	}
	var rpcErr *acp.RPCError
	if err := client.Call(ctx, "_vendor/extension", map[string]any{}, nil); !errors.As(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("unknown method error = %v", err)
	}
}

func TestRuntimeSendsTheExtendedInitializeResult(t *testing.T) {
	ctx := context.Background()
	runtime := New(&echoAgent{})
	runtime.InitializeResult = func(response schema.InitializeResponse) (any, error) {
		encoded, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}
		var extended map[string]any
		if err := json.Unmarshal(encoded, &extended); err != nil {
			return nil, err
		}
		extended["capabilities"].(map[string]any)["_vendor"] = map[string]any{"feature": true}
		return extended, nil
	}
	client := startExtendedRuntime(t, runtime)
	var raw json.RawMessage
	if _, err := client.InitializeExtended(ctx, acpv2.Capabilities{}, acpv2.ClientInfo{Name: "client", Version: "1"}, &raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"_vendor":{"feature":true}`) {
		t.Fatalf("initialize result = %s", raw)
	}
	if _, err := client.NewSession(ctx, "/repo"); err != nil {
		t.Fatalf("session/new after an extended initialize: %v", err)
	}
}

func TestRuntimeValidatesTheExtendedInitializeResult(t *testing.T) {
	ctx := context.Background()
	// As in the reference SDK, the runtime sets the negotiated version on the
	// value sent, whatever the hook returned.
	for name, hook := range map[string]func(schema.InitializeResponse) (any, error){
		"typed": func(response schema.InitializeResponse) (any, error) {
			response.ProtocolVersion = 1
			return response, nil
		},
		"raw": func(response schema.InitializeResponse) (any, error) {
			return map[string]any{"protocolVersion": 7, "info": response.Info, "capabilities": response.Capabilities}, nil
		},
		"missing": func(response schema.InitializeResponse) (any, error) {
			return map[string]any{"info": response.Info, "capabilities": response.Capabilities}, nil
		},
	} {
		runtime := New(&echoAgent{})
		runtime.InitializeResult = hook
		client := startExtendedRuntime(t, runtime)
		initialization, err := client.Initialize(ctx, acpv2.Capabilities{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if initialization.ProtocolVersion != 2 {
			t.Fatalf("%s: protocolVersion = %d, want 2", name, initialization.ProtocolVersion)
		}
	}

	for name, hook := range map[string]func(schema.InitializeResponse) (any, error){
		"hook error": func(schema.InitializeResponse) (any, error) { return nil, errors.New("cannot extend") },
		"sessions dropped": func(response schema.InitializeResponse) (any, error) {
			return map[string]any{"protocolVersion": 2, "info": response.Info}, nil
		},
		"not an initialize response": func(schema.InitializeResponse) (any, error) { return []int{1}, nil },
	} {
		runtime := New(&echoAgent{})
		runtime.InitializeResult = hook
		client := startExtendedRuntime(t, runtime)
		if _, err := client.Initialize(ctx, acpv2.Capabilities{}); err == nil {
			t.Fatalf("%s: initialize succeeded", name)
		}
	}
}
