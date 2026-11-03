package acphttp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/transport/http"
)

// scriptedAgent is one connection of a scriptedEntry: the test reads what the
// client POSTed and writes whatever frames it wants the agent to send.
type scriptedAgent struct {
	received chan string
	ctx      context.Context

	mu  sync.Mutex
	out io.Writer
}

func (a *scriptedAgent) send(t *testing.T, frame string) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := io.WriteString(a.out, frame+"\n"); err != nil {
		t.Fatalf("agent send: %v", err)
	}
}

// next returns the next line the agent received.
func (a *scriptedAgent) next(t *testing.T) string {
	t.Helper()
	select {
	case line := <-a.received:
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("agent received nothing")
		return ""
	}
}

// scriptedEntry is an Entry whose agents answer initialize through
// onInitialize (by default with a result) and hand every other line to the
// test.
type scriptedEntry struct {
	onInitialize func(agent *scriptedAgent, id json.RawMessage)
	agents       chan *scriptedAgent
}

func newScriptedEntry() *scriptedEntry {
	return &scriptedEntry{agents: make(chan *scriptedAgent, 4)}
}

func (e *scriptedEntry) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	agent := &scriptedAgent{received: make(chan string, 64), ctx: ctx, out: out}
	e.agents <- agent
	scanner := bufio.NewScanner(in)
	for scanner.Scan() {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(scanner.Bytes(), &message)
		if message.Method == "initialize" {
			if e.onInitialize != nil {
				e.onInitialize(agent, message.ID)
			} else {
				_, _ = fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1}}`+"\n", message.ID)
			}
			continue
		}
		agent.received <- scanner.Text()
	}
	<-ctx.Done()
	return ctx.Err()
}

func (e *scriptedEntry) agent(t *testing.T) *scriptedAgent {
	t.Helper()
	select {
	case agent := <-e.agents:
		return agent
	case <-time.After(5 * time.Second):
		t.Fatal("no agent connection started")
		return nil
	}
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`

type testServer struct {
	*httptest.Server
	acp *acphttp.Server
}

func startServer(t *testing.T, entry acphttp.Entry, options acphttp.ServerOptions) testServer {
	t.Helper()
	acpServer, err := acphttp.NewServer(entry, options)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(acpServer)
	t.Cleanup(func() {
		_ = acpServer.Close()
		server.Close()
	})
	return testServer{Server: server, acp: acpServer}
}

func (s testServer) do(t *testing.T, method, body string, headers map[string]string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, s.URL+acphttp.DefaultPath, reader)
	if err != nil {
		t.Fatal(err)
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := s.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func (s testServer) post(t *testing.T, connectionID, sessionID, body string) *http.Response {
	t.Helper()
	headers := map[string]string{acphttp.HeaderConnectionID: connectionID}
	if sessionID != "" {
		headers[acphttp.HeaderSessionID] = sessionID
	}
	return s.do(t, http.MethodPost, body, headers)
}

func (s testServer) initialize(t *testing.T) string {
	t.Helper()
	response := s.do(t, http.MethodPost, initializeBody, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d", response.StatusCode)
	}
	connectionID := response.Header.Get(acphttp.HeaderConnectionID)
	if connectionID == "" {
		t.Fatal("initialize response is missing the connection id")
	}
	return connectionID
}

// sseStream is an open event stream; close cancels its request.
type sseStream struct {
	response *http.Response
	reader   *bufio.Reader
	close    context.CancelFunc
}

func (s testServer) stream(t *testing.T, connectionID, sessionID string) (*http.Response, *sseStream) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+acphttp.DefaultPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set(acphttp.HeaderConnectionID, connectionID)
	if sessionID != "" {
		request.Header.Set(acphttp.HeaderSessionID, sessionID)
	}
	response, err := s.Client().Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stream := &sseStream{response: response, reader: bufio.NewReader(response.Body), close: func() {
		cancel()
		_ = response.Body.Close()
	}}
	t.Cleanup(stream.close)
	return response, stream
}

func (s testServer) openStream(t *testing.T, connectionID, sessionID string) *sseStream {
	t.Helper()
	response, stream := s.stream(t, connectionID, sessionID)
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("stream %q status = %d: %s", sessionID, response.StatusCode, body)
	}
	return stream
}

