package acphttp_test

import (
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

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/transport/http"
)

// initializeServer is a fake server side of the binding whose answers to
// initialize are scripted, one per initialize POST, and which records every
// request it gets.
type initializeServer struct {
	// answers are used in order; each formats the request id into its body.
	answers []initializeAnswer

	mu       sync.Mutex
	next     int
	requests []string
}

type initializeAnswer struct {
	status       int
	connectionID string
	body         string
}

func (s *initializeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, fmt.Sprintf("%s conn=%s accept=%s", r.Method, r.Header.Get(acphttp.HeaderConnectionID), r.Header.Get("Accept")))
	s.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.Header.Get(acphttp.HeaderConnectionID) == "":
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &request)
		s.mu.Lock()
		answer := s.answers[s.next]
		s.next++
		s.mu.Unlock()
		if answer.connectionID != "" {
			w.Header().Set(acphttp.HeaderConnectionID, answer.connectionID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.status)
		_, _ = io.WriteString(w, strings.ReplaceAll(answer.body, "$ID", string(request.ID)))
	case r.Method == http.MethodPost:
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *initializeServer) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func dialInitializeServer(t *testing.T, fake *initializeServer) (*acp.Connection, context.Context) {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	client, err := acphttp.Dial(ctx, server.URL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client, ctx
}

// A failed initialize whose response named a connection deletes it, as the
// Rust client does: the id is stored before the status and body are checked.
// The caller gets the Rust client's message.
func TestClientDeletesConnectionOfFailedInitialize(t *testing.T) {
	for name, test := range map[string]struct {
		answer initializeAnswer
		err    string
	}{
		"status":         {initializeAnswer{status: http.StatusInternalServerError, connectionID: "conn", body: " boom\n"}, "initialize: HTTP 500 Internal Server Error:  boom\n"},
		"unknown status": {initializeAnswer{status: 599, connectionID: "conn", body: "x"}, "initialize: HTTP 599 <unknown status code>: x"},
		"malformed body": {initializeAnswer{status: http.StatusOK, connectionID: "conn", body: `{"jsonrpc":"2.0","id":$ID,"result":`}, "initialize: invalid initialize response: Parse error: {\n  \"line\": \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"result\\\":\"\n}"},
		"not a message":  {initializeAnswer{status: http.StatusOK, connectionID: "conn", body: `{"jsonrpc":"2.0","id":$ID}`}, "initialize: invalid initialize response: Invalid request"},
		"batch":          {initializeAnswer{status: http.StatusOK, connectionID: "conn", body: `[{"jsonrpc":"2.0","id":$ID,"result":{}}]`}, `initialize: invalid initialize response: Invalid request: "JSON-RPC batches are not supported; send each message individually"`},
	} {
		t.Run(name, func(t *testing.T) {
			fake := &initializeServer{answers: []initializeAnswer{test.answer}}
			client, ctx := dialInitializeServer(t, fake)
			if err := client.Call(ctx, "initialize", map[string]any{"protocolVersion": 1}, nil); err == nil || err.Error() != test.err {
				t.Fatalf("initialize = %v, want %q", err, test.err)
			}
			_ = client.Close()
			want := []string{"POST conn= accept=application/json", "DELETE conn=conn accept="}
			if got := fake.snapshot(); strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("requests = %q, want %q", got, want)
			}
		})
	}
}

// A failed initialize without a connection id has nothing to delete.
func TestClientFailedInitializeWithoutConnectionSendsNoDelete(t *testing.T) {
	fake := &initializeServer{answers: []initializeAnswer{{status: http.StatusOK, body: `{"jsonrpc":"2.0","id":$ID,"result":{}}`}}}
	client, ctx := dialInitializeServer(t, fake)
	err := client.Call(ctx, "initialize", map[string]any{"protocolVersion": 1}, nil)
	if want := "initialize: server did not return acp-connection-id header"; err == nil || err.Error() != want {
		t.Fatalf("initialize without a connection id = %v, want %q", err, want)
	}
	_ = client.Close()
	if got := fake.snapshot(); strings.Join(got, "\n") != "POST conn= accept=application/json" {
		t.Fatalf("requests = %q", got)
	}
}

// An initialize error response rejects the connection even when it carries a
// connection id: the caller gets the error, the connection is deleted, no
// stream opens, and the next frame must be initialize again.
func TestClientInitializeErrorResponseRejectsConnection(t *testing.T) {
	fake := &initializeServer{answers: []initializeAnswer{
		{status: http.StatusOK, connectionID: "rejected", body: `{"jsonrpc":"2.0","id":$ID,"error":{"code":-32600,"message":"Invalid request","data":"no"}}`},
		{status: http.StatusOK, connectionID: "accepted", body: `{"jsonrpc":"2.0","id":$ID,"result":{"protocolVersion":1}}`},
	}}
	client, ctx := dialInitializeServer(t, fake)
	err := client.Call(ctx, "initialize", map[string]any{"protocolVersion": 1}, nil)
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32600 {
		t.Fatalf("rejected initialize = %v", err)
	}
	if err := client.Call(ctx, "initialize", map[string]any{"protocolVersion": 1}, nil); err != nil {
		t.Fatalf("second initialize: %v", err)
	}
	if err := client.Notify(ctx, "_test/after", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	want := []string{
		"POST conn= accept=application/json",
		"DELETE conn=rejected accept=",
		"POST conn= accept=application/json",
		"GET conn=accepted accept=text/event-stream",
		"POST conn=accepted accept=application/json",
		"DELETE conn=accepted accept=",
	}
	if got := fake.snapshot(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %q, want %q", got, want)
	}
}
