package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// invalidRequestLine is the exact line the reference SDK sends for a valid
// JSON value that is not a JSON-RPC message and is not shaped like a response.
const invalidRequestLine = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request"}}`

// nullV2RequestIDLine is the exact line the reference SDK sends for a v2
// request whose id is null.
const nullV2RequestIDLine = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"ACP protocol v2 request ids must be a string or an integer, not null"}}`

// wirePeer drives the far end of a pipeClient connection line by line.
type wirePeer struct {
	t      *testing.T
	writer io.Writer
	reader *bufio.Reader
}

func newWirePeer(t *testing.T, peer io.ReadWriter) *wirePeer {
	return &wirePeer{t: t, writer: peer, reader: bufio.NewReader(peer)}
}

func (p *wirePeer) send(line string) {
	p.t.Helper()
	if _, err := io.WriteString(p.writer, line+"\n"); err != nil {
		p.t.Fatal(err)
	}
}

func (p *wirePeer) recv() string {
	p.t.Helper()
	line, err := p.reader.ReadString('\n')
	if err != nil {
		p.t.Fatal(err)
	}
	return strings.TrimSuffix(line, "\n")
}

// echoHandler answers every request with {"ok":true} and records the methods
// it ran.
type echoHandler struct {
	mu  sync.Mutex
	ran []string
}

func (h *echoHandler) handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	h.record(method)
	return map[string]bool{"ok": true}, nil
}

func (h *echoHandler) notify(method string, _ json.RawMessage) error {
	h.record(method)
	return nil
}

func (h *echoHandler) record(method string) {
	h.mu.Lock()
	h.ran = append(h.ran, method)
	h.mu.Unlock()
}

func (h *echoHandler) methods() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.ran, ",")
}

func TestMalformedCallShapedValuesGetInvalidRequest(t *testing.T) {
	cases := map[string]string{
		"wrong jsonrpc":                 `{"jsonrpc":"1.0","id":1,"method":"x"}`,
		"missing jsonrpc":               `{"id":1,"method":"x","params":{}}`,
		"numeric jsonrpc":               `{"jsonrpc":2.0,"method":"x"}`,
		"empty object":                  `{}`,
		"id only":                       `{"jsonrpc":"2.0","id":1}`,
		"number":                        `17`,
		"string":                        `"hello"`,
		"null":                          `null`,
		"boolean":                       `true`,
		"method is a number":            `{"jsonrpc":"2.0","id":1,"method":5}`,
		"method is null":                `{"jsonrpc":"2.0","id":1,"method":null}`,
		"string params":                 `{"jsonrpc":"2.0","method":"x","params":"s"}`,
		"numeric params":                `{"jsonrpc":"2.0","id":1,"method":"x","params":3}`,
		"fractional id":                 `{"jsonrpc":"2.0","id":1.5,"method":"x"}`,
		"exponent id":                   `{"jsonrpc":"2.0","id":1e3,"method":"x"}`,
		"negative zero id":              `{"jsonrpc":"2.0","id":-0,"method":"x"}`,
		"id above int64":                `{"jsonrpc":"2.0","id":9223372036854775808,"method":"x"}`,
		"id below int64":                `{"jsonrpc":"2.0","id":-9223372036854775809,"method":"x"}`,
		"boolean id":                    `{"jsonrpc":"2.0","id":true,"method":"x"}`,
		"object id":                     `{"jsonrpc":"2.0","id":{},"method":"x"}`,
		"method with result":            `{"jsonrpc":"2.0","id":1,"method":"x","result":{}}`,
		"method with error":             `{"jsonrpc":"2.0","method":"x","error":{"code":1,"message":"m"}}`,
		"member names are exact":        `{"jsonrpc":"2.0","id":1,"Method":"x"}`,
		"cancel request with bad shape": `{"jsonrpc":"2.0","method":"$/cancel_request","params":7}`,
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			handler := &echoHandler{}
			c, conn := pipeClient(t, handler.handle, handler.notify)
			peer := newWirePeer(t, conn)
			peer.send(wire)
			if got := peer.recv(); got != invalidRequestLine {
				t.Fatalf("answer = %s, want %s", got, invalidRequestLine)
			}
			peer.send(`{"jsonrpc":"2.0","id":"after","method":"after/request","params":{}}`)
			if got := peer.recv(); got != `{"jsonrpc":"2.0","id":"after","result":{"ok":true}}` {
				t.Fatalf("response after malformed value = %s (connection error %v)", got, c.Err())
			}
			if got := handler.methods(); got != "after/request" {
				t.Fatalf("handlers ran %s", got)
			}
		})
	}
}

func TestValidMessageVariantsAreAccepted(t *testing.T) {
	// Shapes the reference SDK reads as valid messages, which must run and,
	// for requests, echo the id exactly.
	cases := map[string]struct{ wire, answer string }{
		"null params":       {`{"jsonrpc":"2.0","id":1,"method":"m","params":null}`, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`},
		"array params":      {`{"jsonrpc":"2.0","id":1,"method":"m","params":[1]}`, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`},
		"no params":         {`{"jsonrpc":"2.0","id":1,"method":"m"}`, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`},
		"extra members":     {`{"jsonrpc":"2.0","id":1,"method":"m","extra":true}`, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`},
		"int64 max id":      {`{"jsonrpc":"2.0","id":9223372036854775807,"method":"m"}`, `{"jsonrpc":"2.0","id":9223372036854775807,"result":{"ok":true}}`},
		"int64 min id":      {`{"jsonrpc":"2.0","id":-9223372036854775808,"method":"m"}`, `{"jsonrpc":"2.0","id":-9223372036854775808,"result":{"ok":true}}`},
		"empty string id":   {`{"jsonrpc":"2.0","id":"","method":"m"}`, `{"jsonrpc":"2.0","id":"","result":{"ok":true}}`},
		"empty method name": {`{"jsonrpc":"2.0","id":2,"method":""}`, `{"jsonrpc":"2.0","id":2,"result":{"ok":true}}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			handler := &echoHandler{}
			_, conn := pipeClient(t, handler.handle, handler.notify)
			peer := newWirePeer(t, conn)
			peer.send(tc.wire)
			if got := peer.recv(); got != tc.answer {
				t.Fatalf("answer = %s, want %s", got, tc.answer)
			}
		})
	}
}

