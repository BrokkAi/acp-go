package acphttp_test

import (
	"bufio"
	"bytes"
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

	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/transport/http"
)

// batchRejection is the exact JSON-RPC error the reference SDK answers any
// batch array with.
const batchRejection = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"JSON-RPC batches are not supported; send each message individually"}}`

// recordingEntry serves a real agent and records every line it is given and
// every connection it starts.
type recordingEntry struct {
	inner acphttp.Entry

	mu          sync.Mutex
	connections int
	lines       []string
}

func (e *recordingEntry) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	e.mu.Lock()
	e.connections++
	e.mu.Unlock()
	reader, writer := io.Pipe()
	go func() {
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		for scanner.Scan() {
			e.mu.Lock()
			e.lines = append(e.lines, scanner.Text())
			e.mu.Unlock()
			if _, err := writer.Write(append(scanner.Bytes(), '\n')); err != nil {
				break
			}
		}
		_ = writer.CloseWithError(scanner.Err())
	}()
	return e.inner.Serve(ctx, reader, out)
}

func (e *recordingEntry) snapshot() (int, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.connections, append([]string(nil), e.lines...)
}

func TestHTTPServerRejectsBatchPostWithJSONRPCError(t *testing.T) {
	entry := &recordingEntry{inner: agent.New(httpTestAgent{})}
	acpServer := &acphttp.Server{Entry: entry}
	defer acpServer.Close()
	server := httptest.NewServer(acpServer)
	defer server.Close()

	post := func(t *testing.T, body, connectionID string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, server.URL+acphttp.DefaultPath, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		if connectionID != "" {
			request.Header.Set(acphttp.HeaderConnectionID, connectionID)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}

	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientInfo":{"name":"c","version":"1"}}}`
	batches := []string{
		`[]`,
		`[` + initialize + `]`,
		`[{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/","mcpServers":[]}},{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}]`,
		`[{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}]`,
		`[1,2]`,
	}
	assertRejected := func(t *testing.T, response *http.Response, body string) {
		t.Helper()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status = %d", body, response.StatusCode)
		}
		if contentType := response.Header.Get("Content-Type"); contentType != "application/json" {
			t.Fatalf("%s: Content-Type = %q", body, contentType)
		}
		if id := response.Header.Get(acphttp.HeaderConnectionID); id != "" {
			t.Fatalf("%s: rejected batch returned connection id %q", body, id)
		}
		got, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != batchRejection {
			t.Fatalf("%s: body = %s, want %s", body, got, batchRejection)
		}
	}

	// Without a connection, no batch (not even one holding initialize)
	// creates one.
	for _, batch := range batches {
		assertRejected(t, post(t, batch, ""), batch)
		assertRejected(t, post(t, " \n"+batch+"\n", "unknown"), batch)
	}
	if connections, _ := entry.snapshot(); connections != 0 {
		t.Fatalf("batch POSTs created %d connections", connections)
	}

	// On an open connection, a batch is answered the same way and none of
	// its entries reaches the agent.
	response := post(t, initialize, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d", response.StatusCode)
	}
	connectionID := response.Header.Get(acphttp.HeaderConnectionID)
	for _, batch := range batches {
		assertRejected(t, post(t, batch, connectionID), batch)
	}
	if response := post(t, `{"jsonrpc":"2.0","method":"_test/after","params":{}}`, connectionID); response.StatusCode != http.StatusAccepted {
		t.Fatalf("single frame after batches status = %d", response.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		connections, lines := entry.snapshot()
		if len(lines) == 2 {
			if connections != 1 || lines[0] != initialize || !strings.Contains(lines[1], "_test/after") {
				t.Fatalf("agent saw connections=%d lines=%q", connections, lines)
			}
			break
		}
		if len(lines) > 2 || time.Now().After(deadline) {
			t.Fatalf("agent saw lines %q; batch entries must not be forwarded", lines)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeHTTPAgent is a minimal server side of the binding whose event stream
// sends caller-chosen frames, and which records every later POST body.
type fakeHTTPAgent struct {
	initializeBody string
	events         []string
	posts          chan string
}

func (f *fakeHTTPAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.Header.Get(acphttp.HeaderConnectionID) == "":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(acphttp.HeaderConnectionID, "conn")
		_, _ = io.WriteString(w, f.initializeBody)
	case r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		f.posts <- string(bytes.TrimSpace(body))
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, event := range f.events {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
}

func TestHTTPClientTreatsStreamedBatchAsMalformed(t *testing.T) {
	fake := &fakeHTTPAgent{
		initializeBody: `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`,
		events: []string{
			`[{"jsonrpc":"2.0","id":5,"method":"_test/batched","params":{}},{"jsonrpc":"2.0","method":"_test/batched_notification","params":{}}]`,
			`{"jsonrpc":"2.0","id":6,"method":"_test/after","params":{}}`,
		},
		posts: make(chan string, 4),
	}
	server := httptest.NewServer(fake)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mu sync.Mutex
	var ran []string
	record := func(method string) {
		mu.Lock()
		ran = append(ran, method)
		mu.Unlock()
	}
	client, err := acphttp.Dial(ctx, server.URL, func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		record(method)
		return map[string]bool{"ok": true}, nil
	}, func(method string, _ json.RawMessage) error {
		record(method)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Call(ctx, "initialize", map[string]any{"protocolVersion": 1}, nil); err != nil {
		t.Fatal(err)
	}

	// The client answers the array with the single Invalid Request error, and
	// then serves the next message normally.
	var posts []string
	for len(posts) < 2 {
		select {
		case body := <-fake.posts:
			posts = append(posts, body)
		case <-ctx.Done():
			t.Fatalf("client POSTs = %q", posts)
		}
	}
	if posts[0] != batchRejection || posts[1] != `{"jsonrpc":"2.0","id":6,"result":{"ok":true}}` {
		t.Fatalf("client POSTs = %q", posts)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(ran, ",") != "_test/after" {
		t.Fatalf("client handlers ran %v; batch entries must not run", ran)
	}
}

func TestHTTPClientRejectsBatchInitializeResponse(t *testing.T) {
	fake := &fakeHTTPAgent{
		initializeBody: `[{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}]`,
		posts:          make(chan string, 1),
	}
	server := httptest.NewServer(fake)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := acphttp.Dial(ctx, server.URL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	err = client.Call(ctx, "initialize", map[string]any{"protocolVersion": 1}, nil)
	if err == nil || !strings.Contains(err.Error(), "JSON-RPC batches are not supported") {
		t.Fatalf("initialize with a batch response = %v", err)
	}
}