// expect reads the next events and compares them with want.
func (s *sseStream) expect(t *testing.T, want ...string) {
	t.Helper()
	for _, frame := range want {
		if got := readEvent(t, s.reader); got != frame {
			t.Fatalf("event = %s, want %s", got, frame)
		}
	}
}

func bodyText(t *testing.T, response *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func expectStatus(t *testing.T, response *http.Response, status int, body string) {
	t.Helper()
	if got := bodyText(t, response); response.StatusCode != status || got != body {
		t.Fatalf("response = %d %q, want %d %q", response.StatusCode, got, status, body)
	}
}

// newSession has the agent answer session/new with sessionID on the
// connection stream.
func newSession(t *testing.T, server testServer, agent *scriptedAgent, connection *sseStream, connectionID string, id int, sessionID string) {
	t.Helper()
	expectStatus(t, server.post(t, connectionID, "", fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"session/new","params":{"cwd":"/","mcpServers":[]}}`, id)), http.StatusAccepted, "")
	agent.next(t)
	result := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"sessionId":%q}}`, id, sessionID)
	agent.send(t, result)
	connection.expect(t, result)
}

// TestServerRoutesFramesToSessionStreams covers per-session routing: each
// session's notifications, callbacks, and responses reach only its stream,
// and connection-scoped frames reach the connection stream.
func TestServerRoutesFramesToSessionStreams(t *testing.T) {
	entry := newScriptedEntry()
	server := startServer(t, entry, acphttp.ServerOptions{})
	connectionID := server.initialize(t)
	agent := entry.agent(t)
	connection := server.openStream(t, connectionID, "")

	newSession(t, server, agent, connection, connectionID, 2, "a")
	newSession(t, server, agent, connection, connectionID, 3, "b")
	response, a := server.stream(t, connectionID, "a")
	if response.StatusCode != http.StatusOK || response.Header.Get(acphttp.HeaderSessionID) != "a" ||
		response.Header.Get(acphttp.HeaderConnectionID) != connectionID ||
		response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("session stream response = %d %v", response.StatusCode, response.Header)
	}
	b := server.openStream(t, connectionID, "b")

	// The session header becomes params.sessionId for the agent.
	expectStatus(t, server.post(t, connectionID, "a", `{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"prompt":[]}}`), http.StatusAccepted, "")
	if got := agent.next(t); !strings.Contains(got, `"sessionId":"a"`) || !strings.Contains(got, `"method":"session/prompt"`) {
		t.Fatalf("agent received %s", got)
	}
	expectStatus(t, server.post(t, connectionID, "", `{"jsonrpc":"2.0","id":5,"method":"session/prompt","params":{"sessionId":"b","prompt":[]}}`), http.StatusAccepted, "")
	agent.next(t)

	frames := []string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"a","update":{"n":1}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"b","update":{"n":1}}}`,
		`{"jsonrpc":"2.0","id":99,"method":"session/request_permission","params":{"sessionId":"b"}}`,
		`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":7,"sessionId":"a"}}`,
		`{"jsonrpc":"2.0","method":"proxy/successor","params":{"method":"$/cancel_request","params":{"requestId":8,"sessionId":"a"}}}`,
		`{"jsonrpc":"2.0","method":"_test/connection","params":{}}`,
		`{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn"}}`,
		`{"jsonrpc":"2.0","id":5,"result":{"stopReason":"end_turn"}}`,
		`{"jsonrpc":"2.0","method":"_test/marker","params":{"sessionId":"a"}}`,
		`{"jsonrpc":"2.0","method":"_test/marker","params":{"sessionId":"b"}}`,
		`{"jsonrpc":"2.0","method":"_test/marker","params":{}}`,
	}
	for _, frame := range frames {
		agent.send(t, frame)
	}
	a.expect(t, frames[0], frames[6], frames[8])
	b.expect(t, frames[1], frames[2], frames[7], frames[9])
	connection.expect(t, frames[3], frames[4], frames[5], frames[10])
}

