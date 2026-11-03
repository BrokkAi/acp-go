package acphttp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/transport/http"
)

// testdata/websocket_rust_server.json holds raw WebSocket exchanges and what
// the agent-client-protocol-http 5.0.0 AcpHttpServer did with each: the same
// bytes were sent over TCP to the Rust server serving a Lines agent, and its
// handshake response, the frames it wrote, how the socket ended, the answers
// to HTTP requests made with the connection id, and the lines its agent read
// were recorded. Each case runs here against acphttp.Server serving the same
// agent and must produce the same record.
type wsServerCase struct {
	Name    string            `json:"name"`
	CORS    json.RawMessage   `json:"cors"`
	Request string            `json:"request"`
	Early   string            `json:"early"`
	Steps   []json.RawMessage `json:"steps"`
	Rust    []string          `json:"rust"`
}

// wsLinesAgent is the Rust probe's agent: it records each line, answers
// initialize, test/echo (with its params), test/notify, and test/raw (with
// params.line verbatim), and stops after test/exit or test/notify-exit. Like
// a Rust Lines agent whose task has ended, a stopped agent leaves the
// connection open until the next line reaches it.
type wsLinesAgent struct {
	mu    sync.Mutex
	lines []string
}

func (a *wsLinesAgent) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	reader := bufio.NewReader(in)
	stopped := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil || stopped {
			return nil
		}
		line = strings.TrimSuffix(line, "\n")
		a.mu.Lock()
		a.lines = append(a.lines, line)
		a.mu.Unlock()
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal([]byte(line), &message) != nil {
			continue
		}
		id, params := string(message.ID), string(message.Params)
		if id == "" {
			id = "null"
		}
		if params == "" {
			params = "null"
		}
		reply := ""
		switch message.Method {
		case "initialize":
			reply = `{"jsonrpc":"2.0","id":` + id + `,"result":{"protocolVersion":1}}`
		case "test/echo":
			reply = `{"jsonrpc":"2.0","id":` + id + `,"result":` + params + `}`
		case "test/notify", "test/notify-exit":
			reply = `{"jsonrpc":"2.0","method":"test/note","params":{"n":1}}`
		case "test/raw":
			var raw struct {
				Line string `json:"line"`
			}
			_ = json.Unmarshal(message.Params, &raw)
			reply = raw.Line
		}
		if reply != "" || message.Method == "test/raw" {
			if _, err := io.WriteString(out, reply+"\n"); err != nil {
				return nil
			}
		}
		if message.Method == "test/exit" || message.Method == "test/notify-exit" {
			stopped = true
		}
	}
}

func (a *wsLinesAgent) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.lines...)
}

// wsShow is the probe's rendering of bytes: quoted with backslashes and
// quotes escaped when printable ASCII, hex otherwise, cut short when long.
func wsShow(data []byte) string {
	printable := true
	for _, b := range data {
		if b < 0x20 || b >= 0x7f {
			printable = false
			break
		}
	}
	var body string
	if printable {
		body = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(string(data)) + `"`
	} else {
		body = "hex:" + hex.EncodeToString(data)
	}
	if len(body) > 160 {
		return fmt.Sprintf("%s...(%d bytes)", body[:120], len(data))
	}
	return body
}

// wsRenderFrames decodes the frames in data as the probe does.
func wsRenderFrames(data []byte) []string {
	var out []string
	for len(data) > 0 {
		partial := func() []string { return append(out, "PARTIAL "+wsShow(data)) }
		if len(data) < 2 {
			return partial()
		}
		b0, b1 := data[0], data[1]
		pos, length, form := 2, uint64(b1&0x7f), 7
		switch length {
		case 126:
			if len(data) < 4 {
				return partial()
			}
			length, pos, form = uint64(binary.BigEndian.Uint16(data[2:])), 4, 16
		case 127:
			if len(data) < 10 {
				return partial()
			}
			length, pos, form = binary.BigEndian.Uint64(data[2:]), 10, 64
		}
		masked := b1&0x80 != 0
		var key []byte
		if masked {
			if len(data) < pos+4 {
				return partial()
			}
			key = data[pos : pos+4]
			pos += 4
		}
		if uint64(len(data)) < uint64(pos)+length {
			return partial()
		}
		payload := append([]byte(nil), data[pos:pos+int(length)]...)
		if masked {
			for i := range payload {
				payload[i] ^= key[i%4]
			}
		}
		maskBit := 0
		if masked {
			maskBit = 1
		}
		out = append(out, fmt.Sprintf("FRAME op=%d fin=%d rsv=%d mask=%d len%d=%d %s", b0&0x0f, b0>>7, (b0>>4)&7, maskBit, form, length, wsShow(payload)))
		data = data[pos+int(length):]
	}
	return out
}

