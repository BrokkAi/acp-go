package acphttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	schemav2 "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	agentv2 "github.com/BrokkAi/acp-go/v2/agent"
)

// v2WSAgent is a minimal draft-v2 agent.
type v2WSAgent struct{}

func (v2WSAgent) Initialize(context.Context, agentv2.Client, schemav2.InitializeRequest) (schemav2.InitializeResponse, error) {
	return schemav2.InitializeResponse{
		ProtocolVersion: acpv2.Version,
		Info:            schemav2.Implementation{Name: "ws-v2-agent", Version: "1"},
		Capabilities:    &schemav2.AgentCapabilities{Session: &schemav2.SessionCapabilities{}},
	}, nil
}

func (v2WSAgent) NewSession(context.Context, agentv2.Client, schemav2.NewSessionRequest) (schemav2.NewSessionResponse, error) {
	return schemav2.NewSessionResponse{SessionID: "ws-v2-session"}, nil
}

func (v2WSAgent) Prompt(context.Context, agentv2.Client, schemav2.PromptRequest, agentv2.SessionUpdater) (schemav2.PromptResponse, error) {
	return schemav2.PromptResponse{MessageID: "m1"}, nil
}

func (v2WSAgent) ListSessions(context.Context, agentv2.Client, schemav2.ListSessionsRequest) (schemav2.ListSessionsResponse, error) {
	return schemav2.ListSessionsResponse{}, nil
}

func (v2WSAgent) ResumeSession(context.Context, agentv2.Client, schemav2.ResumeSessionRequest) (schemav2.ResumeSessionResponse, error) {
	return schemav2.ResumeSessionResponse{}, nil
}

func (v2WSAgent) CloseSession(context.Context, agentv2.Client, schemav2.CloseSessionRequest) (schemav2.CloseSessionResponse, error) {
	return schemav2.CloseSessionResponse{}, nil
}

func (v2WSAgent) CancelSession(schemav2.CancelSessionNotification) error { return nil }

// dialWSStreams opens the WebSocket binding and returns it as the stream
// pair a connection reads and writes.
func dialWSStreams(t *testing.T, endpoint string) (io.ReadCloser, io.WriteCloser) {
	t.Helper()
	ws, err := websocketHandshake(context.Background(), endpoint, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	transport := &wsTransport{ws: ws, feed: writer, stopWatch: func() bool { return true }}
	go transport.read()
	return reader, transport
}

// TestWebSocketServesV2Agent serves the draft-v2 agent runtime over the
// WebSocket binding unchanged.
func TestWebSocketServesV2Agent(t *testing.T) {
	acpServer := &Server{Entry: agentv2.New(v2WSAgent{})}
	defer acpServer.Close()
	server := httptest.NewServer(acpServer)
	defer server.Close()
	in, out := dialWSStreams(t, "ws"+strings.TrimPrefix(server.URL, "http")+DefaultPath)
	connection := acpv2.Connect(in, out, nil, nil)
	defer connection.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	initialization, err := connection.InitializeWithInfo(ctx, acpv2.Capabilities{}, acpv2.ClientInfo{Name: "ws-v2-client", Version: "1"})
	if err != nil {
		t.Fatalf("v2 initialize over WebSocket: %v", err)
	}
	session, err := connection.NewSessionWithOptions(ctx, initialization, t.TempDir(), acpv2.NewSessionOptions{})
	if err != nil {
		t.Fatalf("v2 session/new over WebSocket: %v", err)
	}
	if session.SessionID != "ws-v2-session" {
		t.Fatalf("session = %+v", session)
	}
}

// TestWebSocketDialContextBoundsConnection checks that cancelling Dial's
// context ends the connection, as it ends the HTTP binding's requests.
func TestWebSocketDialContextBoundsConnection(t *testing.T) {
	acpServer := &Server{Entry: agentv2.New(v2WSAgent{})}
	defer acpServer.Close()
	server := httptest.NewServer(acpServer)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	connection, err := Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	cancel()
	select {
	case <-connection.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("connection did not end with its context")
	}
}

// pipeConn joins two wsConns over an in-memory pipe.
func pipeConn(t *testing.T) (client, server *wsConn, serverSide net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return newWSConn(a, nil, wsClientRole), newWSConn(b, nil, wsServerRole), b
}

// TestWSCodecRoundTrip writes messages of every length form from each side
// and reads them back on the other: client frames are masked with a fresh
// key, server frames are not, and lengths use the shortest form.
func TestWSCodecRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, 125, 126, 65535, 65536, 70000} {
		payload := bytes.Repeat([]byte("a"), size)
		client, server, _ := pipeConn(t)
		go func() { _ = client.writeMessage(opText, payload) }()
		message, err := server.readMessage()
		if err != nil || message.kind != wsText || !bytes.Equal(message.payload, payload) {
			t.Fatalf("size %d: server read %v, %v", size, message.kind, err)
		}
		go func() { _ = server.writeMessage(opBinary, payload) }()
		message, err = client.readMessage()
		if err != nil || message.kind != wsBinary || !bytes.Equal(message.payload, payload) {
			t.Fatalf("size %d: client read %v, %v", size, message.kind, err)
		}
	}
}