// TestServerBuffersFramesUntilStreamOpens covers frames the agent sends
// before initialize answers and before any stream is open.
func TestServerBuffersFramesUntilStreamOpens(t *testing.T) {
	early := `{"jsonrpc":"2.0","method":"_test/before_initialize","params":{}}`
	entry := newScriptedEntry()
	entry.onInitialize = func(agent *scriptedAgent, id json.RawMessage) {
		_, _ = io.WriteString(agent.out, early+"\n")
		_, _ = fmt.Fprintf(agent.out, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":1}}`+"\n", id)
	}
	server := startServer(t, entry, acphttp.ServerOptions{})
	response := server.do(t, http.MethodPost, initializeBody, nil)
	if got := bodyText(t, response); response.StatusCode != http.StatusOK || got != `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}` {
		t.Fatalf("initialize = %d %s", response.StatusCode, got)
	}
	connectionID := response.Header.Get(acphttp.HeaderConnectionID)
	agent := entry.agent(t)
	afterInitialize := `{"jsonrpc":"2.0","method":"_test/after_initialize","params":{}}`
	agent.send(t, afterInitialize)
	session := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"late"}}`
	agent.send(t, session)

	server.openStream(t, connectionID, "").expect(t, early, afterInitialize)
	server.openStream(t, connectionID, "late").expect(t, session)
}

// TestServerSubscriberRules covers 409 for a second subscriber and that a
// returning subscriber gets what the first one left unread.
func TestServerSubscriberRules(t *testing.T) {
	entry := newScriptedEntry()
	server := startServer(t, entry, acphttp.ServerOptions{})
	connectionID := server.initialize(t)
	agent := entry.agent(t)
	first := server.openStream(t, connectionID, "")
	response, _ := server.stream(t, connectionID, "")
	expectStatus(t, response, http.StatusConflict, "outbound stream already has a subscriber")

	server.openStream(t, connectionID, "s")
	response, _ = server.stream(t, connectionID, "s")
	expectStatus(t, response, http.StatusConflict, "outbound stream already has a subscriber")

	read := `{"jsonrpc":"2.0","method":"_test/one","params":{}}`
	agent.send(t, read)
	first.expect(t, read)
	first.close()
	// The server lets go of the stream once it sees the client leave. The
	// frame is sent only once the next subscriber holds the stream: one sent
	// while the departing subscriber is still being served may be written to
	// its dead connection and lost, as in the Rust crate.
	queued := `{"jsonrpc":"2.0","method":"_test/two","params":{}}`
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, stream := server.stream(t, connectionID, "")
		if response.StatusCode == http.StatusOK {
			agent.send(t, queued)
			stream.expect(t, queued)
			break
		}
		stream.close()
		if time.Now().After(deadline) {
			t.Fatalf("connection stream still busy: %d", response.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServerCapsUnconfirmedSessionStreams covers the 16 streams a client may
// open for sessions the agent has not named, and 404 beyond them.
func TestServerCapsUnconfirmedSessionStreams(t *testing.T) {
	entry := newScriptedEntry()
	server := startServer(t, entry, acphttp.ServerOptions{})
	connectionID := server.initialize(t)
	agent := entry.agent(t)

	streams := make([]*sseStream, 0, 16)
	for i := range 16 {
		streams = append(streams, server.openStream(t, connectionID, fmt.Sprintf("made-up-%d", i)))
	}
	response, _ := server.stream(t, connectionID, "one-too-many")
	expectStatus(t, response, http.StatusNotFound, "unknown Acp-Session-Id")

	// A session the agent names does not count against the cap.
	agent.send(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"named"}}`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, stream := server.stream(t, connectionID, "named")
		if response.StatusCode == http.StatusOK {
			stream.close()
			break
		}
		stream.close()
		if time.Now().After(deadline) {
			t.Fatalf("named session stream status = %d", response.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Closing an unconfirmed stream frees its slot.
	streams[0].close()
	for {
		response, stream := server.stream(t, connectionID, "replacement")
		if response.StatusCode == http.StatusOK {
			break
		}
		stream.close()
		if time.Now().After(deadline) {
			t.Fatalf("replacement stream status = %d", response.StatusCode)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestServerSessionRequestDoesNotCreateStream covers a response for a session
// without a stream, which goes to the connection stream.
func TestServerSessionRequestDoesNotCreateStream(t *testing.T) {
	entry := newScriptedEntry()
	server := startServer(t, entry, acphttp.ServerOptions{})
	connectionID := server.initialize(t)
	agent := entry.agent(t)
	connection := server.openStream(t, connectionID, "")

	expectStatus(t, server.post(t, connectionID, "ghost", `{"jsonrpc":"2.0","id":7,"method":"session/prompt","params":{"prompt":[]}}`), http.StatusAccepted, "")
	agent.next(t)
	failure := `{"jsonrpc":"2.0","id":7,"error":{"code":-32602,"message":"Invalid params"}}`
	agent.send(t, failure)
	connection.expect(t, failure)
}

// TestServerSessionCloseRemovesStream covers session/close: a subscribed
// session stream still gets the response, and the session is then forgotten.
func TestServerSessionCloseRemovesStream(t *testing.T) {
	entry := newScriptedEntry()
	server := startServer(t, entry, acphttp.ServerOptions{})
	connectionID := server.initialize(t)
	agent := entry.agent(t)
	connection := server.openStream(t, connectionID, "")
	newSession(t, server, agent, connection, connectionID, 2, "watched")
	newSession(t, server, agent, connection, connectionID, 3, "unwatched")
	watched := server.openStream(t, connectionID, "watched")

	expectStatus(t, server.post(t, connectionID, "watched", `{"jsonrpc":"2.0","id":4,"method":"session/close","params":{}}`), http.StatusAccepted, "")
	agent.next(t)
	expectStatus(t, server.post(t, connectionID, "unwatched", `{"jsonrpc":"2.0","id":5,"method":"session/delete","params":{}}`), http.StatusAccepted, "")
	agent.next(t)
	agent.send(t, `{"jsonrpc":"2.0","id":4,"result":{}}`)
	agent.send(t, `{"jsonrpc":"2.0","id":5,"result":{}}`)
	watched.expect(t, `{"jsonrpc":"2.0","id":4,"result":{}}`)
	connection.expect(t, `{"jsonrpc":"2.0","id":5,"result":{}}`)

	// The removed sessions are unknown again: with every unconfirmed slot
	// taken, neither stream opens.
	for i := range 16 {
		server.openStream(t, connectionID, fmt.Sprintf("made-up-%d", i))
	}
	response, _ := server.stream(t, connectionID, "unwatched")
	expectStatus(t, response, http.StatusNotFound, "unknown Acp-Session-Id")
}

func TestServerSessionHeaderRules(t *testing.T) {
	entry := newScriptedEntry()
	server := startServer(t, entry, acphttp.ServerOptions{})
	connectionID := server.initialize(t)
	agent := entry.agent(t)

	for body, want := range map[string]string{
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"other"}}`: "Acp-Session-Id header does not match params.sessionId",
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":7}}`:       "params.sessionId must be a string",
		`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":["s"]}`:                 "Acp-Session-Id header requires object params",
	} {
		expectStatus(t, server.post(t, connectionID, "s", body), http.StatusBadRequest, want)
	}
	expectStatus(t, server.post(t, connectionID, "", `{"jsonrpc":"2.0","method":"session/cancel"}`), http.StatusBadRequest, "Acp-Session-Id header required")

	// A message without params gains them, and a matching sessionId passes.
	expectStatus(t, server.post(t, connectionID, "s", `{"jsonrpc":"2.0","method":"session/cancel"}`), http.StatusAccepted, "")
	if got := agent.next(t); got != `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}` {
		t.Fatalf("agent received %s", got)
	}
	matching := `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`
	expectStatus(t, server.post(t, connectionID, "s", matching), http.StatusAccepted, "")
	if got := agent.next(t); got != matching {
		t.Fatalf("agent received %s", got)
	}
	// $/cancel_request is connection-scoped and never takes the header.
	cancel := `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`
	expectStatus(t, server.post(t, connectionID, "s", cancel), http.StatusAccepted, "")
	if got := agent.next(t); got != cancel {
		t.Fatalf("agent received %s", got)
	}
	// A response carries no session.
	reply := `{"jsonrpc":"2.0","id":99,"result":{}}`
	expectStatus(t, server.post(t, connectionID, "s", reply), http.StatusAccepted, "")
	if got := agent.next(t); got != reply {
		t.Fatalf("agent received %s", got)
	}
}

// TestServerDeleteEndsConnection covers DELETE: 400 without a connection id,
// 404 for an unknown one, and 202 that stops the agent and ends the streams
// after what is queued.
func TestServerDeleteEndsConnection(t *testing.T) {
	entry := newScriptedEntry()
	server := startServer(t, entry, acphttp.ServerOptions{})
	connectionID := server.initialize(t)
	agent := entry.agent(t)
	stream := server.openStream(t, connectionID, "")

	expectStatus(t, server.do(t, http.MethodDelete, "", nil), http.StatusBadRequest, "Acp-Connection-Id header required")
	expectStatus(t, server.do(t, http.MethodDelete, "", map[string]string{acphttp.HeaderConnectionID: "unknown"}), http.StatusNotFound, "")

	last := `{"jsonrpc":"2.0","method":"_test/last","params":{}}`
	agent.send(t, last)
	stream.expect(t, last)
	expectStatus(t, server.do(t, http.MethodDelete, "", map[string]string{acphttp.HeaderConnectionID: connectionID}), http.StatusAccepted, "")
	if rest, err := io.ReadAll(stream.reader); err != nil || strings.Contains(string(rest), "data:") {
		t.Fatalf("stream after DELETE = %q, %v", rest, err)
	}
	select {
	case <-agent.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("DELETE did not stop the agent")
	}
	expectStatus(t, server.post(t, connectionID, "", `{"jsonrpc":"2.0","method":"_test/after","params":{}}`), http.StatusNotFound, "")
	expectStatus(t, server.do(t, http.MethodDelete, "", map[string]string{acphttp.HeaderConnectionID: connectionID}), http.StatusNotFound, "")
}

// TestServerInitializeOutcomes covers a rejected initialize, which opens no
// connection, and an agent that exits before it answers.
func TestServerInitializeOutcomes(t *testing.T) {
	rejected := newScriptedEntry()
	rejected.onInitialize = func(agent *scriptedAgent, id json.RawMessage) {
		_, _ = fmt.Fprintf(agent.out, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32600,"message":"Invalid request"}}`+"\n", id)
	}
	server := startServer(t, rejected, acphttp.ServerOptions{})
	response := server.do(t, http.MethodPost, initializeBody, nil)
	if got := bodyText(t, response); response.StatusCode != http.StatusOK || response.Header.Get(acphttp.HeaderConnectionID) != "" ||
		got != `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"Invalid request"}}` {
		t.Fatalf("rejected initialize = %d %v %s", response.StatusCode, response.Header, got)
	}
	select {
	case <-rejected.agent(t).ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a rejected initialize kept its agent running")
	}

	server = startServer(t, exitingEntry{}, acphttp.ServerOptions{})
	expectStatus(t, server.do(t, http.MethodPost, initializeBody, nil), http.StatusInternalServerError, "agent closed before initialize response")

	// An initialize request always opens a new connection.
	entry := newScriptedEntry()
	server = startServer(t, entry, acphttp.ServerOptions{})
	first := server.initialize(t)
	response = server.do(t, http.MethodPost, initializeBody, map[string]string{acphttp.HeaderConnectionID: first})
	if second := response.Header.Get(acphttp.HeaderConnectionID); response.StatusCode != http.StatusOK || second == "" || second == first {
		t.Fatalf("initialize on a live connection = %d %q", response.StatusCode, second)
	}
}

type exitingEntry struct{}

func (exitingEntry) Serve(context.Context, io.ReadCloser, io.WriteCloser) error { return nil }

// deafEntry answers initialize, then closes its input and keeps running.
type deafEntry struct{ closed chan struct{} }

func (e deafEntry) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	if _, err := bufio.NewReader(in).ReadBytes('\n'); err != nil {
		return err
	}
	_, _ = io.WriteString(out, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`+"\n")
	_ = in.Close()
	close(e.closed)
	<-ctx.Done()
	return ctx.Err()
}

