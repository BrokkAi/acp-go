package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func pipeClient(t *testing.T, h Handler, n Notifications) (*Connection, net.Conn) {
	t.Helper()
	local, peer := net.Pipe()
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	c := Connect(local, local, h, n)
	t.Cleanup(func() { _ = peer.Close(); _ = c.Close() })
	return c, peer
}
func TestBidirectionalCallsAndNotificationOrder(t *testing.T) {
	var messages strings.Builder
	c, peer := pipeClient(t, func(ctx context.Context, method string, p json.RawMessage) (any, error) {
		if method != "test/read" {
			return nil, fmt.Errorf("unexpected %s", method)
		}
		return map[string]string{"content": "data"}, nil
	}, func(method string, p json.RawMessage) error {
		var value string
		if err := json.Unmarshal(p, &value); err != nil {
			return err
		}
		messages.WriteString(value)
		return nil
	})
	server := make(chan error, 1)
	go func() {
		d := json.NewDecoder(peer)
		e := json.NewEncoder(peer)
		var prompt packet
		if err := d.Decode(&prompt); err != nil {
			server <- err
			return
		}
		_ = e.Encode(packet{Version: "2.0", ID: json.RawMessage(`"agent-7"`), Method: "test/read", Params: json.RawMessage(`{}`)})
		var response packet
		if err := d.Decode(&response); err != nil {
			server <- err
			return
		}
		if string(response.ID) != `"agent-7"` || string(response.Result) != `{"content":"data"}` {
			server <- fmt.Errorf("bad callback response: %+v", response)
			return
		}
		for _, s := range []string{"one", "two", "three"} {
			b, _ := json.Marshal(s)
			_ = e.Encode(packet{Version: "2.0", Method: "session/update", Params: b})
		}
		_ = e.Encode(packet{Version: "2.0", ID: prompt.ID, Result: json.RawMessage(`{"stopReason":"end_turn"}`)})
		_ = peer.Close()
		server <- nil
	}()
	var result map[string]string
	if err := c.Call(context.Background(), "session/prompt", map[string]string{"sessionId": "s"}, &result); err != nil {
		t.Fatal(err)
	}
	if result["stopReason"] != "end_turn" || messages.String() != "onetwothree" {
		t.Fatalf("response raced ahead of updates: %v %q", result, messages.String())
	}
	if err := <-server; err != nil {
		t.Fatal(err)
	}
}
func TestIncomingCancellationDoesNotBlockOtherRequests(t *testing.T) {
	c, peer := pipeClient(t, func(ctx context.Context, method string, p json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, nil)
	_ = c
	e := json.NewEncoder(peer)
	d := json.NewDecoder(peer)
	_ = e.Encode(packet{Version: "2.0", ID: json.RawMessage(`"wait"`), Method: "terminal/wait_for_exit", Params: json.RawMessage(`{}`)})
	_ = e.Encode(packet{Version: "2.0", Method: "$/cancel_request", Params: json.RawMessage(`{"requestId":"wait"}`)})
	var p packet
	if err := d.Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.Error == nil || p.Error.Code != -32800 || string(p.ID) != `"wait"` {
		t.Fatalf("bad cancelled response: %+v", p)
	}
}
func TestOutgoingCancellationAndUnknownMethod(t *testing.T) {
	c, peer := pipeClient(t, nil, nil)
	seen := make(chan packet, 1)
	go func() { d := json.NewDecoder(peer); var p packet; _ = d.Decode(&p); _ = d.Decode(&p); seen <- p }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "long_request", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline: %v", err)
	}
	p := <-seen
	if p.Method != "$/cancel_request" {
		t.Fatalf("missing cancellation: %+v", p)
	}
	_ = json.NewEncoder(peer).Encode(packet{Version: "2.0", ID: json.RawMessage(`9`), Method: "unknown", Params: json.RawMessage(`{}`)})
	if err := json.NewDecoder(peer).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.Error == nil || p.Error.Code != -32601 {
		t.Fatalf("missing method-not-found: %+v", p)
	}
}
func TestInvalidFrameAndBlockedWriter(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		c, peer := pipeClient(t, nil, nil)
		_, _ = io.WriteString(peer, "not-json\n")
		select {
		case <-c.Done():
			if c.Err() == nil {
				t.Fatal("missing failure")
			}
		case <-time.After(time.Second):
			t.Fatal("invalid frame accepted")
		}
	})
	t.Run("blocked write", func(t *testing.T) {
		c, _ := pipeClient(t, nil, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		if err := c.Call(ctx, "initialize", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("blocked write did not cancel: %v", err)
		}
	})
}