func wsBigFrame(t *testing.T, raw json.RawMessage) []byte {
	var big struct {
		Prefix, Fill, Suffix string
		Count                int
		Op                   *byte
		Fin                  *bool
	}
	if err := json.Unmarshal(raw, &big); err != nil {
		t.Fatal(err)
	}
	op, fin := byte(1), true
	if big.Op != nil {
		op = *big.Op
	}
	if big.Fin != nil {
		fin = *big.Fin
	}
	payload := []byte(big.Prefix + strings.Repeat(big.Fill, big.Count) + big.Suffix)
	return wsTestFrame(op, fin, payload)
}

// wsTestFrame is a masked client frame with the probe's fixed mask key.
func wsTestFrame(op byte, fin bool, payload []byte) []byte {
	first := op
	if fin {
		first |= 0x80
	}
	frame := []byte{first}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, 0x80|byte(n))
	case n < 65536:
		frame = append(frame, 0x80|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(n))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	key := []byte{0x37, 0xfa, 0x21, 0x3d}
	frame = append(frame, key...)
	for i, b := range payload {
		frame = append(frame, b^key[i%4])
	}
	return frame
}

// wsRawHTTP sends one request on its own connection and renders the answer
// as the probe does.
func wsRawHTTP(t *testing.T, addr, method, connectionID string, session *string, body *string) string {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	request := method + " /acp HTTP/1.1\r\nHost: x\r\nConnection: close\r\nAcp-Connection-Id: " + connectionID + "\r\n"
	if session != nil {
		request += "Acp-Session-Id: " + *session + "\r\n"
	}
	if method == "GET" {
		request += "Accept: text/event-stream\r\n"
	}
	if body != nil {
		request += fmt.Sprintf("Content-Type: application/json\r\nAccept: application/json\r\nContent-Length: %d\r\n", len(*body))
	}
	request += "\r\n"
	if body != nil {
		request += *body
	}
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	answer, _ := io.ReadAll(conn)
	head, rest, _ := strings.Cut(string(answer), "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	contentType := ""
	for _, line := range lines[1:] {
		if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "content-type") {
			contentType = strings.TrimSpace(value)
		}
	}
	scope := ""
	if session != nil {
		scope = " sess=" + *session
	}
	return fmt.Sprintf("HTTP %s%s -> %s ct=%s body=%s", method, scope, lines[0], rustDebug(contentType), rustDebug(rest))
}

// rustDebug is Rust's Debug for a str, for the ASCII the probe prints.
func rustDebug(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s) + `"`
}

