package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// startRawRuntime serves implementation over a pipe and returns the client
// end, for tests that write frames byte for byte.
func startRawRuntime(t *testing.T, implementation Agent) (net.Conn, *bufio.Reader) {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	serveDone := make(chan error, 1)
	go func() { serveDone <- New(implementation).Serve(context.Background(), peer, peer) }()
	t.Cleanup(func() {
		_ = local.Close()
		_ = peer.Close()
		<-serveDone
	})
	return local, bufio.NewReader(local)
}

// caller is the Call method shared by a v2 client connection and a raw one.
type caller interface {
	Call(ctx context.Context, method string, params, result any) error
}

// rawClient is a version-neutral connection to the runtime that sends
// initialize exactly as given, so tests can send what a v2 client connection
// would negotiate or refuse locally.
type rawClient struct {
	*acp.Connection
}

// Initialize sends a valid v2 initialize.
func (c rawClient) Initialize(ctx context.Context, _ acpv2.Capabilities) (schema.InitializeResponse, error) {
	var response schema.InitializeResponse
	err := c.Call(ctx, schema.InitializeMethodName, v2InitializeRequest, &response)
	return response, err
}

func startRawClient(t *testing.T, implementation Agent) rawClient {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	serveDone := make(chan error, 1)
	go func() { serveDone <- New(implementation).Serve(context.Background(), peer, peer) }()
	client := acp.Connect(local, local, nil, nil, acp.WithProtocolVersion(2))
	t.Cleanup(func() {
		_ = client.Close()
		_ = peer.Close()
		<-serveDone
	})
	return rawClient{client}
}

func readFrame(t *testing.T, lines *bufio.Reader) string {
	t.Helper()
	line, err := lines.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(line, "\n")
}

func writeFrames(t *testing.T, conn net.Conn, frames string) {
	t.Helper()
	if _, err := conn.Write([]byte(frames)); err != nil {
		t.Fatal(err)
	}
}