type cancellingWriter struct {
	net.Conn
	cancel context.CancelFunc
}

func (w *cancellingWriter) Write(p []byte) (int, error) {
	n, err := w.Conn.Write(p)
	if w.cancel != nil && err == nil {
		w.cancel()
		w.cancel = nil
		// The peer has received the request but Write has not returned yet.
		// Cancellation must allow this successful write to finish.
		time.Sleep(20 * time.Millisecond)
	}
	return n, err
}

func TestCancellationAfterDeliveredRequestKeepsConnection(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := Connect(local, &cancellingWriter{Conn: local, cancel: cancel}, nil, nil)
	defer c.Close()
	done := make(chan error, 1)
	go func() { done <- c.Call(ctx, "first", nil, nil) }()
	decoder := json.NewDecoder(peer)
	var request packet
	if err := decoder.Decode(&request); err != nil {
		t.Fatal(err)
	}
	var cancellation packet
	if err := decoder.Decode(&cancellation); err != nil {
		t.Fatalf("connection closed instead of cancelling the delivered request: %v", err)
	}
	if cancellation.Method != "$/cancel_request" {
		t.Fatalf("expected cancellation, got %+v", cancellation)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("first request: %v", err)
	}
	go func() { done <- c.Call(context.Background(), "second", nil, nil) }()
	if err := decoder.Decode(&request); err != nil {
		t.Fatal(err)
	}
	if request.Method != "second" {
		t.Fatalf("unexpected follow-up request: %+v", request)
	}
	if err := json.NewEncoder(peer).Encode(packet{Version: "2.0", ID: request.ID, Result: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestVersionNegotiation(t *testing.T) {
	c, peer := pipeClient(t, nil, nil)
	go func() {
		var p packet
		_ = json.NewDecoder(peer).Decode(&p)
		_ = json.NewEncoder(peer).Encode(packet{Version: "2.0", ID: p.ID, Result: json.RawMessage(`{"protocolVersion":2}`)})
	}()
	if _, err := c.Initialize(context.Background(), Capabilities{}); err == nil {
		t.Fatal("accepted unsupported version")
	}
}

func TestFinalReplySurvivesImmediateExit(t *testing.T) {
	for i := 0; i < 100; i++ {
		local, peer := net.Pipe()
		c := Connect(local, local, nil, nil)
		go func() {
			defer peer.Close()
			var p packet
			if json.NewDecoder(peer).Decode(&p) == nil {
				_ = json.NewEncoder(peer).Encode(packet{Version: "2.0", ID: p.ID, Result: json.RawMessage(`{"ok":true}`)})
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var result struct{ OK bool }
		err := c.Call(ctx, "fast", nil, &result)
		cancel()
		_ = c.Close()
		if err != nil || !result.OK {
			t.Fatalf("lost final reply on iteration %d: %v", i, err)
		}
	}
}

func TestClientIdentity(t *testing.T) {
	c, peer := pipeClient(t, nil, nil)
	seen := make(chan map[string]json.RawMessage, 1)
	go func() {
		var p packet
		_ = json.NewDecoder(peer).Decode(&p)
		var params map[string]json.RawMessage
		_ = json.Unmarshal(p.Params, &params)
		seen <- params
		_ = json.NewEncoder(peer).Encode(packet{Version: "2.0", ID: p.ID, Result: json.RawMessage(`{"protocolVersion":1}`)})
	}()
	if _, err := c.InitializeWithInfo(context.Background(), Capabilities{}, ClientInfo{Name: "fixture", Version: "2.3.4"}); err != nil {
		t.Fatal(err)
	}
	var info ClientInfo
	if err := json.Unmarshal((<-seen)["clientInfo"], &info); err != nil || info.Name != "fixture" || info.Version != "2.3.4" {
		t.Fatalf("wrong identity: %+v %v", info, err)
	}
}

func TestClientAllowsOnlyOneSuccessfulInitialize(t *testing.T) {
	c, peer := pipeClient(t, nil, nil)
	responses := make(chan struct{}, 1)
	go func() {
		defer close(responses)
		for range 1 {
			var request packet
			if err := json.NewDecoder(peer).Decode(&request); err != nil {
				return
			}
			_ = json.NewEncoder(peer).Encode(packet{
				Version: "2.0",
				ID:      request.ID,
				Result:  json.RawMessage(`{"protocolVersion":1}`),
			})
		}
	}()
	if _, err := c.InitializeWithInfo(context.Background(), Capabilities{}, ClientInfo{Name: "once", Version: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InitializeWithInfo(context.Background(), Capabilities{}, ClientInfo{Name: "twice", Version: "1"}); err == nil ||
		!strings.Contains(err.Error(), "may only be initialized once") {
		t.Fatalf("duplicate initialize error = %v", err)
	}
	<-responses
}

// The peer can consume a response before its Write call returns.
type delayedResponseWriter struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (w *delayedResponseWriter) Write(p []byte) (int, error) {
	n, err := w.Conn.Write(p)
	if err == nil {
		<-w.closed
	}
	return n, err
}
func (w *delayedResponseWriter) Close() error {
	w.once.Do(func() { close(w.closed) })
	return w.Conn.Close()
}

func TestReplacementAfterDeliveredResponse(t *testing.T) {
	local, peer := net.Pipe()
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	writer := &delayedResponseWriter{Conn: local, closed: make(chan struct{})}
	started := make(chan string, 33)
	finish := make(chan struct{})
	c := Connect(local, writer, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		started <- method
		if method == "first" {
			select {
			case <-finish:
			case <-ctx.Done():
			}
		} else {
			<-ctx.Done()
		}
		return nil, ctx.Err()
	}, nil)
	defer c.Close()
	defer peer.Close()
	encoder := json.NewEncoder(peer)
	for i := 0; i < 32; i++ {
		method := "held"
		if i == 0 {
			method = "first"
		}
		if err := encoder.Encode(packet{Version: "2.0", ID: json.RawMessage(fmt.Sprint(i)), Method: method}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("handler did not start")
		}
	}
	close(finish)
	var response packet
	if err := json.NewDecoder(peer).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if string(response.ID) != "0" {
		t.Fatalf("unexpected response: %+v", response)
	}
	if err := encoder.Encode(packet{Version: "2.0", ID: json.RawMessage("32"), Method: "replacement"}); err != nil {
		t.Fatal(err)
	}
	select {
	case method := <-started:
		if method != "replacement" {
			t.Fatalf("unexpected handler %q", method)
		}
	case <-c.Done():
		t.Fatalf("valid replacement closed connection: %v", c.Err())
	case <-time.After(2 * time.Second):
		t.Fatal("replacement did not start")
	}
}

func TestIncomingRequestConcurrencyLimit(t *testing.T) {
	started := make(chan struct{}, 32)
	c, peer := pipeClient(t, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}, nil)
	encoder := json.NewEncoder(peer)
	for i := 0; i < 32; i++ {
		if err := encoder.Encode(packet{Version: "2.0", ID: json.RawMessage(fmt.Sprint(i)), Method: "hold"}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("handler did not start")
		}
	}
	if err := encoder.Encode(packet{Version: "2.0", ID: json.RawMessage("32"), Method: "overflow"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
		if err := c.Err(); err == nil || !strings.Contains(err.Error(), "too many simultaneous") {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrency limit was not enforced")
	}
}