func runWSServerCase(t *testing.T, c wsServerCase, wantAgentLines int) []string {
	agent := &wsLinesAgent{}
	options := acphttp.ServerOptions{}
	switch {
	case string(c.CORS) == `"any"`:
		options.CORS = acphttp.AllowAnyOrigin()
	case strings.HasPrefix(string(c.CORS), "["):
		var origins []string
		if err := json.Unmarshal(c.CORS, &origins); err != nil {
			t.Fatal(err)
		}
		cors, err := acphttp.AllowOrigins(origins...)
		if err != nil {
			t.Fatal(err)
		}
		options.CORS = cors
	}
	server, err := acphttp.NewServer(agent, options)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	defer server.Close()
	addr := httpServer.Listener.Addr().String()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var mu sync.Mutex
	var received []byte
	end := "OPEN"
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 1<<20)
		for {
			n, err := conn.Read(buf)
			mu.Lock()
			received = append(received, buf[:n]...)
			switch {
			case errors.Is(err, io.EOF):
				end = "EOF"
			case err != nil && errors.Is(err, net.ErrClosed):
			case err != nil && isConnectionReset(err):
				end = "ERR ConnectionReset"
			case err != nil:
				end = "ERR " + err.Error()
			}
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	first := []byte(c.Request)
	if c.Early != "" {
		early, _ := hex.DecodeString(c.Early)
		first = append(first, early...)
	}
	if _, err := conn.Write(first); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	connectionID := ""
	mu.Lock()
	if head, _, ok := strings.Cut(string(received), "\r\n\r\n"); ok {
		for _, line := range strings.Split(head, "\r\n") {
			if name, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(name, "acp-connection-id") {
				connectionID = strings.TrimSpace(value)
			}
		}
	}
	mu.Unlock()
	var httpLog []string
	for _, raw := range c.Steps {
		var step struct {
			Send          string          `json:"send"`
			Big           json.RawMessage `json:"big"`
			Sleep         int             `json:"sleep"`
			ShutdownWrite bool            `json:"shutdown_write"`
			Post          *struct {
				Body    string  `json:"body"`
				Session *string `json:"session"`
			} `json:"post"`
			Get *struct {
				Session *string `json:"session"`
			} `json:"get"`
			Delete bool `json:"delete"`
		}
		if err := json.Unmarshal(raw, &step); err != nil {
			t.Fatal(err)
		}
		switch {
		case step.Send != "":
			data, _ := hex.DecodeString(step.Send)
			if _, err := conn.Write(data); err != nil {
				httpLog = append(httpLog, "SEND failed")
			}
		case step.Big != nil:
			if _, err := conn.Write(wsBigFrame(t, step.Big)); err != nil {
				httpLog = append(httpLog, "SEND failed")
			}
		case step.Sleep > 0:
			time.Sleep(time.Duration(step.Sleep) * time.Millisecond)
		case step.ShutdownWrite:
			_ = conn.(*net.TCPConn).CloseWrite()
		case step.Post != nil:
			httpLog = append(httpLog, wsRawHTTP(t, addr, "POST", connectionID, step.Post.Session, &step.Post.Body))
		case step.Get != nil:
			httpLog = append(httpLog, wsRawHTTP(t, addr, "GET", connectionID, step.Get.Session, nil))
		case step.Delete:
			httpLog = append(httpLog, wsRawHTTP(t, addr, "DELETE", connectionID, nil, nil))
		}
	}
	time.Sleep(300 * time.Millisecond)
	// A 16 MiB frame can take the agent longer to read than the probe
	// waited, under the race detector in particular.
	for deadline := time.Now().Add(60 * time.Second); len(agent.seen()) < wantAgentLines && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	data := append([]byte(nil), received...)
	ended := end
	mu.Unlock()
	var out []string
	if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 {
		for n, line := range strings.Split(string(data[:i]), "\r\n") {
			if n == 0 {
				out = append(out, "STATUS "+line)
				continue
			}
			name, value, _ := strings.Cut(line, ":")
			name, value = strings.ToLower(name), strings.TrimSpace(value)
			switch name {
			case "date":
				value = "<date>"
			case "acp-connection-id":
				value = "<id>"
			}
			out = append(out, "H "+name+": "+value)
		}
		rest := data[i+4:]
		if strings.Contains(out[0], " 101 ") {
			out = append(out, wsRenderFrames(rest)...)
		} else {
			out = append(out, "BODY "+wsShow(rest))
		}
	} else {
		out = append(out, "RAW "+wsShow(data))
	}
	out = append(out, "END "+ended)
	out = append(out, httpLog...)
	for _, line := range agent.seen() {
		out = append(out, "AGENT "+wsShow([]byte(line)))
	}
	return out
}

// normalizeWSRecord keeps what the binding decides and drops what belongs to
// the HTTP library: a response that is not a 101 is compared by status,
// body, and its content type and CORS headers, since hyper and net/http
// write other headers, and a failed write of the test's own frames depends
// on when the peer reset the socket. A 101 response, which the binding
// writes itself, is compared header by header.
func normalizeWSRecord(record []string) []string {
	var out []string
	upgraded := len(record) > 0 && strings.Contains(record[0], " 101 ")
	for _, line := range record {
		switch {
		case line == "SEND failed":
			continue
		case line == "END ERR ConnectionReset":
			// Closing a socket with unread data resets it; either way the
			// server closed it.
			line = "END EOF"
		case strings.HasPrefix(line, "STATUS ") && !upgraded:
			_, status, _ := strings.Cut(strings.TrimPrefix(line, "STATUS "), " ")
			line = "STATUS " + status
		case strings.HasPrefix(line, "H ") && !upgraded:
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "H "), ":")
			if name != "content-type" && name != "vary" && !strings.HasPrefix(name, "access-control-") {
				continue
			}
		case strings.HasPrefix(line, "HTTP "):
			line = strings.Replace(line, " -> HTTP/1.1 ", " -> ", 1)
		}
		out = append(out, line)
	}
	if !upgraded {
		// net/http writes headers sorted by name.
		start := 1
		end := start
		for end < len(out) && strings.HasPrefix(out[end], "H ") {
			end++
		}
		if start < end {
			sort.Strings(out[start:end])
		}
	}
	return out
}

func TestWebSocketServerMatchesRustServer(t *testing.T) {
	data, err := os.ReadFile("testdata/websocket_rust_server.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []wsServerCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			agentLines := 0
			for _, line := range c.Rust {
				if strings.HasPrefix(line, "AGENT ") {
					agentLines++
				}
			}
			got := normalizeWSRecord(runWSServerCase(t, c, agentLines))
			want := normalizeWSRecord(c.Rust)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("got\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}

// isConnectionReset reports whether err is a TCP reset. Unix reports
// "connection reset by peer"; Windows reports WSAECONNRESET as "An existing
// connection was forcibly closed by the remote host".
func isConnectionReset(err error) bool {
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	text := err.Error()
	return strings.Contains(text, "connection reset") || strings.Contains(text, "forcibly closed")
}
