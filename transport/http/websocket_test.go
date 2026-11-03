package acphttp_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/schema"
	"github.com/BrokkAi/acp-go/transport/http"
)

func wsURL(server *httptest.Server) string {
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// runWebSocketSession drives a v1 agent through the WebSocket binding with
// the same acp.Connection API as stdio: initialize, session/new, and a
// prompt whose update arrives on the socket.
func runWebSocketSession(t *testing.T, endpoint string, client *http.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	updates := make(chan acp.Update, 4)
	notifications := acp.SessionUpdates(func(update acp.Update) error {
		updates <- update
		return nil
	})
	connection, err := acphttp.Client{Endpoint: endpoint, HTTP: client}.Dial(ctx, nil, notifications)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	initialization, err := connection.InitializeWithInfo(ctx, acp.WorkspaceCapabilities(true, true, true), acp.ClientInfo{Name: "ws-test-client", Version: "1"})
	if err != nil {
		t.Fatalf("initialize over WebSocket: %v", err)
	}
	if initialization.AgentInfo == nil || initialization.AgentInfo.Name != "http-test-agent" {
		t.Fatalf("unexpected initialization: %+v", initialization)
	}
	session, err := connection.NewSession(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("session/new over WebSocket: %v", err)
	}
	reason, err := connection.PromptContent(ctx, initialization, session, []acp.Content{acp.NewTextContent("hi")})
	if err != nil {
		t.Fatalf("session/prompt over WebSocket: %v", err)
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
		t.Fatal("session update was not delivered on the socket")
	}
}

// TestWebSocketFullSession serves an existing v1 agent runtime over the
// WebSocket binding and drives it with acphttp.Dial on a ws:// URL.
func TestWebSocketFullSession(t *testing.T) {
	acpServer := &acphttp.Server{Entry: agent.New(httpTestAgent{})}
	defer acpServer.Close()
	server := httptest.NewServer(acpServer)
	defer server.Close()
	runWebSocketSession(t, wsURL(server), nil)
}

// TestWebSocketOverTLS drives the binding over wss:// against a server that
// also offers HTTP/2: the handshake is sent over HTTP/1.1, as the Rust client
// requires, with the caller's TLS configuration.
func TestWebSocketOverTLS(t *testing.T) {
	acpServer := &acphttp.Server{Entry: agent.New(httpTestAgent{})}
	defer acpServer.Close()
	server := httptest.NewUnstartedServer(acpServer)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	runWebSocketSession(t, "wss"+strings.TrimPrefix(server.URL, "https"), server.Client())
}

// TestDialRejectsOtherSchemes checks the scheme picks the binding and that
// any other scheme is refused with the Rust client's error, before any
// request.
func TestDialRejectsOtherSchemes(t *testing.T) {
	_, err := acphttp.Dial(context.Background(), "ftp://127.0.0.1/acp", nil, nil)
	if err == nil || err.Error() != "unsupported URL scheme: ftp; expected http, https, ws, or wss" {
		t.Fatalf("Dial error = %v", err)
	}
}

// TestDialWebSocketHandshakeFailure checks a refused handshake fails Dial
// with the Rust client's error and sends no ACP data.
func TestDialWebSocketHandshakeFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer server.Close()
	_, err := acphttp.Dial(context.Background(), wsURL(server), nil, nil)
	if err == nil || err.Error() != "WebSocket connect failed: unexpected status 302 Found" {
		t.Fatalf("Dial error = %v", err)
	}
}

// wsRawUpgrade opens a raw WebSocket to the server and returns the socket,
// a reader past the 101 response, and the connection id.
func wsRawUpgrade(t *testing.T, server *httptest.Server) (net.Conn, *bufio.Reader, string) {
	t.Helper()
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	request := "GET /acp HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	connectionID := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		line = strings.TrimSuffix(line, "\r\n")
		if line == "" {
			break
		}
		if value, ok := strings.CutPrefix(line, "acp-connection-id: "); ok {
			connectionID = value
		}
	}
	return conn, reader, connectionID
}

