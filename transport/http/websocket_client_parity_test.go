package acphttp

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
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// testdata/websocket_rust_client.json holds scripted WebSocket servers and
// what the agent-client-protocol-http 5.0.0 HttpClient did against each: a
// Lines client wrote the case's lines through a ws:// HttpClient to a raw TCP
// server that answered the handshake with the case's response and then sent
// the case's bytes. The server recorded the handshake request and the frames
// the client sent, and the probe recorded the lines the client delivered and
// how the transport ended. Each case runs here against the Go transport and
// must produce the same record.
type wsClientCase struct {
	Name     string            `json:"name"`
	Response string            `json:"response"`
	Steps    []json.RawMessage `json:"steps"`
	Lines    []string          `json:"lines"`
	Rust     []string          `json:"rust"`
}

func wsShowBytes(data []byte) string {
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

func wsFrameLines(data []byte) []string {
	var out []string
	for len(data) > 0 {
		if len(data) < 2 {
			return append(out, "PARTIAL "+wsShowBytes(data))
		}
		b0, b1 := data[0], data[1]
		pos, length, form := 2, uint64(b1&0x7f), 7
		switch length {
		case 126:
			length, pos, form = uint64(binary.BigEndian.Uint16(data[2:])), 4, 16
		case 127:
			length, pos, form = binary.BigEndian.Uint64(data[2:]), 10, 64
		}
		masked := b1&0x80 != 0
		var key []byte
		if masked {
			key = data[pos : pos+4]
			pos += 4
		}
		if uint64(len(data)) < uint64(pos)+length {
			return append(out, "PARTIAL "+wsShowBytes(data))
		}
		payload := append([]byte(nil), data[pos:pos+int(length)]...)
		maskBit := 0
		if masked {
			maskBit = 1
			for i := range payload {
				payload[i] ^= key[i%4]
			}
		}
		out = append(out, fmt.Sprintf("FRAME op=%d fin=%d rsv=%d mask=%d len%d=%d %s", b0&0x0f, b0>>7, (b0>>4)&7, maskBit, form, length, wsShowBytes(payload)))
		data = data[pos+int(length):]
	}
	return out
}

// validKey reports a Sec-WebSocket-Key that is the base64 of 16 bytes.
func validKey(key string) bool {
	if len(key) != 24 || !strings.HasSuffix(key, "==") || !strings.ContainsRune("AQgw", rune(key[21])) {
		return false
	}
	for _, c := range key[:22] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/') {
			return false
		}
	}
	return true
}