func TestMalformedResponsesAreIgnored(t *testing.T) {
	// A malformed value shaped like a response is dropped, as the reference
	// SDK's frame_entry does: no answer, no pending call completed, and the
	// connection keeps serving.
	cases := map[string]string{
		"result and error":       `{"jsonrpc":"2.0","id":%s,"result":{"bad":true},"error":{"code":1,"message":"m"}}`,
		"missing id":             `{"jsonrpc":"2.0","result":{"bad":true}}`,
		"missing jsonrpc":        `{"id":%s,"result":{"bad":true}}`,
		"wrong jsonrpc":          `{"jsonrpc":"1.0","id":%s,"result":{"bad":true}}`,
		"fractional id":          `{"jsonrpc":"2.0","id":1.5,"result":{"bad":true}}`,
		"error missing code":     `{"jsonrpc":"2.0","id":%s,"error":{"message":"m"}}`,
		"error missing message":  `{"jsonrpc":"2.0","id":%s,"error":{"code":1}}`,
		"null error":             `{"jsonrpc":"2.0","id":%s,"error":null}`,
		"null code":              `{"jsonrpc":"2.0","id":%s,"error":{"code":null,"message":"m"}}`,
		"null message":           `{"jsonrpc":"2.0","id":%s,"error":{"code":1,"message":null}}`,
		"string code":            `{"jsonrpc":"2.0","id":%s,"error":{"code":"1","message":"m"}}`,
		"code beyond int32":      `{"jsonrpc":"2.0","id":%s,"error":{"code":2147483648,"message":"m"}}`,
		"error is not an object": `{"jsonrpc":"2.0","id":%s,"error":"bad"}`,
	}
	for name, template := range cases {
		t.Run(name, func(t *testing.T) {
			handler := &echoHandler{}
			c, conn := pipeClient(t, handler.handle, handler.notify)
			peer := newWirePeer(t, conn)
			var result map[string]bool
			done := make(chan error, 1)
			go func() { done <- c.Call(context.Background(), "test/call", map[string]any{}, &result) }()
			var request packet
			if err := json.Unmarshal([]byte(peer.recv()), &request); err != nil {
				t.Fatal(err)
			}
			peer.send(strings.ReplaceAll(template, "%s", string(request.ID)))
			peer.send(`{"jsonrpc":"2.0","id":"after","method":"after/request","params":{}}`)
			if got := peer.recv(); got != `{"jsonrpc":"2.0","id":"after","result":{"ok":true}}` {
				t.Fatalf("next line = %s, want the response to the next request (connection error %v)", got, c.Err())
			}
			select {
			case err := <-done:
				t.Fatalf("a malformed response completed the call: %v %v", result, err)
			default:
			}
			peer.send(`{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":{"good":true}}`)
			select {
			case err := <-done:
				if err != nil || !result["good"] || result["bad"] {
					t.Fatalf("call = %v, %v", result, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("valid response did not complete the call")
			}
		})
	}
}

func TestErrorResponseWithRepeatedMemberKeepsLastValue(t *testing.T) {
	// The reference SDK reads a message as a serde_json value before typing
	// it, so a repeated member of the error object keeps its last value.
	c, conn := pipeClient(t, nil, nil)
	peer := newWirePeer(t, conn)
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), "test/call", map[string]any{}, nil) }()
	var request packet
	if err := json.Unmarshal([]byte(peer.recv()), &request); err != nil {
		t.Fatal(err)
	}
	peer.send(`{"jsonrpc":"2.0","id":` + string(request.ID) + `,"error":{"code":-32002,"message":"missing","data":null,"code":1,"extension":{"retry":true}}}`)
	var rpcErr *RPCError
	if err := <-done; !errors.As(err, &rpcErr) || rpcErr.Code != 1 || rpcErr.Message != "missing" || string(rpcErr.Data) != "null" || string(rpcErr.Extra["extension"]) != `{"retry":true}` {
		t.Fatalf("Call = %#v", err)
	}
}