// TestWSClientFramesUseFreshMasks checks two client frames with the same
// payload are masked with different keys.
func TestWSClientFramesUseFreshMasks(t *testing.T) {
	var buf bytes.Buffer
	client := newWSConn(nopReadWriteCloser{&buf}, nil, wsClientRole)
	for range 2 {
		if err := client.writeMessage(opText, []byte("same payload")); err != nil {
			t.Fatal(err)
		}
	}
	frames := buf.Bytes()
	first, second := frames[:2+4+12], frames[2+4+12:]
	if first[0] != 0x81 || first[1] != 0x80|12 || second[1] != 0x80|12 {
		t.Fatalf("unexpected headers % x / % x", first[:2], second[:2])
	}
	if bytes.Equal(first[2:6], second[2:6]) {
		t.Fatal("two frames reused one mask key")
	}
}

type nopReadWriteCloser struct{ *bytes.Buffer }

func (nopReadWriteCloser) Close() error { return nil }

// wsReadAll feeds raw frames to a server-side reader and returns every
// message and the error that ended reading.
func wsReadAll(t *testing.T, role wsRole, data []byte) ([]wsMessage, error) {
	t.Helper()
	conn := newWSConn(nopReadWriteCloser{bytes.NewBuffer(data)}, nil, role)
	var messages []wsMessage
	for {
		message, err := conn.readMessage()
		if err != nil {
			return messages, err
		}
		messages = append(messages, message)
	}
}

func clientFrame(op byte, fin bool, payload []byte) []byte {
	first := op
	if fin {
		first |= 0x80
	}
	frame := []byte{first}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n < 65536:
		frame = binary.BigEndian.AppendUint16(append(frame, 0x80|126), uint16(n))
	default:
		frame = binary.BigEndian.AppendUint64(append(frame, 0x80|127), uint64(n))
	}
	key := [4]byte{1, 2, 3, 4}
	frame = append(frame, key[:]...)
	start := len(frame)
	frame = append(frame, payload...)
	maskBytes(frame[start:], key)
	return frame
}

// TestWSFragmentation covers reassembly: control frames between fragments,
// a UTF-8 sequence split across fragments, and the fragmentation rules
// tungstenite enforces.
func TestWSFragmentation(t *testing.T) {
	join := func(frames ...[]byte) []byte { return bytes.Join(frames, nil) }
	messages, err := wsReadAll(t, wsServerRole, join(
		clientFrame(opText, false, []byte("{\"a\":\"\xc3")),
		clientFrame(opPong, true, []byte("x")),
		clientFrame(opContinue, false, []byte("\xa9")),
		clientFrame(opContinue, true, []byte("\"}")),
	))
	if !errors.Is(err, errWSReset) || len(messages) != 2 || messages[0].kind != wsPong || string(messages[1].payload) != "{\"a\":\"é\"}" {
		t.Fatalf("messages %+v, err %v", messages, err)
	}
	for _, test := range []struct {
		name string
		data []byte
		want string
	}{
		{"continue-alone", clientFrame(opContinue, true, nil), "WebSocket protocol error: Continue frame but nothing to continue"},
		{"text-in-fragment", join(clientFrame(opBinary, false, nil), clientFrame(opText, true, nil)), "WebSocket protocol error: While waiting for more fragments received: TEXT"},
		{"invalid-first", clientFrame(opText, false, []byte("a\xff")), "UTF-8 encoding error: �"},
		{"invalid-completion", join(clientFrame(opText, false, []byte("\xe2\x82")), clientFrame(opContinue, true, []byte("("))), "UTF-8 encoding error: �"},
		{"incomplete-end", join(clientFrame(opText, false, []byte("\xf0\x9f")), clientFrame(opContinue, true, []byte("\x98"))), "UTF-8 encoding error: incomplete string: Incomplete { buffer: [240, 159, 152, 0], buffer_len: 3 }"},
		{"single-invalid", clientFrame(opText, true, []byte("ab\xff")), "UTF-8 encoding error: invalid utf-8 sequence of 1 bytes from index 2"},
		{"fragmented-control", clientFrame(opPing, false, nil), "WebSocket protocol error: Fragmented control frame"},
		{"unmasked", []byte{0x81, 0x00}, "WebSocket protocol error: Received an unmasked frame from client"},
		{"invalid-opcode", []byte{0x83, 0x80, 0, 0, 0, 0}, "WebSocket protocol error: Encountered invalid opcode: 3"},
		{"too-long", []byte{0x81, 0xff, 0, 0, 0, 0, 1, 0, 0, 1, 9, 9, 9, 9}, "Space limit exceeded: Message too long: 16777217 > 16777216"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := wsReadAll(t, wsServerRole, test.data)
			if err == nil || err.Error() != test.want {
				t.Fatalf("err = %v, want %s", err, test.want)
			}
		})
	}
}