func initializeFrame(id int) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"client","version":"%d"}}}`+"\n", id, id)
}

// TestRuntimeAppliesTheRepeatWindowInWireOrder follows the reference SDK's
// incoming actor, which applies the lifecycle to each request as it reads it:
// a repeated initialize and a session/new sent back to back after the first
// initialize completed see the repeat begin before session/new, so the repeat
// succeeds and session/new is refused, however the handlers are scheduled.
func TestRuntimeAppliesTheRepeatWindowInWireOrder(t *testing.T) {
	implementation := newGatedInitializeAgent()
	conn, lines := startRawRuntime(t, implementation)

	writeFrames(t, conn, initializeFrame(1))
	<-implementation.entered
	implementation.release <- initializeSucceeds()
	if line := readFrame(t, lines); !strings.Contains(line, `"id":1,"result"`) {
		t.Fatalf("first initialize = %s", line)
	}

	writeFrames(t, conn, initializeFrame(2)+`{"jsonrpc":"2.0","id":3,"method":"session/new","params":{"cwd":"/repo","mcpServers":[]}}`+"\n")
	<-implementation.entered
	// The repeat is still running in the agent, so the only answer so far is
	// session/new's refusal.
	want := `{"jsonrpc":"2.0","id":3,"error":{"code":-32600,"message":"Invalid request","data":"ACP initialization must complete before ` + "`session/new`" + ` can be used"}}`
	if line := readFrame(t, lines); line != want {
		t.Fatalf("session/new = %s, want %s", line, want)
	}
	implementation.release <- initializeSucceeds()
	if line := readFrame(t, lines); !strings.Contains(line, `"id":2,"result"`) {
		t.Fatalf("repeated initialize = %s", line)
	}

	// The refused session/new did not close the repeat window.
	writeFrames(t, conn, initializeFrame(4))
	<-implementation.entered
	implementation.release <- initializeSucceeds()
	if line := readFrame(t, lines); !strings.Contains(line, `"id":4,"result"`) {
		t.Fatalf("third initialize = %s", line)
	}
}

// TestRuntimeAdmittedRequestClosesTheRepeatWindowInWireOrder: an admitted
// request read before a repeat closes the window, even when its handler has
// not run yet.
func TestRuntimeAdmittedRequestClosesTheRepeatWindowInWireOrder(t *testing.T) {
	implementation := newGatedInitializeAgent()
	conn, lines := startRawRuntime(t, implementation)
	writeFrames(t, conn, initializeFrame(1))
	<-implementation.entered
	implementation.release <- initializeSucceeds()
	readFrame(t, lines)

	writeFrames(t, conn, `{"jsonrpc":"2.0","id":2,"method":"session/list","params":{}}`+"\n"+initializeFrame(3))
	got := map[string]bool{readFrame(t, lines): true, readFrame(t, lines): true}
	refusal := `{"jsonrpc":"2.0","id":3,"error":{"code":-32600,"message":"Invalid request","data":"ACP initialization cannot be repeated after any other request; reconnect to initialize again"}}`
	if !got[refusal] {
		t.Fatalf("answers = %v, want %s", got, refusal)
	}
	if calls := implementation.calls.Load(); calls != 1 {
		t.Fatalf("Initialize calls = %d, want 1", calls)
	}
}

// TestRuntimeDropsNotificationsBeforeInitialization follows the reference
// responder's ensure_notification_allowed: a notification that arrives before
// an initialize has completed is ignored.
func TestRuntimeDropsNotificationsBeforeInitialization(t *testing.T) {
	cancelled := make(chan schema.CancelSessionNotification, 2)
	conn, lines := startRawRuntime(t, &echoAgent{cancelled: cancelled})
	writeFrames(t, conn, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"early"}}`+"\n"+initializeFrame(1))
	readFrame(t, lines)
	writeFrames(t, conn, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"late"}}`+"\n"+
		`{"jsonrpc":"2.0","id":2,"method":"session/list","params":{}}`+"\n")
	readFrame(t, lines)
	select {
	case notification := <-cancelled:
		if notification.SessionID != "late" {
			t.Fatalf("cancel before initialize reached the agent: %+v", notification)
		}
	default:
		t.Fatal("cancel after initialize did not reach the agent")
	}
}

// startTalkingRuntime serves an agent whose Initialize sends a request and a
// notification to the client and reports the errors.
type talkingAgent struct {
	*echoAgent
	errs chan error
}

func (a *talkingAgent) Initialize(ctx context.Context, client Client, request schema.InitializeRequest) (schema.InitializeResponse, error) {
	connection := client.(*clientConnection).connection()
	a.errs <- connection.Notify(ctx, "_custom/note", map[string]any{})
	a.errs <- connection.Call(ctx, "_custom/ask", map[string]any{}, nil)
	// An outgoing initialize is validated before the direction is refused.
	a.errs <- connection.Call(ctx, schema.InitializeMethodName, map[string]any{}, nil)
	a.errs <- connection.Call(ctx, schema.InitializeMethodName, map[string]any{"protocolVersion": 7, "info": map[string]string{"name": "a", "version": "1"}}, nil)
	return a.echoAgent.Initialize(ctx, client, request)
}

// TestRuntimeRefusesOutgoingTrafficBeforeInitialization follows the reference
// responder: until initialize completes the agent's own requests and
// notifications fail without being written, and an agent never sends
// initialize.
func TestRuntimeRefusesOutgoingTrafficBeforeInitialization(t *testing.T) {
	implementation := &talkingAgent{echoAgent: &echoAgent{}, errs: make(chan error, 4)}
	conn, lines := startRawRuntime(t, implementation)
	writeFrames(t, conn, initializeFrame(1))
	if line := readFrame(t, lines); !strings.Contains(line, `"id":1,"result"`) {
		t.Fatalf("initialize = %s (the agent wrote something first)", line)
	}
	text := func(s string) string { data, _ := json.Marshal(s); return string(data) }
	for _, want := range []struct {
		code int
		data string
	}{
		{-32600, text("ACP initialization must complete before `_custom/note` can be used")},
		{-32600, text("ACP initialization must complete before `_custom/ask` can be used")},
		{-32602, `{"error":"missing field ` + "`info`" + `","json":{"protocolVersion":2},"phase":"deserialization"}`},
		{-32600, text("ACP clients send `initialize` requests and ACP agents respond to them")},
	} {
		err := <-implementation.errs
		var rpcErr *acp.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != want.code || string(rpcErr.Data) != want.data {
			t.Fatalf("error = %v, want %d %s", err, want.code, want.data)
		}
	}
}

// eagerAgent starts a notifier in Initialize that sends as soon as the
// runtime allows it.
type eagerAgent struct {
	*echoAgent
	sent chan error
}

func (a *eagerAgent) Initialize(ctx context.Context, client Client, request schema.InitializeRequest) (schema.InitializeResponse, error) {
	connection := client.(*clientConnection).connection()
	go func() {
		for {
			err := connection.Notify(context.Background(), "_custom/ready", map[string]any{})
			if err == nil {
				a.sent <- nil
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	time.Sleep(10 * time.Millisecond)
	return a.echoAgent.Initialize(ctx, client, request)
}

// TestRuntimeWritesTheInitializeResponseFirst follows the reference SDK,
// which completes the initialization as its response is sent: nothing the
// agent sends once it may can precede that response on the wire.
func TestRuntimeWritesTheInitializeResponseFirst(t *testing.T) {
	for range 20 {
		implementation := &eagerAgent{echoAgent: &echoAgent{}, sent: make(chan error, 1)}
		conn, lines := startRawRuntime(t, implementation)
		writeFrames(t, conn, initializeFrame(1))
		if line := readFrame(t, lines); !strings.Contains(line, `"id":1,"result"`) {
			t.Fatalf("first frame = %s", line)
		}
		if line := readFrame(t, lines); !strings.Contains(line, `"method":"_custom/ready"`) {
			t.Fatalf("second frame = %s", line)
		}
		<-implementation.sent
	}
}

// TestRuntimeReadsExactV2InitializeLeniently: like the reference schema, an
// exact v2 request ignores a _meta, capabilities, or info title that does not
// parse; only info itself must parse.
func TestRuntimeReadsExactV2InitializeLeniently(t *testing.T) {
	for _, params := range []string{
		`{"protocolVersion":2,"info":{"name":"c","version":"1"},"_meta":"x"}`,
		`{"protocolVersion":2,"info":{"name":"c","version":"1","title":5,"_meta":"x"},"capabilities":{"elicitation":"bad"}}`,
		`{"protocolVersion":2,"info":{"name":"c","version":"1"},"capabilities":"bad"}`,
	} {
		conn, lines := startRawRuntime(t, &echoAgent{})
		writeFrames(t, conn, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+params+"}\n")
		if line := readFrame(t, lines); !strings.Contains(line, `"id":1,"result"`) {
			t.Fatalf("%s: initialize = %s", params, line)
		}
	}
}

// TestRuntimeSetsTheNegotiatedVersionOnTheResponse: the reference SDK
// overwrites the agent's protocolVersion with the negotiated one.
func TestRuntimeSetsTheNegotiatedVersionOnTheResponse(t *testing.T) {
	implementation := newGatedInitializeAgent()
	conn, lines := startRawRuntime(t, implementation)
	writeFrames(t, conn, initializeFrame(1))
	<-implementation.entered
	outcome := initializeSucceeds()
	outcome.response.ProtocolVersion = 7
	implementation.release <- outcome
	if line := readFrame(t, lines); !strings.Contains(line, `"protocolVersion":2`) || strings.Contains(line, `"protocolVersion":7`) {
		t.Fatalf("initialize = %s", line)
	}
}