func TestRepeatedResponseIsIgnored(t *testing.T) {
	c, conn := pipeClient(t, (&echoHandler{}).handle, nil)
	peer := newWirePeer(t, conn)
	for _, want := range []string{"first", "second"} {
		var result map[string]string
		done := make(chan error, 1)
		go func() { done <- c.Call(context.Background(), "test/call", map[string]any{}, &result) }()
		var request packet
		if err := json.Unmarshal([]byte(peer.recv()), &request); err != nil {
			t.Fatal(err)
		}
		// The repeat of the first response arrives before the call reads it.
		peer.send(`{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":{"v":"` + want + `"}}`)
		peer.send(`{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":{"v":"repeat"}}`)
		if err := <-done; err != nil || result["v"] != want {
			t.Fatalf("call = %v, %v", result, err)
		}
	}
	peer.send(`{"jsonrpc":"2.0","id":"after","method":"after/request","params":{}}`)
	if got := peer.recv(); got != `{"jsonrpc":"2.0","id":"after","result":{"ok":true}}` {
		t.Fatalf("response after repeated response = %s (connection error %v)", got, c.Err())
	}
}

func TestNullRequestIDs(t *testing.T) {
	request := `{"jsonrpc":"2.0","id":null,"method":"test/echo","params":{}}`
	after := `{"jsonrpc":"2.0","id":"after","method":"after/request","params":{}}`
	afterAnswer := `{"jsonrpc":"2.0","id":"after","result":{"ok":true}}`
	for name, tc := range map[string]struct {
		options []ConnectOption
		answer  string
		ran     string
	}{
		"version-neutral default accepts": {nil, `{"jsonrpc":"2.0","id":null,"result":{"ok":true}}`, "test/echo,after/request"},
		"v1 accepts":                      {[]ConnectOption{WithProtocolVersion(1)}, `{"jsonrpc":"2.0","id":null,"result":{"ok":true}}`, "test/echo,after/request"},
		"v2 rejects":                      {[]ConnectOption{WithProtocolVersion(2)}, nullV2RequestIDLine, "after/request"},
	} {
		t.Run(name, func(t *testing.T) {
			handler := &echoHandler{}
			c, conn := pipeClient(t, handler.handle, handler.notify, tc.options...)
			peer := newWirePeer(t, conn)
			peer.send(request)
			if got := peer.recv(); got != tc.answer {
				t.Fatalf("answer = %s, want %s", got, tc.answer)
			}
			peer.send(after)
			if got := peer.recv(); got != afterAnswer {
				t.Fatalf("response after null id = %s (connection error %v)", got, c.Err())
			}
			if got := handler.methods(); got != tc.ran {
				t.Fatalf("handlers ran %s, want %s", got, tc.ran)
			}
		})
	}
}

