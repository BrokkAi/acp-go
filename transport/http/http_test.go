package acphttp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/schema"
	"github.com/BrokkAi/acp-go/transport/http"
	agentv2 "github.com/BrokkAi/acp-go/v2/agent"
)

// Both stdio agent runtimes must be servable over the HTTP binding unchanged.
var (
	_ acphttp.Entry = (*agent.Runtime)(nil)
	_ acphttp.Entry = (*agentv2.Runtime)(nil)
)

type httpTestAgent struct{}

func (httpTestAgent) Initialize(context.Context, agent.Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion: acp.Version,
		AgentInfo:       &schema.Implementation{Name: "http-test-agent", Version: "test"},
	}, nil
}

func (httpTestAgent) NewSession(context.Context, agent.Client, schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	return schema.NewSessionResponse{SessionID: "http-session"}, nil
}

func (httpTestAgent) Prompt(_ context.Context, _ agent.Client, _ schema.PromptRequest, updates agent.SessionUpdater) (schema.PromptResponse, error) {
	if err := updates.Update(schema.SessionUpdate{AgentMessageChunk: &schema.ContentChunk{
		Content: schema.ContentBlock{Text: &schema.TextContent{Text: "hello over HTTP"}},
	}}); err != nil {
		return schema.PromptResponse{}, err
	}
	return schema.PromptResponse{StopReason: schema.StopReasonEndTurn}, nil
}

// TestHTTPTransportFullSession drives a v1 agent through the draft HTTP binding:
// initialize is answered inline, the event stream carries the session update,
// and the prompt completes with the agent's stop reason.
func TestHTTPTransportFullSession(t *testing.T) {
	acpServer := &acphttp.Server{Entry: agent.New(httpTestAgent{})}
	defer acpServer.Close()
	server := httptest.NewServer(acpServer)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	updates := make(chan acp.Update, 4)
	notifications := acp.SessionUpdates(func(update acp.Update) error {
		updates <- update
		return nil
	})
	client, err := acphttp.Dial(ctx, server.URL, nil, notifications)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	initialization, err := client.InitializeWithInfo(ctx, acp.WorkspaceCapabilities(true, true, true), acp.ClientInfo{Name: "http-test-client", Version: "1"})
	if err != nil {
		t.Fatalf("initialize over HTTP: %v", err)
	}
	if initialization.AgentInfo == nil || initialization.AgentInfo.Name != "http-test-agent" {
		t.Fatalf("unexpected initialization: %+v", initialization)
	}
	session, err := client.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("session/new over HTTP: %v", err)
	}
	reason, err := client.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent("hi")})
	if err != nil {
		t.Fatalf("session/prompt over HTTP: %v", err)
	}
	if reason != schema.StopReasonEndTurn {
		t.Fatalf("stop reason = %q", reason)
	}
	select {
	case update := <-updates:
		chunk := update.Update.AgentMessageChunk
		if chunk == nil || chunk.Content.Text == nil || chunk.Content.Text.Text != "hello over HTTP" {
			t.Fatalf("unexpected update: %+v", update)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session update was not delivered on the event stream")
	}
}

// TestHTTPTransportRejectsMisuse covers the binding's error paths: a missing
// JSON content type, a later frame without a connection id, a session-scoped
// method without the session header, and an unknown connection.
func TestHTTPTransportRejectsMisuse(t *testing.T) {
	acpServer := &acphttp.Server{Entry: agent.New(httpTestAgent{})}
	defer acpServer.Close()
	server := httptest.NewServer(acpServer)
	defer server.Close()

	post := func(t *testing.T, body string, headers map[string]string) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, server.URL+acphttp.DefaultPath, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}

	if response := post(t, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, nil); response.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("missing content type status = %d", response.StatusCode)
	}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientInfo":{"name":"c","version":"1"}}}`
	if response := post(t, `{"jsonrpc":"2.0","id":2,"method":"session/new","params":{}}`, map[string]string{"Content-Type": "application/json"}); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("first frame without initialize status = %d", response.StatusCode)
	}
	if response := post(t, `{"jsonrpc":"2.0","id":2,"method":"session/list","params":{}}`, map[string]string{"Content-Type": "application/json", acphttp.HeaderConnectionID: "missing"}); response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown connection status = %d", response.StatusCode)
	}

	// Open a real connection, then exercise the session-header requirement.
	response := post(t, initialize, map[string]string{"Content-Type": "application/json"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d", response.StatusCode)
	}
	connectionID := response.Header.Get(acphttp.HeaderConnectionID)
	if connectionID == "" {
		t.Fatal("initialize response is missing the connection id")
	}
	if response := post(t, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s"}}`, map[string]string{"Content-Type": "application/json", acphttp.HeaderConnectionID: connectionID}); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing session header status = %d", response.StatusCode)
	}
	if response := post(t, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"s"}}`, map[string]string{"Content-Type": "application/json", acphttp.HeaderConnectionID: connectionID, acphttp.HeaderSessionID: "s"}); response.StatusCode != http.StatusAccepted {
		t.Fatalf("session-scoped status = %d", response.StatusCode)
	}
}

func TestHTTPTransportStreamRequiresAcceptHeader(t *testing.T) {
	acpServer := &acphttp.Server{Entry: agent.New(httpTestAgent{})}
	defer acpServer.Close()
	server := httptest.NewServer(acpServer)
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+acphttp.DefaultPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(acphttp.HeaderConnectionID, "missing")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("missing Accept status = %d", response.StatusCode)
	}
}
