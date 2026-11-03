package acphttp_test

import (
	"context"
	"encoding/json"
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

// rawStreamServer is the server side of the binding: it answers initialize
// with a fixed body, writes stream as the connection stream's raw bytes, and
// records every POST body.
type rawStreamServer struct {
	initialize string
	stream     string
	posts      chan string
}

func (s *rawStreamServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		s.posts <- string(body)
		if r.Header.Get(acphttp.HeaderConnectionID) == "" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set(acphttp.HeaderConnectionID, "conn")
			_, _ = io.WriteString(w, s.initialize)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, s.stream)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	case http.MethodDelete:
		w.WriteHeader(http.StatusAccepted)
	}
}

func nextPost(t *testing.T, ctx context.Context, posts chan string) string {
	t.Helper()
	select {
	case body := <-posts:
		return body
	case <-ctx.Done():
		t.Fatal("no POST")
		return ""
	}
}

// The client posts each message as the Rust client does, serialized again
// with serde_json (.json(&msg)): no HTML escapes, and numbers as serde_json
// writes them.
func TestHTTPClientPostsMessagesAsRust(t *testing.T) {
	fake := &rawStreamServer{
		initialize: `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`,
		posts:      make(chan string, 4),
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	client, err := acphttp.Dial(ctx, server.URL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Call(ctx, "initialize", map[string]any{"protocolVersion": 1, "s": "<&>"}, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := nextPost(t, ctx, fake.posts), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"s":"<&>"}}`; got != want {
		t.Errorf("initialize POST = %s, want %s", got, want)
	}
	if err := client.Notify(ctx, "_test/n", json.RawMessage(`{"z":1.0,"a":1e2,"b":12345678901234567890123,"e":"\u00e9\/"}`)); err != nil {
		t.Fatal(err)
	}
	if got, want := nextPost(t, ctx, fake.posts), `{"jsonrpc":"2.0","method":"_test/n","params":{"z":1.0,"a":100.0,"b":1.2345678901234568e+22,"e":"é/"}}`; got != want {
		t.Errorf("notification POST = %s, want %s", got, want)
	}
}

// A message scoped to the empty session id carries an empty Acp-Session-Id,
// as the Rust client sends the header for every scoped message and stream:
// the session's stream opens with it, and the POST carries it too.
func TestHTTPClientSendsEmptySessionHeader(t *testing.T) {
	agent := &recordingAgent{lines: make(chan string, 4)}
	acpServer := &acphttp.Server{Entry: agent}
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(acphttp.HeaderConnectionID) != "" {
			values, ok := r.Header[acphttp.HeaderSessionID]
			mu.Lock()
			seen = append(seen, r.Method+" "+fmt.Sprint(ok, values))
			mu.Unlock()
		}
		acpServer.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { _ = acpServer.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	client, err := acphttp.Dial(ctx, server.URL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Call(ctx, "initialize", map[string]any{"protocolVersion": 1}, nil); err != nil {
		t.Fatal(err)
	}
	<-agent.lines
	if err := client.Notify(ctx, "session/cancel", map[string]any{"sessionId": ""}); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-agent.lines:
		if want := `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":""}}`; line != want {
			t.Fatalf("agent read %s, want %s", line, want)
		}
	case <-ctx.Done():
		t.Fatal("the notification did not reach the agent")
	}
	mu.Lock()
	defer mu.Unlock()
	// The connection stream has no session header; the session stream and
	// the POST have an empty one.
	want := []string{"GET false []", "GET true []", "POST true []"}
	if got := append([]string(nil), seen...); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %q, want %q", got, want)
	}
}

// The client reads its event streams as the Rust client's eventsource-stream
// parser does: lines end at CRLF, CR, or LF, a leading byte order mark and
// comments are skipped, data fields are joined with "\n", one leading space
// is dropped from each, and an event with empty data is skipped. Data that is
// one JSON-RPC message only once its lines are joined still arrives as that
// message, and joined data that is not JSON gets the parse error the Rust
// client's connection answers it with.
func TestHTTPClientParsesEventStreamAsRust(t *testing.T) {
	fake := &rawStreamServer{
		initialize: `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`,
		stream: "\uFEFF: hello\r\n" +
			"data: {\"jsonrpc\":\"2.0\",\r\ndata:\"method\":\"_test/a\",\r\ndata:  \"params\":{\"k\":\"\\u003c\"}}\r\n\r\n" +
			"event: x\ndata\n\n" +
			"data:{\"jsonrpc\":\"2.0\",\"method\":\"_test/b\",\"params\":{}}\rid: 1\r\r" +
			"data: not\ndata: json\n\n" +
			"data: {\"jsonrpc\":\"2.0\",\"method\":\"_test/c\",\"params\":{}}\n\n",
		posts: make(chan string, 4),
	}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	var mu sync.Mutex
	var got []string
	done := make(chan struct{})
	client, err := acphttp.Dial(ctx, server.URL, nil, func(method string, params json.RawMessage) error {
		mu.Lock()
		got = append(got, method+" "+string(params))
		mu.Unlock()
		if method == "_test/c" {
			close(done)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Call(ctx, "initialize", map[string]any{"protocolVersion": 1}, nil); err != nil {
		t.Fatal(err)
	}
	nextPost(t, ctx, fake.posts)
	if got, want := nextPost(t, ctx, fake.posts), `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error","data":{"line":"not\njson"}}}`; got != want {
		t.Errorf("answer to the joined data = %s, want %s", got, want)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the last event was not delivered")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{`_test/a {"k":"<"}`, `_test/b {}`, `_test/c {}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notifications = %q, want %q", got, want)
	}
}