func TestNullRequestIDRejectionKeepsInFlightWork(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	c, conn := pipeClient(t, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "slow" {
			close(started)
			<-release
		}
		return map[string]string{"method": method}, nil
	}, nil, WithProtocolVersion(2))
	peer := newWirePeer(t, conn)
	peer.send(`{"jsonrpc":"2.0","id":1,"method":"slow","params":{}}`)
	<-started
	peer.send(`{"jsonrpc":"2.0","id":null,"method":"fast","params":{}}`)
	if got := peer.recv(); got != nullV2RequestIDLine {
		t.Fatalf("answer = %s", got)
	}
	close(release)
	if got := peer.recv(); got != `{"jsonrpc":"2.0","id":1,"result":{"method":"slow"}}` {
		t.Fatalf("in-flight response = %s (connection error %v)", got, c.Err())
	}
}

// failureLog collects the errors a Connection reports through
// WithErrorHandler.
type failureLog struct {
	mu       sync.Mutex
	failures []error
	reported chan struct{}
}

func newFailureLog() *failureLog { return &failureLog{reported: make(chan struct{}, 16)} }

func (l *failureLog) report(err error) {
	l.mu.Lock()
	l.failures = append(l.failures, err)
	l.mu.Unlock()
	l.reported <- struct{}{}
}

func (l *failureLog) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-l.reported:
	case <-time.After(2 * time.Second):
		t.Fatal("no failure was reported")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failures[len(l.failures)-1]
}

func TestRequestHandlerPanicIsAnswered(t *testing.T) {
	failures := newFailureLog()
	release := make(chan struct{})
	started := make(chan struct{})
	c, conn := pipeClient(t, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		switch method {
		case "slow":
			close(started)
			<-release
			return map[string]bool{"slow": true}, nil
		case "test/panic":
			panic("boom")
		}
		return map[string]bool{"ok": true}, nil
	}, nil, WithErrorHandler(failures.report))
	peer := newWirePeer(t, conn)
	peer.send(`{"jsonrpc":"2.0","id":1,"method":"slow","params":{}}`)
	<-started
	peer.send(`{"jsonrpc":"2.0","id":2,"method":"test/panic","params":{}}`)
	want := "{\"jsonrpc\":\"2.0\",\"id\":2,\"error\":{\"code\":-32603,\"message\":\"Internal error\",\"data\":\"request handler dropped its responder for `test/panic` without responding\"}}"
	if got := peer.recv(); got != want {
		t.Fatalf("panic answer = %s, want %s", got, want)
	}
	var failure *HandlerPanic
	if err := failures.wait(t); !errors.As(err, &failure) || failure.Method != "test/panic" || failure.Value != "boom" || !bytes.Contains(failure.Stack, []byte("connection_resilience_test.go")) {
		t.Fatalf("reported %#v", err)
	}
	close(release)
	if got := peer.recv(); got != `{"jsonrpc":"2.0","id":1,"result":{"slow":true}}` {
		t.Fatalf("in-flight response = %s (connection error %v)", got, c.Err())
	}
	peer.send(`{"jsonrpc":"2.0","id":3,"method":"after","params":{}}`)
	if got := peer.recv(); got != `{"jsonrpc":"2.0","id":3,"result":{"ok":true}}` {
		t.Fatalf("response after panic = %s", got)
	}
}

