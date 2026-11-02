package acp

import (
	"bufio"
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

func pipeClient(t *testing.T, h Handler, n Notifications, options ...ConnectOption) (*Connection, net.Conn) {
	t.Helper()
	local, peer := net.Pipe()
	_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
	c := Connect(local, local, h, n, options...)
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
		var value []string
		if err := json.Unmarshal(p, &value); err != nil {
			return err
		}
		messages.WriteString(strings.Join(value, ""))
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
			b, _ := json.Marshal([]string{s})
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

func TestCancelRequestSendsTypedNotification(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   any
		want string
	}{
		{name: "string", id: "wait", want: `"wait"`},
		{name: "integer", id: 7, want: `7`},
		{name: "raw", id: json.RawMessage(`"agent-3"`), want: `"agent-3"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, peer := pipeClient(t, nil, nil)
			seen := make(chan packet, 1)
			go func() {
				var p packet
				if err := json.NewDecoder(peer).Decode(&p); err == nil {
					seen <- p
				}
			}()
			if err := c.CancelRequest(context.Background(), tc.id); err != nil {
				t.Fatalf("CancelRequest: %v", err)
			}
			var p packet
			select {
			case p = <-seen:
			case <-time.After(time.Second):
				t.Fatal("missing $/cancel_request frame")
			}
			if p.Method != "$/cancel_request" || len(p.ID) != 0 {
				t.Fatalf("unexpected frame: %+v", p)
			}
			var params struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if err := json.Unmarshal(p.Params, &params); err != nil {
				t.Fatal(err)
			}
			if string(params.RequestID) != tc.want {
				t.Fatalf("requestId = %s, want %s", params.RequestID, tc.want)
			}
		})
	}
}

func TestCancelRequestRejectsInvalidIDs(t *testing.T) {
	c, _ := pipeClient(t, nil, nil)
	for _, id := range []any{nil, "", json.RawMessage(``), json.RawMessage(`null`), struct{}{}} {
		if err := c.CancelRequest(context.Background(), id); err == nil {
			t.Fatalf("expected error for %#v", id)
		}
	}
}

func TestBlockedWriter(t *testing.T) {
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

func TestManyConcurrentIncomingRequestsAreAllServed(t *testing.T) {
	// The reference SDK puts no limit on concurrent incoming requests, so a
	// peer that pipelines many of them is served instead of disconnected.
	const count = 100
	started := make(chan struct{}, count)
	release := make(chan struct{})
	c, peer := pipeClient(t, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		started <- struct{}{}
		select {
		case <-release:
			return map[string]bool{"ok": true}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}, nil)
	encoder := json.NewEncoder(peer)
	for i := range count {
		if err := encoder.Encode(packet{Version: "2.0", ID: json.RawMessage(fmt.Sprint(i)), Method: "hold"}); err != nil {
			t.Fatal(err)
		}
	}
	for range count {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("handler did not start; connection error %v", c.Err())
		}
	}
	close(release)
	decoder := json.NewDecoder(peer)
	seen := make(map[string]bool)
	for range count {
		var response packet
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.Error != nil || string(response.Result) != `{"ok":true}` {
			t.Fatalf("response = %+v", response)
		}
		seen[string(response.ID)] = true
	}
	if len(seen) != count {
		t.Fatalf("answered %d distinct requests, want %d", len(seen), count)
	}
}

// batchRejection is the exact line the reference SDK sends for any JSON-RPC
// batch array.
const batchRejection = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"JSON-RPC batches are not supported; send each message individually"}}`

func TestBatchArraysGetOneInvalidRequestAndRunNothing(t *testing.T) {
	batches := map[string]string{
		"empty":             `[]`,
		"padded empty":      " [ ] ",
		"request only":      `[{"jsonrpc":"2.0","id":1,"method":"test/echo","params":{}},{"jsonrpc":"2.0","id":2,"method":"test/echo","params":{}}]`,
		"notification only": `[{"jsonrpc":"2.0","method":"test/notify","params":{}}]`,
		"mixed":             `[{"jsonrpc":"2.0","id":1,"method":"test/echo","params":{}},{"jsonrpc":"2.0","method":"test/notify","params":{}},17,{"jsonrpc":"2.0","id":99,"result":{}}]`,
		"cancel request":    `[{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":7}}]`,
		"initialize":        `[{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}]`,
		"scalars":           `[1,2,3]`,
	}
	for name, batch := range batches {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var ran []string
			record := func(method string) {
				mu.Lock()
				ran = append(ran, method)
				mu.Unlock()
			}
			_, peer := pipeClient(t, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
				record(method)
				return map[string]bool{"ok": true}, nil
			}, func(method string, _ json.RawMessage) error {
				record(method)
				return nil
			})
			reader := bufio.NewReader(peer)

			if _, err := io.WriteString(peer, batch+"\n"); err != nil {
				t.Fatal(err)
			}
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(line); got != batchRejection {
				t.Fatalf("batch response = %s, want %s", got, batchRejection)
			}

			// The connection keeps serving single messages afterwards, and the
			// only handlers that run are for the messages sent after the batch.
			if _, err := io.WriteString(peer, `{"jsonrpc":"2.0","method":"after/notify","params":{}}`+"\n"+
				`{"jsonrpc":"2.0","id":"after","method":"after/request","params":{}}`+"\n"); err != nil {
				t.Fatal(err)
			}
			line, err = reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(line); got != `{"jsonrpc":"2.0","id":"after","result":{"ok":true}}` {
				t.Fatalf("response after batch = %s", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(ran, ",") != "after/notify,after/request" {
				t.Fatalf("handlers ran %v; batch entries must not run", ran)
			}
		})
	}
}

func TestResponseArrayDoesNotCompletePendingCall(t *testing.T) {
	c, peer := pipeClient(t, nil, nil)
	reader := bufio.NewReader(peer)
	done := make(chan error, 1)
	var result map[string]bool
	go func() { done <- c.Call(context.Background(), "test/call", map[string]any{}, &result) }()

	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(line), &request); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(peer, `[{"jsonrpc":"2.0","id":%s,"result":{"batched":true}}]`+"\n", request.ID); err != nil {
		t.Fatal(err)
	}
	if line, err = reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != batchRejection {
		t.Fatalf("response array answer = %q, %v", line, err)
	}
	select {
	case err := <-done:
		t.Fatalf("a batched response completed the call: %v", err)
	default:
	}
	if _, err := fmt.Fprintf(peer, `{"jsonrpc":"2.0","id":%s,"result":{"single":true}}`+"\n", request.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil || !result["single"] || result["batched"] {
			t.Fatalf("call = %v, %v", result, err)
		}
	case <-time.After(time.Second):
		t.Fatal("single response did not complete the call")
	}
}

// parseErrorLine is the exact line the reference SDK sends for a line that is
// not valid JSON: -32700 with id null and the line, without its terminator, as
// data. quoted is the line as a JSON string.
func parseErrorLine(quoted string) string {
	return `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error","data":{"line":` + quoted + `}}}`
}

func TestInvalidJSONGetsOneParseErrorAndConnectionContinues(t *testing.T) {
	loneSurrogate := `"\` + `ud800"`
	cases := map[string]struct{ wire, quoted string }{
		"not json":           {"not-json\n", `"not-json"`},
		"crlf":               {"not-json\r\n", `"not-json"`},
		"open bracket":       {"[\n", `"["`},
		"truncated array":    {"  [1,\n", `"  [1,"`},
		"truncated object":   {`{"jsonrpc":"2.0",` + "\n", `"{\"jsonrpc\":\"2.0\","`},
		"empty line":         {"\n", `""`},
		"blank line":         {" \t\n", `" \t"`},
		"two values":         {"{} {}\n", `"{} {}"`},
		"lone surrogate":     {`{"jsonrpc":"2.0","method":` + loneSurrogate + "}\n", `"{\"jsonrpc\":\"2.0\",\"method\":\"\\` + `ud800\"}"`},
		"number out of f64":  {`{"jsonrpc":"2.0","id":1e400,"method":"x"}` + "\n", `"{\"jsonrpc\":\"2.0\",\"id\":1e400,\"method\":\"x\"}"`},
		"array out of f64":   {"[1e400]\n", `"[1e400]"`},
		"near max float":     {"[1.7976931348623158e308]\n", `"[1.7976931348623158e308]"`},
		"nested 128 deep":    {strings.Repeat("[", 128) + strings.Repeat("]", 128) + "\n", `"` + strings.Repeat("[", 128) + strings.Repeat("]", 128) + `"`},
		"surrogate in batch": {"[" + loneSurrogate + "]\n", `"[\"\\` + `ud800\"]"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var ran []string
			c, peer := pipeClient(t, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
				mu.Lock()
				ran = append(ran, method)
				mu.Unlock()
				return map[string]bool{"ok": true}, nil
			}, nil)
			reader := bufio.NewReader(peer)
			if _, err := io.WriteString(peer, tc.wire); err != nil {
				t.Fatal(err)
			}
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if got, want := strings.TrimSuffix(line, "\n"), parseErrorLine(tc.quoted); got != want {
				t.Fatalf("response = %s, want %s", got, want)
			}
			if _, err := io.WriteString(peer, `{"jsonrpc":"2.0","id":"after","method":"after/request","params":{}}`+"\n"); err != nil {
				t.Fatal(err)
			}
			if line, err = reader.ReadString('\n'); err != nil || strings.TrimSpace(line) != `{"jsonrpc":"2.0","id":"after","result":{"ok":true}}` {
				t.Fatalf("response after parse error = %q, %v (connection error %v)", line, err, c.Err())
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(ran, ",") != "after/request" {
				t.Fatalf("handlers ran %v", ran)
			}
		})
	}
}

func TestInvalidUTF8StillClosesConnection(t *testing.T) {
	// The reference SDK reads lines as UTF-8 text, so invalid UTF-8 is a
	// transport failure there, not a parse error.
	c, peer := pipeClient(t, nil, nil)
	_, _ = io.WriteString(peer, "\xff\n")
	select {
	case <-c.Done():
	case <-time.After(time.Second):
		t.Fatal("invalid UTF-8 was accepted")
	}
}

func TestMalformedFrameAnswerIsNotOvertaken(t *testing.T) {
	for name, malformed := range map[string]struct{ wire, answer string }{
		"batch":       {"[]\n", batchRejection},
		"parse error": {"[\n", parseErrorLine(`"["`)},
	} {
		t.Run(name, func(t *testing.T) {
			var c *Connection
			notified := make(chan error, 1)
			c, peer := pipeClient(t, func(context.Context, string, json.RawMessage) (any, error) {
				return map[string]bool{"ok": true}, nil
			}, func(method string, _ json.RawMessage) error {
				// A synchronous send from the handler of the frame right after
				// the malformed one.
				go func() { notified <- c.Notify(context.Background(), "handler/sent", map[string]any{}) }()
				return <-notified
			})
			reader := bufio.NewReader(peer)
			if _, err := io.WriteString(peer, malformed.wire+
				`{"jsonrpc":"2.0","method":"trigger","params":{}}`+"\n"+
				`{"jsonrpc":"2.0","id":7,"method":"fast","params":{}}`+"\n"); err != nil {
				t.Fatal(err)
			}
			var lines []string
			for range 3 {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				lines = append(lines, strings.TrimSpace(line))
			}
			want := []string{
				malformed.answer,
				`{"jsonrpc":"2.0","method":"handler/sent","params":{}}`,
				`{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`,
			}
			if strings.Join(lines, "\n") != strings.Join(want, "\n") {
				t.Fatalf("wire order:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

func TestInputClosedPrecedesDoneWhileHandlersRun(t *testing.T) {
	started := make(chan struct{})
	doneEarly := make(chan bool, 1)
	ready := make(chan struct{})
	var c *Connection
	c, peer := pipeClient(t, func(context.Context, string, json.RawMessage) (any, error) {
		<-ready
		close(started)
		<-c.InputClosed()
		select {
		case <-c.Done():
			doneEarly <- true
		default:
			doneEarly <- false
		}
		return nil, nil
	}, nil)
	close(ready)
	_, _ = io.WriteString(peer, `{"jsonrpc":"2.0","id":1,"method":"test/wait"}`+"\n")
	<-started
	_ = peer.Close()
	select {
	case early := <-doneEarly:
		if early {
			t.Fatal("Done closed before in-flight handlers returned")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("InputClosed did not close on EOF")
	}
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done did not close after the handler returned")
	}
}