// wsReadText reads one unmasked server frame and returns its payload.
func wsReadText(t *testing.T, conn net.Conn, reader *bufio.Reader) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	head := make([]byte, 2)
	if _, err := io.ReadFull(reader, head); err != nil {
		t.Fatal(err)
	}
	if head[0] != 0x81 || head[1]&0x80 != 0 || head[1]&0x7f >= 126 {
		t.Fatalf("unexpected frame header % x", head)
	}
	payload := make([]byte, head[1])
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// cancelRecordingAgent forwards every line it reads to lines and answers
// nothing, so an initialize request stays in flight.
type cancelRecordingAgent struct{ lines chan string }

func (a cancelRecordingAgent) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	reader := bufio.NewReader(in)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil
		}
		a.lines <- strings.TrimSuffix(line, "\n")
	}
}

// TestWebSocketCancelBeforeInitializeResponse checks that, unlike HTTP+SSE,
// the WebSocket binding carries a $/cancel_request as soon as the socket is
// open, while the initialize request is still in flight, in order.
func TestWebSocketCancelBeforeInitializeResponse(t *testing.T) {
	agent := cancelRecordingAgent{lines: make(chan string, 4)}
	acpServer := &acphttp.Server{Entry: agent}
	defer acpServer.Close()
	server := httptest.NewServer(acpServer)
	defer server.Close()
	conn, _, _ := wsRawUpgrade(t, server)
	for _, message := range []string{
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}`,
		`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":0}}`,
	} {
		if _, err := conn.Write(wsTestFrame(0x1, true, []byte(message))); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}`,
		`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":0}}`,
	} {
		select {
		case got := <-agent.lines:
			if got != want {
				t.Fatalf("agent read %s, want %s", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("agent did not read %s", want)
		}
	}
}

// TestWebSocketServerCloseEndsSockets checks Server.Close delivers what the
// agent queued and closes the socket without a close frame, as the Rust
// server drops it.
func TestWebSocketServerCloseEndsSockets(t *testing.T) {
	acpServer := &acphttp.Server{Entry: agent.New(httpTestAgent{})}
	server := httptest.NewServer(acpServer)
	defer server.Close()
	conn, reader, connectionID := wsRawUpgrade(t, server)
	if connectionID == "" {
		t.Fatal("101 response has no connection id")
	}
	if _, err := conn.Write(wsTestFrame(0x1, true, []byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1,"clientInfo":{"name":"c","version":"1"}}}`))); err != nil {
		t.Fatal(err)
	}
	if got := wsReadText(t, conn, reader); !strings.Contains(got, `"id":0,"result"`) {
		t.Fatalf("initialize answer = %s", got)
	}
	_ = acpServer.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if rest, err := io.ReadAll(reader); err != nil || len(rest) != 0 {
		t.Fatalf("after Server.Close read %q, %v; want a plain end of stream", rest, err)
	}
}

// TestWebSocketClientTimeoutCoversHandshakeOnly checks Client.HTTP's Timeout
// bounds the handshake and not the socket, as reqwest's request timeout
// covers only the opening handshake.
func TestWebSocketClientTimeoutCoversHandshakeOnly(t *testing.T) {
	acpServer := &acphttp.Server{Entry: agent.New(httpTestAgent{})}
	defer acpServer.Close()
	server := httptest.NewServer(acpServer)
	defer server.Close()
	client := &http.Client{Timeout: 200 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := acphttp.Client{Endpoint: wsURL(server), HTTP: client}.Dial(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	time.Sleep(400 * time.Millisecond)
	if _, err := connection.InitializeWithInfo(ctx, acp.WorkspaceCapabilities(true, true, true), acp.ClientInfo{Name: "c", Version: "1"}); err != nil {
		t.Fatalf("initialize after the client timeout: %v", err)
	}

	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		conn, err := silent.Accept()
		if err == nil {
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn)
		}
	}()
	start := time.Now()
	_, err = acphttp.Client{Endpoint: "ws://" + silent.Addr().String(), HTTP: &http.Client{Timeout: 100 * time.Millisecond}}.Dial(context.Background(), nil, nil)
	if err == nil || !strings.HasPrefix(err.Error(), "WebSocket connect failed: ") || time.Since(start) > 5*time.Second {
		t.Fatalf("handshake against a silent server: %v after %v", err, time.Since(start))
	}
}
