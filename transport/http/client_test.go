package acphttp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/transport/http"
)

// requestLog records the method and session header of each request a handler
// serves.
type requestLog struct {
	next http.Handler

	mu       sync.Mutex
	requests []string
}

func (l *requestLog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.mu.Lock()
	l.requests = append(l.requests, r.Method+" "+r.Header.Get(acphttp.HeaderSessionID))
	l.mu.Unlock()
	l.next.ServeHTTP(w, r)
}

func (l *requestLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.requests...)
}

// TestClientOpensSessionStreamsAndDeletes covers the client's stream
// lifecycle: a session stream opens before the first message naming the
// session, so a session/load replay arrives on it, and Close sends DELETE.
func TestClientOpensSessionStreamsAndDeletes(t *testing.T) {
	entry := newScriptedEntry()
	acpServer, err := acphttp.NewServer(entry, acphttp.ServerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer acpServer.Close()
	log := &requestLog{next: acpServer}
	server := httptest.NewServer(log)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	updates := make(chan string, 4)
	client, err := acphttp.Dial(ctx, server.URL, nil, func(method string, params json.RawMessage) error {
		updates <- method + " " + string(params)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ctx, "initialize", map[string]any{"protocolVersion": 1}, nil); err != nil {
		t.Fatal(err)
	}
	agent := entry.agent(t)

	loaded := make(chan error, 1)
	go func() {
		loaded <- client.Call(ctx, "session/load", map[string]any{"sessionId": "old", "cwd": "/", "mcpServers": []any{}}, nil)
	}()
	var load struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(agent.next(t)), &load); err != nil {
		t.Fatal(err)
	}
	agent.send(t, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"old","update":{"replayed":true}}}`)
	agent.send(t, `{"jsonrpc":"2.0","id":`+string(load.ID)+`,"result":{}}`)
	if err := <-loaded; err != nil {
		t.Fatalf("session/load: %v", err)
	}
	select {
	case update := <-updates:
		if !strings.Contains(update, `"replayed":true`) {
			t.Fatalf("update = %s", update)
		}
	case <-ctx.Done():
		t.Fatal("the replayed update was not delivered")
	}

	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST ", "GET ", "GET old", "POST old", "DELETE "}
	if got := log.snapshot(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("client requests = %q, want %q", got, want)
	}
	select {
	case <-agent.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("DELETE did not stop the agent")
	}
}