// Once the agent stops reading, a POST gets an empty 500, as the Rust
// crate's send to a dropped agent channel fails.
func TestServerRefusesFramesOnceAgentStopsReading(t *testing.T) {
	entry := deafEntry{closed: make(chan struct{})}
	server := startServer(t, entry, acphttp.ServerOptions{})
	connectionID := server.initialize(t)
	<-entry.closed
	for _, body := range []string{`{"jsonrpc":"2.0","method":"_test/x"}`, `{"jsonrpc":"2.0","id":2,"method":"_test/y"}`, `not json`} {
		expectStatus(t, server.post(t, connectionID, "", body), http.StatusInternalServerError, "")
	}
}

// A body that is not UTF-8 gets the text of Rust's Utf8Error.
func TestServerRejectsInvalidUTF8LikeRust(t *testing.T) {
	server := startServer(t, newScriptedEntry(), acphttp.ServerOptions{})
	for body, want := range map[string]string{
		"\xff":                      "Invalid JSON-RPC: invalid utf-8 sequence of 1 bytes from index 0",
		"{\"a\":\"\xe0\xa0A\"}":     "Invalid JSON-RPC: invalid utf-8 sequence of 2 bytes from index 6",
		"{\"a\":\"\xe2\x82\xac\xe2": "Invalid JSON-RPC: incomplete utf-8 byte sequence from index 9",
	} {
		expectStatus(t, server.do(t, http.MethodPost, body, nil), http.StatusBadRequest, want)
	}
}

