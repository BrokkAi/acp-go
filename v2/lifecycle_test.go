package v2

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// readRequest reads one request frame the client wrote.
func readRequest(t *testing.T, reader *bufio.Reader) (string, json.RawMessage, string) {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatal(err)
	}
	return frame.Method, frame.ID, strings.TrimSpace(line)
}

func answer(t *testing.T, peer io.Writer, id json.RawMessage, result string) {
	t.Helper()
	if _, err := io.WriteString(peer, `{"jsonrpc":"2.0","id":`+string(id)+`,"result":`+result+"}\n"); err != nil {
		t.Fatal(err)
	}
}

const v2Result = `{"protocolVersion":2,"info":{"name":"agent","version":"1"},"capabilities":{"session":{}}}`

// TestV2ClientTracksEveryInitialize follows the reference SDK's initiator: an
// initialize sent through the embedded acp.Connection is still prepared
// (protocolVersion set to 2 in place) and recorded; SendInitialize writes
// protocolVersion first; and an initialize whose caller stopped waiting
// completes when its response arrives.
func TestV2ClientTracksEveryInitialize(t *testing.T) {
	local, peer := net.Pipe()
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	client := Connect(local, local, nil, nil)
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	reader := bufio.NewReader(peer)
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		done <- client.Connection.Call(ctx, schema.InitializeMethodName, json.RawMessage(`{"info":{"name":"c","version":"1"},"protocolVersion":9}`), nil)
	}()
	_, id, line := readRequest(t, reader)
	if !strings.Contains(line, `"params":{"info":{"name":"c","version":"1"},"protocolVersion":2}`) {
		t.Fatalf("initialize frame = %s", line)
	}
	answer(t, peer, id, v2Result)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := client.initializationSnapshot(); !ok {
		t.Fatal("initialize through the embedded connection was not recorded")
	}

	// A repeat whose caller gives up stays in progress and completes late.
	cancelled, cancel := context.WithCancel(ctx)
	go func() {
		_, err := client.InitializeWithInfo(cancelled, Capabilities{}, ClientInfo{Name: "late", Version: "1"})
		done <- err
	}()
	_, id, line = readRequest(t, reader)
	if !strings.Contains(line, `"method":"initialize","params":{"protocolVersion":2,"info":{"name":"late","version":"1"},"capabilities":{}}`) {
		t.Fatalf("initialize frame = %s", line)
	}
	cancel()
	if method, _, _ := readRequest(t, reader); method != schema.CancelRequestMethodName {
		t.Fatalf("expected $/cancel_request, got %s", method)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled initialize: %v", err)
	}
	if err := client.Call(ctx, schema.SessionListMethodName, schema.ListSessionsRequest{}, nil); !isInitializationRequired(err, schema.SessionListMethodName) {
		t.Fatalf("request during the abandoned repeat: %v", err)
	}
	answer(t, peer, id, `{"protocolVersion":2,"info":{"name":"late-agent","version":"1"}}`)
	deadline := time.Now().Add(time.Second)
	for {
		if initialization, _ := client.initializationSnapshot(); initialization.Info.Name == "late-agent" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the late response did not complete the initialization")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestV2ClientReadsTheResponseLeniently: like the reference schema, a v2
// initialize response ignores nested members that do not parse and case
// variants of known names; only info is required.
func TestV2ClientReadsTheResponseLeniently(t *testing.T) {
	local, peer := net.Pipe()
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	client := Connect(local, local, nil, nil)
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	reader := bufio.NewReader(peer)
	done := make(chan error, 1)
	var initialization Initialization
	go func() {
		var err error
		initialization, err = client.Initialize(context.Background(), Capabilities{})
		done <- err
	}()
	_, id, _ := readRequest(t, reader)
	answer(t, peer, id, `{"protocolVersion":2,"info":{"name":"a","version":"1","title":5},"Info":7,"capabilities":{"session":{"delete":"bad","prompt":{}},"Session":5},"authMethods":5,"_meta":"x"}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if initialization.Info.Name != "a" || initialization.Capabilities == nil || initialization.Capabilities.Session == nil ||
		initialization.Capabilities.Session.Delete != nil || initialization.Capabilities.Session.Prompt == nil {
		t.Fatalf("initialization = %+v", initialization)
	}
}

// TestV2ClientRefusesAPrewrappedInitialize follows the reference SDK's
// invalid_prewrapped_initialize: an initialize already wrapped in
// proxy/successor is refused before it is written.
func TestV2ClientRefusesAPrewrappedInitialize(t *testing.T) {
	client, _ := readyClient(t, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		t.Errorf("%s reached the wire", method)
		return nil, nil
	}, nil)
	err := client.Call(context.Background(), "proxy/successor", map[string]any{"method": "initialize", "params": map[string]any{}}, nil)
	if err == nil || !strings.Contains(err.Error(), "-32600") {
		t.Fatalf("prewrapped initialize: %v", err)
	}
}

// TestV2ClientRefusesResponsesDuringARepeat follows the reference SDK's
// outgoing_response_to: a successful answer to the agent's request becomes
// -32600 while a repeat is in progress.
func TestV2ClientRefusesResponsesDuringARepeat(t *testing.T) {
	local, peer := net.Pipe()
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	entered := make(chan struct{})
	release := make(chan struct{})
	client := Connect(local, local, func(context.Context, string, json.RawMessage) (any, error) {
		close(entered)
		<-release
		return map[string]bool{"ok": true}, nil
	}, nil)
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	reader := bufio.NewReader(peer)
	initializeRaw(t, client, peer, reader)

	if _, err := io.WriteString(peer, `{"jsonrpc":"2.0","id":"agent-1","method":"fs/read_text_file","params":{}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	<-entered
	done := make(chan error, 1)
	go func() { _, err := client.Initialize(context.Background(), Capabilities{}); done <- err }()
	_, id, _ := readRequest(t, reader)
	close(release)
	want := `{"jsonrpc":"2.0","id":"agent-1","error":{"code":-32600,"message":"Invalid request","data":"ACP initialization must complete before ` + "`fs/read_text_file`" + ` can be used"}}`
	if line, err := reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != want {
		t.Fatalf("response during a repeat = %q, %v", line, err)
	}
	answer(t, peer, id, v2Result)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