func runWSClientCase(t *testing.T, c wsClientCase) []string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var mu sync.Mutex
	var log []string
	var received []byte
	end := "OPEN"
	// The scripted bytes are built before the client connects, so a large
	// frame is sent as promptly as the Rust probe sent it.
	type action struct {
		data     []byte
		sleep    time.Duration
		shutdown bool
	}
	var actions []action
	for _, raw := range c.Steps {
		var step struct {
			Send     string `json:"send"`
			Sleep    int    `json:"sleep"`
			Shutdown bool   `json:"shutdown"`
			Big      *struct {
				Prefix, Fill, Suffix string
				Count                int
			} `json:"big"`
		}
		if err := json.Unmarshal(raw, &step); err != nil {
			t.Fatal(err)
		}
		switch {
		case step.Send != "":
			data, _ := hex.DecodeString(step.Send)
			actions = append(actions, action{data: data})
		case step.Big != nil:
			payload := step.Big.Prefix + strings.Repeat(step.Big.Fill, step.Big.Count) + step.Big.Suffix
			data := binary.BigEndian.AppendUint64([]byte{0x81, 127}, uint64(len(payload)))
			actions = append(actions, action{data: append(data, payload...)})
		case step.Sleep > 0:
			actions = append(actions, action{sleep: time.Duration(step.Sleep) * time.Millisecond})
		case step.Shutdown:
			actions = append(actions, action{shutdown: true})
		}
	}
	serverDone := make(chan struct{})
	stopServer := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		key := ""
		for first := true; ; first = false {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSuffix(line, "\r\n")
			if line == "" {
				break
			}
			mu.Lock()
			if first {
				log = append(log, "REQ "+line)
			} else {
				name, value, _ := strings.Cut(line, ":")
				value = strings.TrimSpace(value)
				if strings.EqualFold(name, "sec-websocket-key") {
					key = value
					value = fmt.Sprintf("<key ok=%t>", validKey(value))
				}
				log = append(log, "REQH "+strings.ToLower(name)+": "+value)
			}
			mu.Unlock()
		}
		if _, err := io.WriteString(conn, strings.ReplaceAll(c.Response, "{accept}", wsAccept(key))); err != nil {
			return
		}
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			buf := make([]byte, 1<<20)
			for {
				n, err := reader.Read(buf)
				mu.Lock()
				received = append(received, buf[:n]...)
				switch {
				case errors.Is(err, io.EOF):
					end = "EOF"
				case err != nil && isConnectionReset(err):
					end = "ERR ConnectionReset"
				case err != nil && !errors.Is(err, net.ErrClosed):
					end = "ERR " + err.Error()
				}
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
		for _, step := range actions {
			switch {
			case step.data != nil:
				if _, err := conn.Write(step.data); err != nil {
					mu.Lock()
					log = append(log, "SEND failed")
					mu.Unlock()
				}
			case step.shutdown:
				_ = conn.(*net.TCPConn).CloseWrite()
			default:
				time.Sleep(step.sleep)
			}
		}
		<-stopServer
		_ = conn.Close()
		<-readDone
	}()

	var delivered []string
	result := ""
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws, err := websocketHandshake(ctx, "ws://"+listener.Addr().String()+DefaultPath, http.DefaultClient)
	var transport *wsTransport
	readDone := make(chan struct{})
	var readErr error
	if err != nil {
		result = "Err Internal error: " + debugString(err.Error())
		close(readDone)
	} else {
		reader, writer := io.Pipe()
		transport = &wsTransport{ws: ws, feed: writer, stopWatch: func() bool { return true }}
		go transport.read()
		go func() {
			defer close(readDone)
			lines := bufio.NewReader(reader)
			for {
				line, err := lines.ReadString('\n')
				if err != nil {
					if !errors.Is(err, io.EOF) {
						readErr = err
					}
					return
				}
				delivered = append(delivered, strings.TrimSuffix(line, "\n"))
			}
		}()
		var failure error
		time.Sleep(100 * time.Millisecond)
		for _, line := range c.Lines {
			if _, err := transport.Write([]byte(line + "\n")); err != nil && failure == nil {
				failure = err
			}
			time.Sleep(150 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
		if failure == nil {
			select {
			case <-readDone:
				failure = readErr
			default:
			}
		}
		if failure != nil {
			result = "Err Internal error: " + debugString(failure.Error())
		} else {
			result = "Ok after close"
		}
		_ = transport.Close()
	}
	<-readDone
	time.Sleep(200 * time.Millisecond)
	close(stopServer)
	<-serverDone
	mu.Lock()
	out := append([]string(nil), log...)
	out = append(out, wsFrameLines(received)...)
	out = append(out, "END "+end)
	mu.Unlock()
	for _, line := range delivered {
		out = append(out, "DELIVERED "+wsShowBytes([]byte(line)))
	}
	return append(out, "RESULT "+result)
}

// normalizeWSClientRecord keeps the request line and the handshake headers
// but Host, whose port differs, sorted, and drops failed writes of
// the scripted server, which depend on when the client reset the socket, and
// whether the client's close of the socket came as a reset.
func normalizeWSClientRecord(record []string) []string {
	var headers, out []string
	for _, line := range record {
		switch {
		case line == "SEND failed":
		case line == "END ERR ConnectionReset":
			// Closing a socket with unread data resets it; either way the
			// client closed it.
			out = append(out, "END EOF")
		case strings.HasPrefix(line, "REQH "):
			name, _, _ := strings.Cut(strings.TrimPrefix(line, "REQH "), ":")
			if name != "host" {
				headers = append(headers, line)
			}
		default:
			out = append(out, line)
		}
	}
	sort.Strings(headers)
	if len(out) > 0 && strings.HasPrefix(out[0], "REQ ") {
		return append(append([]string{out[0]}, headers...), out[1:]...)
	}
	return append(headers, out...)
}

// serverFailedFirst reports a Rust record in which the scripted server ended
// the transport, with a protocol violation, a close frame or the end of the
// socket, before the client sent any of the case's lines. In those cases the
// server's bytes go out 50 ms after the 101 and the probe writes its first
// line 100 ms after connecting, and the Rust driver's reader and writer are
// independent futures raced by futures::future::select, so whether a line
// reaches the wire before the reader fails the transport is decided by that
// margin alone: run with the server's bytes sent after the first line, the
// Rust client sends the line's text frame and ends with the same error. The
// Rust probe never lost the race (hundreds of runs, idle and under CPU
// load), but the Go transport, whose reader is a goroutine, can under load.
func serverFailedFirst(want []string) bool {
	for _, line := range want {
		if strings.HasPrefix(line, "FRAME op=1 ") {
			return false
		}
	}
	result := want[len(want)-1]
	return strings.HasPrefix(result, `RESULT Err Internal error: "ws recv: `) ||
		strings.HasPrefix(result, `RESULT Err Internal error: "WebSocket closed by peer: `)
}

// dropRacedLineFrames removes from got the text frames of a leading run of
// lines, in order, that the client may have sent before the server's failure
// reached its reader (see serverFailedFirst). Every other frame, and the
// order of what is left, is still compared.
func dropRacedLineFrames(got, lines []string) []string {
	out := make([]string, 0, len(got))
	next := 0
	for _, line := range got {
		if next < len(lines) && line == wsClientTextFrameLine(lines[next]) {
			next++
			continue
		}
		out = append(out, line)
	}
	return out
}

// wsClientTextFrameLine is the record line of the masked text frame the Go
// transport sends for line.
func wsClientTextFrameLine(line string) string {
	payload := []byte(line)
	if parsed, ok := parseMessage(payload); ok {
		payload = parsed.encode()
	}
	form := 7
	switch {
	case len(payload) > 0xffff:
		form = 64
	case len(payload) > 125:
		form = 16
	}
	return fmt.Sprintf("FRAME op=1 fin=1 rsv=0 mask=1 len%d=%d %s", form, len(payload), wsShowBytes(payload))
}

func TestWebSocketClientMatchesRustClient(t *testing.T) {
	data, err := os.ReadFile("testdata/websocket_rust_client.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []wsClientCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			got := normalizeWSClientRecord(runWSClientCase(t, c))
			want := normalizeWSClientRecord(c.Rust)
			if serverFailedFirst(want) {
				got = dropRacedLineFrames(got, c.Lines)
			}
			if !bytes.Equal([]byte(strings.Join(got, "\n")), []byte(strings.Join(want, "\n"))) {
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