func TestServerStatusCodes(t *testing.T) {
	server := startServer(t, newScriptedEntry(), acphttp.ServerOptions{})
	connectionID := server.initialize(t)

	expectStatus(t, server.do(t, http.MethodPost, `{"jsonrpc":"2.0","method":"_test/x"}`, nil), http.StatusBadRequest, "Acp-Connection-Id header required")
	expectStatus(t, server.do(t, http.MethodPost, `{"jsonrpc":"2.0","method":"_test/x"}`, map[string]string{"Content-Type": "text/plain"}), http.StatusUnsupportedMediaType, "Content-Type must be application/json")
	expectStatus(t, server.post(t, "unknown", "", `{"jsonrpc":"2.0","method":"_test/x"}`), http.StatusNotFound, "")
	expectStatus(t, server.do(t, http.MethodGet, "", map[string]string{acphttp.HeaderConnectionID: connectionID}), http.StatusNotAcceptable, "client must accept text/event-stream")
	expectStatus(t, server.do(t, http.MethodGet, "", map[string]string{"Accept": "text/event-stream"}), http.StatusBadRequest, "Acp-Connection-Id header required")
	expectStatus(t, server.do(t, http.MethodGet, "", map[string]string{"Accept": "text/event-stream", acphttp.HeaderConnectionID: "unknown"}), http.StatusNotFound, "")
	response := server.do(t, http.MethodPut, "", nil)
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != "POST,GET,HEAD,DELETE" {
		t.Fatalf("PUT = %d %v", response.StatusCode, response.Header)
	}
	request, err := http.NewRequest(http.MethodGet, server.URL+"/elsewhere", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	expectStatus(t, response, http.StatusNotFound, "")
}

func TestServerBodyLimit(t *testing.T) {
	if acphttp.DefaultMaxBodyBytes != 16<<20 {
		t.Fatalf("DefaultMaxBodyBytes = %d", acphttp.DefaultMaxBodyBytes)
	}
	acpServer, err := acphttp.NewServer(newScriptedEntry(), acphttp.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer acpServer.Close()
	// A declared Content-Length over the limit is refused before the body is
	// read.
	request := httptest.NewRequest(http.MethodPost, acphttp.DefaultPath, strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.ContentLength = acphttp.DefaultMaxBodyBytes + 1
	recorder := httptest.NewRecorder()
	acpServer.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge || recorder.Body.String() != "POST body too large" {
		t.Fatalf("declared oversize body = %d %q", recorder.Code, recorder.Body.String())
	}

	server := startServer(t, newScriptedEntry(), acphttp.ServerOptions{MaxBodyBytes: int64(len(initializeBody))})
	server.initialize(t)
	// A streamed body that turns out too large is refused too.
	request, err = http.NewRequest(http.MethodPost, server.URL+acphttp.DefaultPath, io.MultiReader(strings.NewReader(initializeBody), strings.NewReader(" ")))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.ContentLength = -1
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	expectStatus(t, response, http.StatusRequestEntityTooLarge, "POST body too large")
}

func TestServerHealthEndpoint(t *testing.T) {
	server := startServer(t, newScriptedEntry(), acphttp.ServerOptions{})
	get := func(t *testing.T, server testServer, method, path string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(method, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}
	response := get(t, server, http.MethodGet, "/health")
	if response.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("health Content-Type = %q", response.Header.Get("Content-Type"))
	}
	expectStatus(t, response, http.StatusOK, "ok")
	response = get(t, server, http.MethodPost, "/health")
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != "GET,HEAD" {
		t.Fatalf("POST /health = %d %v", response.StatusCode, response.Header)
	}

	disabled := startServer(t, newScriptedEntry(), acphttp.ServerOptions{DisableHealth: true})
	expectStatus(t, get(t, disabled, http.MethodGet, "/health"), http.StatusNotFound, "")

	// With the health endpoint off, /health can be the ACP path.
	custom := startServer(t, newScriptedEntry(), acphttp.ServerOptions{Path: "/health", DisableHealth: true})
	request, err := http.NewRequest(http.MethodPost, custom.URL+"/health", strings.NewReader(initializeBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err = custom.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get(acphttp.HeaderConnectionID) == "" {
		t.Fatalf("initialize at /health = %d", response.StatusCode)
	}
}

func TestServerOptionsValidate(t *testing.T) {
	for _, options := range []acphttp.ServerOptions{
		{Path: "acp"},
		{Path: "/health"},
	} {
		if err := options.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil", options)
		}
		if _, err := acphttp.NewServer(newScriptedEntry(), options); err == nil {
			t.Errorf("NewServer(%+v) = nil error", options)
		}
		recorder := httptest.NewRecorder()
		(&acphttp.Server{Entry: newScriptedEntry(), Options: options}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/acp", nil))
		if recorder.Code != http.StatusInternalServerError {
			t.Errorf("ServeHTTP with %+v = %d", options, recorder.Code)
		}
	}
	if err := (acphttp.ServerOptions{}).Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := acphttp.NewServer(nil, acphttp.ServerOptions{}); err == nil {
		t.Fatal("NewServer(nil) = nil error")
	}
}

func TestCORSOptions(t *testing.T) {
	if acphttp.CORSDisabled().Enabled() || (acphttp.ServerOptions{}).CORS.Enabled() {
		t.Fatal("CORS must be disabled by default")
	}
	if !acphttp.AllowAnyOrigin().AllowsAnyOrigin() {
		t.Fatal("AllowAnyOrigin")
	}
	_, err := acphttp.AllowOrigins("https://ok.example", "bad\norigin")
	var invalid *acphttp.InvalidOriginError
	if !errors.As(err, &invalid) || invalid.Origin != "bad\norigin" || err.Error() != `invalid CORS origin "bad\norigin"` || !errors.Is(err, acphttp.ErrInvalidHeaderValue) {
		t.Fatalf("AllowOrigins with a newline = %v", err)
	}
	cors, err := acphttp.AllowOrigins("http://localhost:5173")
	if err != nil {
		t.Fatal(err)
	}
	if got := cors.Origins(); len(got) != 1 || got[0] != "http://localhost:5173" {
		t.Fatalf("Origins = %q", got)
	}
}

func TestServerCORS(t *testing.T) {
	preflight := func(t *testing.T, server testServer, origin string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodOptions, server.URL+acphttp.DefaultPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", origin)
		request.Header.Set("Access-Control-Request-Method", "POST")
		request.Header.Set("Access-Control-Request-Headers", "content-type,acp-connection-id")
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}

	disabled := startServer(t, newScriptedEntry(), acphttp.ServerOptions{})
	response := preflight(t, disabled, "http://localhost:5173")
	if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("preflight with CORS disabled = %d %v", response.StatusCode, response.Header)
	}

	cors, err := acphttp.AllowOrigins("http://localhost:5173")
	if err != nil {
		t.Fatal(err)
	}
	allowlist := startServer(t, newScriptedEntry(), acphttp.ServerOptions{CORS: cors})
	response = preflight(t, allowlist, "http://localhost:5173")
	want := map[string]string{
		"Access-Control-Allow-Origin":  "http://localhost:5173",
		"Access-Control-Allow-Methods": "GET,POST,DELETE,OPTIONS",
		"Access-Control-Allow-Headers": "content-type,accept,acp-connection-id,acp-session-id,sec-websocket-version,sec-websocket-key,connection,upgrade",
		"Vary":                         "origin",
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("preflight status = %d", response.StatusCode)
	}
	for key, value := range want {
		if got := response.Header.Get(key); got != value {
			t.Errorf("preflight %s = %q, want %q", key, got, value)
		}
	}
	if strings.Contains(strings.ToLower(response.Header.Get("Access-Control-Allow-Headers")), "authorization") || response.Header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("the built-in policy must not allow credentials")
	}
	response = preflight(t, allowlist, "http://localhost:3000")
	if response.StatusCode != http.StatusOK || response.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("preflight from another origin = %d %v", response.StatusCode, response.Header)
	}

	request, err := http.NewRequest(http.MethodPost, allowlist.URL+acphttp.DefaultPath, strings.NewReader(initializeBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://localhost:5173")
	response, err = allowlist.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" ||
		response.Header.Get("Access-Control-Expose-Headers") != "acp-connection-id,acp-session-id" || response.Header.Get("Vary") != "origin" {
		t.Fatalf("CORS initialize = %d %v", response.StatusCode, response.Header)
	}

	anyOrigin := startServer(t, newScriptedEntry(), acphttp.ServerOptions{CORS: acphttp.AllowAnyOrigin()})
	response = preflight(t, anyOrigin, "https://example.com")
	if response.StatusCode != http.StatusOK || response.Header.Get("Access-Control-Allow-Origin") != "*" || response.Header.Get("Vary") != "" {
		t.Fatalf("wildcard preflight = %d %v", response.StatusCode, response.Header)
	}
}