func TestCancelledRequestHandlerPanicIsAnsweredAsCancelled(t *testing.T) {
	failures := newFailureLog()
	started := make(chan struct{})
	_, conn := pipeClient(t, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		panic("cleanup failed")
	}, nil, WithErrorHandler(failures.report))
	peer := newWirePeer(t, conn)
	peer.send(`{"jsonrpc":"2.0","id":"p","method":"test/panic","params":{}}`)
	<-started
	peer.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"p"}}`)
	if got := peer.recv(); got != `{"jsonrpc":"2.0","id":"p","error":{"code":-32800,"message":"Request cancelled"}}` {
		t.Fatalf("answer = %s", got)
	}
	failures.wait(t)
}

func TestPanicAfterInputClosedIsNotAnswered(t *testing.T) {
	// The reference SDK sends no automatic error for a dropped responder while
	// the connection is shutting down, the rule rpcctl.ErrNoResponse follows.
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	failures := newFailureLog()
	var c *Connection
	started := make(chan struct{})
	c = Connect(inReader, outWriter, func(context.Context, string, json.RawMessage) (any, error) {
		close(started)
		<-c.InputClosed()
		panic("after eof")
	}, nil, WithErrorHandler(failures.report))
	t.Cleanup(func() { _ = c.Close() })
	output := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(outReader)
		output <- data
	}()
	if _, err := io.WriteString(inWriter, `{"jsonrpc":"2.0","id":1,"method":"test/panic","params":{}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	<-started
	_ = inWriter.Close()
	var failure *HandlerPanic
	if err := failures.wait(t); !errors.As(err, &failure) || failure.Value != "after eof" {
		t.Fatalf("reported %#v", err)
	}
	select {
	case data := <-output:
		if len(data) != 0 {
			t.Fatalf("answered after input closed: %s", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not finish after EOF")
	}
}

func TestPanicWithoutErrorHandlerIsLogged(t *testing.T) {
	var logged bytes.Buffer
	var mu sync.Mutex
	previous := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logged.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(previous) })
	_, conn := pipeClient(t, func(context.Context, string, json.RawMessage) (any, error) {
		panic("logged boom")
	}, nil)
	peer := newWirePeer(t, conn)
	peer.send(`{"jsonrpc":"2.0","id":1,"method":"test/panic","params":{}}`)
	if got := peer.recv(); !strings.Contains(got, `"code":-32603`) || strings.Contains(got, "logged boom") {
		t.Fatalf("answer = %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logged.String(), "test/panic handler panicked: logged boom") || !strings.Contains(logged.String(), "goroutine") {
		t.Fatalf("log = %q", logged.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestNotificationFailuresKeepConnection(t *testing.T) {
	failures := newFailureLog()
	release := make(chan struct{})
	started := make(chan struct{})
	notifyErr := errors.New("bad cancel")
	c, conn := pipeClient(t, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "slow" {
			close(started)
			<-release
		}
		return map[string]string{"method": method}, nil
	}, func(method string, _ json.RawMessage) error {
		switch method {
		case "test/error":
			return notifyErr
		case "test/panic":
			panic("notification boom")
		}
		return nil
	}, WithErrorHandler(failures.report))
	peer := newWirePeer(t, conn)
	peer.send(`{"jsonrpc":"2.0","id":1,"method":"slow","params":{}}`)
	<-started

	peer.send(`{"jsonrpc":"2.0","method":"test/error","params":{}}`)
	if err := failures.wait(t); !errors.Is(err, notifyErr) || !strings.Contains(err.Error(), "test/error") {
		t.Fatalf("reported %v", err)
	}
	peer.send(`{"jsonrpc":"2.0","method":"test/panic","params":{}}`)
	var failure *HandlerPanic
	if err := failures.wait(t); !errors.As(err, &failure) || failure.Method != "test/panic" || failure.Value != "notification boom" {
		t.Fatalf("reported %#v", err)
	}

	close(release)
	if got := peer.recv(); got != `{"jsonrpc":"2.0","id":1,"result":{"method":"slow"}}` {
		t.Fatalf("in-flight response = %s (connection error %v)", got, c.Err())
	}
	peer.send(`{"jsonrpc":"2.0","id":2,"method":"after","params":{}}`)
	if got := peer.recv(); got != `{"jsonrpc":"2.0","id":2,"result":{"method":"after"}}` {
		t.Fatalf("response after notification failures = %s", got)
	}
}

func TestPanickingErrorHandlerIsContained(t *testing.T) {
	var logged bytes.Buffer
	var mu sync.Mutex
	previous := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logged.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(previous) })
	var calls sync.WaitGroup
	calls.Add(3)
	c, conn := pipeClient(t, func(context.Context, string, json.RawMessage) (any, error) {
		panic("request boom")
	}, func(method string, _ json.RawMessage) error {
		if method == "test/panic" {
			panic("notification boom")
		}
		return errors.New("notification failed")
	}, WithErrorHandler(func(error) {
		calls.Done()
		panic("hook boom")
	}))
	peer := newWirePeer(t, conn)
	peer.send(`{"jsonrpc":"2.0","method":"test/error","params":{}}`)
	peer.send(`{"jsonrpc":"2.0","method":"test/panic","params":{}}`)
	peer.send(`{"jsonrpc":"2.0","id":1,"method":"test/request","params":{}}`)
	if got := peer.recv(); !strings.Contains(got, `"code":-32603`) {
		t.Fatalf("answer = %s (connection error %v)", got, c.Err())
	}
	// Each failure reaches the hook once: its panic is not reported again.
	calls.Wait()
	// The hook's panic is logged just after the hook is entered.
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got, text := strings.Count(logged.String(), "error handler panicked"), logged.String()
		mu.Unlock()
		if got == 3 {
			break
		}
		if got > 3 || time.Now().After(deadline) {
			t.Fatalf("logged %d hook panics:\n%s", got, text)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRequestObserverOption(t *testing.T) {
	failures := newFailureLog()
	var mu sync.Mutex
	var observed []string
	_, conn := pipeClient(t, (&echoHandler{}).handle, nil, WithProtocolVersion(2), WithErrorHandler(failures.report),
		WithRequestObserver(func(method string, _ json.RawMessage) {
			mu.Lock()
			observed = append(observed, method)
			mu.Unlock()
			if method == "test/panic" {
				panic("observer boom")
			}
		}))
	peer := newWirePeer(t, conn)
	peer.send(`{"jsonrpc":"2.0","id":null,"method":"test/null","params":{}}`)
	if got := peer.recv(); got != nullV2RequestIDLine {
		t.Fatalf("answer = %s", got)
	}
	peer.send(`{"jsonrpc":"2.0","id":1,"method":"test/panic","params":{}}`)
	// The request still runs after its observer panicked.
	if got := peer.recv(); got != `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` {
		t.Fatalf("answer = %s", got)
	}
	var failure *HandlerPanic
	if err := failures.wait(t); !errors.As(err, &failure) || failure.Method != "test/panic" || failure.Value != "observer boom" {
		t.Fatalf("reported %#v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(observed, ",") != "test/panic" {
		t.Fatalf("observed %v; a request rejected before dispatch is not observed", observed)
	}
}

func TestCancelledRequestIsAnsweredInReferenceShape(t *testing.T) {
	started := make(chan struct{})
	_, conn := pipeClient(t, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}, nil)
	peer := newWirePeer(t, conn)
	peer.send(`{"jsonrpc":"2.0","id":"c","method":"test/slow","params":{}}`)
	<-started
	peer.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"c"}}`)
	if got := peer.recv(); got != `{"jsonrpc":"2.0","id":"c","error":{"code":-32800,"message":"Request cancelled"}}` {
		t.Fatalf("answer = %s", got)
	}
}

func TestNotificationErrorWithoutHandlerIsDropped(t *testing.T) {
	c, conn := pipeClient(t, (&echoHandler{}).handle, func(string, json.RawMessage) error {
		return errors.New("ignored")
	})
	peer := newWirePeer(t, conn)
	peer.send(`{"jsonrpc":"2.0","method":"test/error","params":{}}`)
	peer.send(`{"jsonrpc":"2.0","id":1,"method":"after","params":{}}`)
	if got := peer.recv(); got != `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}` {
		t.Fatalf("response = %s (connection error %v)", got, c.Err())
	}
}

func TestMalformedValueAnswerIsNotOvertaken(t *testing.T) {
	var c *Connection
	notified := make(chan error, 1)
	c, conn := pipeClient(t, func(context.Context, string, json.RawMessage) (any, error) {
		return map[string]bool{"ok": true}, nil
	}, func(string, json.RawMessage) error {
		go func() { notified <- c.Notify(context.Background(), "handler/sent", map[string]any{}) }()
		return <-notified
	}, WithProtocolVersion(2))
	peer := newWirePeer(t, conn)
	// One write, so the connection holds every frame before it answers any.
	peer.send(`{"jsonrpc":"2.0","id":1}` + "\n" +
		`{"jsonrpc":"2.0","id":null,"method":"x","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","method":"trigger","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":7,"method":"fast","params":{}}`)
	want := []string{
		invalidRequestLine,
		nullV2RequestIDLine,
		`{"jsonrpc":"2.0","method":"handler/sent","params":{}}`,
		`{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`,
	}
	for _, line := range want {
		if got := peer.recv(); got != line {
			t.Fatalf("line = %s, want %s", got, line)
		}
	}
}
