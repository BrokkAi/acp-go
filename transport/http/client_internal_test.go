package acphttp

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestTransport returns a transport for endpoint and the reader of the
// frames it delivers to the connection.
func newTestTransport(t *testing.T, endpoint string) (*httpTransport, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	transport := &httpTransport{endpoint: endpoint + DefaultPath, http: http.DefaultClient, ctx: ctx, cancel: cancel, feed: writer}
	t.Cleanup(func() { _ = transport.Close() })
	return transport, bufio.NewReader(reader)
}

// streamServer answers initialize with body and connection id "conn", opens
// streams that stay idle, and answers later POSTs with postStatus.
func streamServer(t *testing.T, body string, postStatus int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.Header.Get(HeaderConnectionID) == "":
			w.Header().Set(HeaderConnectionID, "conn")
			w.Header().Set("Content-Type", jsonMIME)
			_, _ = io.WriteString(w, body)
		case r.Method == http.MethodPost:
			w.WriteHeader(postStatus)
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", eventStreamMIME)
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// The initialize response is read whole, however large, instead of being cut
// at a frame limit.
func TestClientReadsWholeInitializeResponse(t *testing.T) {
	large := strings.Repeat("x", DefaultMaxBodyBytes+1024)
	body := `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1,"_meta":{"large":"` + large + `"}}}`
	server := streamServer(t, body, http.StatusAccepted)
	transport, delivered := newTestTransport(t, server.URL)
	written := make(chan error, 1)
	go func() {
		_, err := transport.Write([]byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}` + "\n"))
		written <- err
	}()
	line, err := delivered.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != body {
		t.Fatalf("delivered %d bytes, want %d", len(strings.TrimSpace(line)), len(body))
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

// A session/new request whose POST fails will get no result, so it no longer
// marks its id as opening a session.
func TestClientForgetsOpeningRequestWhenPostFails(t *testing.T) {
	server := streamServer(t, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1}}`, http.StatusInternalServerError)
	transport, delivered := newTestTransport(t, server.URL)
	go func() { _, _ = io.Copy(io.Discard, delivered) }()
	if _, err := transport.Write([]byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/","mcpServers":[]}}`)); err == nil {
		t.Fatal("session/new POST answered 500 succeeded")
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.opening) != 0 {
		t.Fatalf("opening = %v after a failed POST", transport.opening)
	}
}