// TestWSMessageSizeLimit checks a fragmented message may hold 64 MiB and no
// more, counting a pending UTF-8 sequence.
func TestWSMessageSizeLimit(t *testing.T) {
	message := &wsIncomplete{}
	if err := message.extend(make([]byte, wsMaxMessage)); err != nil {
		t.Fatal(err)
	}
	if err := message.extend([]byte{0}); err == nil || err.Error() != "Space limit exceeded: Message too long: 67108865 > 67108864" {
		t.Fatalf("err = %v", err)
	}
	text := &wsIncomplete{text: true}
	if err := text.extend(append(bytes.Repeat([]byte("a"), wsMaxMessage-1), 0xc3)); err != nil {
		t.Fatal(err)
	}
	if err := text.extend([]byte{0xa9}); err == nil || err.Error() != "Space limit exceeded: Message too long: 67108865 > 67108864" {
		t.Fatalf("err = %v", err)
	}
}

// TestWSCloseFrames checks how close frames are read and shown: codes that
// may not be received become 1002 "Protocol violation", and the reason is
// debug-printed as the bytes crate does.
func TestWSCloseFrames(t *testing.T) {
	for _, test := range []struct {
		payload []byte
		want    string
	}{
		{nil, "None"},
		{[]byte{0x03, 0xe8}, `Some(CloseFrame { code: Normal, reason: Utf8Bytes(b"") })`},
		{append([]byte{0x03, 0xe9}, "a\"\\\n\r\t\x00\x7f\xc3\xa9"...), `Some(CloseFrame { code: Away, reason: Utf8Bytes(b"a\"\\\n\r\t\0\x7f\xc3\xa9") })`},
		{[]byte{0x0b, 0xb9}, `Some(CloseFrame { code: Iana(3001), reason: Utf8Bytes(b"") })`},
		{[]byte{0x0f, 0xa0}, `Some(CloseFrame { code: Library(4000), reason: Utf8Bytes(b"") })`},
		{[]byte{0x03, 0xed}, `Some(CloseFrame { code: Protocol, reason: Utf8Bytes(b"Protocol violation") })`},
		{[]byte{0x03, 0xec}, `Some(CloseFrame { code: Protocol, reason: Utf8Bytes(b"Protocol violation") })`},
		{[]byte{0x07, 0xd0}, `Some(CloseFrame { code: Protocol, reason: Utf8Bytes(b"Protocol violation") })`},
	} {
		frame, err := parseCloseFrame(test.payload)
		if err != nil {
			t.Fatal(err)
		}
		if got := closeFrameDebug(frame); got != test.want {
			t.Errorf("close % x: got %s, want %s", test.payload, got, test.want)
		}
	}
	if _, err := parseCloseFrame([]byte{3}); err == nil || err.Error() != "WebSocket protocol error: Invalid close sequence" {
		t.Fatalf("one-byte close: %v", err)
	}
	if _, err := parseCloseFrame([]byte{3, 0xe8, 0xff}); err == nil || err.Error() != "UTF-8 encoding error: invalid utf-8 sequence of 1 bytes from index 0" {
		t.Fatalf("invalid reason: %v", err)
	}
}

// TestWSPingIsAnswered checks a ping is answered with a pong carrying its
// payload, written while the other side keeps reading.
func TestWSPingIsAnswered(t *testing.T) {
	client, server, _ := pipeConn(t)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		message, err := server.readMessage()
		if err != nil || message.kind != wsPing || string(message.payload) != "hi" {
			t.Errorf("server read %+v, %v", message, err)
		}
	}()
	if err := client.writeMessage(opPing, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	message, err := client.readMessage()
	if err != nil || message.kind != wsPong || string(message.payload) != "hi" {
		t.Fatalf("client read %+v, %v", message, err)
	}
	wg.Wait()
}

// TestWebSocketClientAnswersMultilineParseError checks a frame of several
// lines that is not JSON gets the parse error the connection would send, as
// the HTTP binding's client does for such an event.
func TestWebSocketClientAnswersMultilineParseError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	got := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		key := ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if value, ok := strings.CutPrefix(line, "Sec-Websocket-Key: "); ok {
				key = strings.TrimSpace(value)
			}
			if line == "\r\n" {
				break
			}
		}
		_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: "+wsAccept(key)+"\r\n\r\n")
		_, _ = conn.Write([]byte{0x81, 9})
		_, _ = io.WriteString(conn, "{not\njson")
		server := newWSConn(conn, reader, wsServerRole)
		message, _ := server.readMessage()
		got <- string(message.payload)
	}()
	_, _ = dialWSStreams(t, "ws://"+listener.Addr().String()+DefaultPath)
	select {
	case answer := <-got:
		if want := `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error","data":{"line":"{not\njson"}}}`; answer != want {
			t.Fatalf("answer %s, want %s", answer, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no parse error was sent")
	}
}
