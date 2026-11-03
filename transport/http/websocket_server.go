package acphttp

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/BrokkAi/acp-go/internal/jsonline"
)

// isWebSocketUpgrade mirrors the checks of axum's WebSocketUpgrade extractor,
// which the Rust crate's GET handler tries before it serves an event stream:
// an HTTP/1.1 GET whose first Connection header contains "upgrade" in any
// case, whose first Upgrade header is "websocket" in any case, with a
// Sec-WebSocket-Key header of any value and a first Sec-WebSocket-Version
// header of "13", on a connection that can be taken over. A request that
// fails any of them is served as an ordinary GET, as in the Rust crate.
func isWebSocketUpgrade(w http.ResponseWriter, r *http.Request) bool {
	if r.ProtoMajor != 1 || r.ProtoMinor != 1 || r.Method != http.MethodGet {
		return false
	}
	connection := r.Header.Values("Connection")
	if len(connection) == 0 || !utf8Valid(connection[0]) || !strings.Contains(asciiLower(connection[0]), "upgrade") {
		return false
	}
	if !firstHeaderIs(r.Header, "Upgrade", "websocket") {
		return false
	}
	if _, ok := r.Header["Sec-Websocket-Key"]; !ok {
		return false
	}
	if !firstHeaderIs(r.Header, "Sec-Websocket-Version", "13") {
		return false
	}
	return hijackable(w)
}

func firstHeaderIs(header http.Header, name, value string) bool {
	values := header.Values(name)
	return len(values) > 0 && asciiEqualFold(values[0], value)
}

// asciiEqualFold compares two strings ignoring ASCII case only, as Rust's
// eq_ignore_ascii_case does.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if asciiLowerByte(a[i]) != asciiLowerByte(b[i]) {
			return false
		}
	}
	return true
}

func asciiLower(s string) string {
	b := []byte(s)
	for i := range b {
		b[i] = asciiLowerByte(b[i])
	}
	return string(b)
}

func asciiLowerByte(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 'a' - 'A'
	}
	return b
}

func utf8Valid(s string) bool { return utf8Error([]byte(s)) == "" }

// hijackable reports whether the response writer, or one it wraps, can hand
// over the connection, as hyper offers an upgrade for an HTTP/1.1 request.
func hijackable(w http.ResponseWriter) bool {
	for {
		if _, ok := w.(http.Hijacker); ok {
			return true
		}
		wrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = wrapper.Unwrap()
	}
}

// handleWebSocket serves a WebSocket upgrade as the Rust crate does. The
// origin policy is checked first: a request with an Origin header the CORS
// policy does not allow gets 403. Otherwise the server answers 101 with the
// new connection's id in Acp-Connection-Id, no subprotocol and no extension,
// and starts one Entry.Serve for the socket.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !s.Options.CORS.allowsOrigin(r.Header) {
		writeText(w, http.StatusForbidden, "WebSocket origin not allowed")
		return
	}
	key := r.Header.Values("Sec-Websocket-Key")[0]
	// hyper reads a request body before it hands the connection over, so the
	// body is never mistaken for frames. It is discarded as it is read, so
	// its size is not bounded, as in the Rust crate.
	if r.ContentLength != 0 {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}
	cors := corsResponseHeaders(w.Header(), s.Options.CORS)
	conn, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		writeText(w, http.StatusInternalServerError, "WebSocket upgrade failed")
		return
	}
	// The server's read and write timeouts are for HTTP exchanges, not for
	// the socket, as hyper's do not apply to an upgraded connection.
	_ = conn.SetDeadline(time.Time{})
	connection := s.newConnection(&outbound{websocket: true, connection: newMailbox()}, nil, nil)
	stream, _ := connection.outbound.leaseConnection()
	var head strings.Builder
	head.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	// hyper adds close to the Connection header of a response to a request
	// that asked to close.
	if connectionTokenClose(r.Header.Values("Connection")) {
		head.WriteString("connection: upgrade, close\r\n")
	} else {
		head.WriteString("connection: upgrade\r\n")
	}
	head.WriteString("upgrade: websocket\r\n")
	head.WriteString("sec-websocket-accept: " + wsAccept(key) + "\r\n")
	head.WriteString("acp-connection-id: " + connection.id + "\r\n")
	for _, field := range cors {
		head.WriteString(field + "\r\n")
	}
	head.WriteString("date: " + time.Now().UTC().Format(http.TimeFormat) + "\r\n\r\n")
	if _, err := buffered.Writer.WriteString(head.String()); err != nil || buffered.Writer.Flush() != nil {
		_ = conn.Close()
		s.discard(connection)
		return
	}
	go s.runWebSocket(connection, stream, newWSConn(conn, buffered.Reader, wsServerRole))
}

// connectionTokenClose reports whether a Connection header lists the token
// close, in any case.
func connectionTokenClose(values []string) bool {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if asciiEqualFold(strings.Trim(part, " \t"), "close") {
				return true
			}
		}
	}
	return false
}

// corsResponseHeaders lists the CORS headers already set for the response,
// in the order the Rust crate's tower-http layer writes them on a 101.
func corsResponseHeaders(header http.Header, cors CORSOptions) []string {
	var names []string
	switch {
	case cors.AllowsAnyOrigin():
		names = []string{"Access-Control-Expose-Headers", "Access-Control-Allow-Origin"}
	case cors.Enabled():
		names = []string{"Vary", "Access-Control-Allow-Origin", "Access-Control-Expose-Headers"}
	}
	var fields []string
	for _, name := range names {
		for _, value := range header.Values(name) {
			fields = append(fields, strings.ToLower(name)+": "+value)
		}
	}
	return fields
}

// runWebSocket carries frames between the socket and the agent until either
// side ends, as the Rust crate's message loop does:
//
//   - A text frame is one JSON-RPC message for the agent, forwarded as the
//     HTTP binding forwards a POST body. A JSON array is answered with one
//     -32600 error and none of its entries is forwarded.
//   - Binary frames and pongs are ignored, and a ping is answered.
//   - A close frame, a protocol violation, or a failed read ends the
//     connection at once and stops the agent. The server does not answer the
//     close frame or send one of its own.
//   - When the agent finishes, or DELETE or Server.Close ends the
//     connection, what the agent sent is delivered and the socket is closed.
//   - When the agent stops reading, the next text frame stops the reading;
//     the socket still delivers what the agent sends until it finishes.
func (s *Server) runWebSocket(connection *serverConn, stream *lease, ws *wsConn) {
	defer stream.release()
	readerDone := make(chan struct{})
	var abort sync.Once
	aborted := make(chan struct{})
	stop := func() { abort.Do(func() { close(aborted) }) }
	go func() {
		defer close(readerDone)
		// Messages are handed to the agent once no further frame is already
		// buffered. The Rust crate queues a message for the agent and goes
		// straight on to the next buffered frame, so a close frame or a
		// protocol violation that arrived with it ends the connection before
		// the agent reads the message; one that arrives later does not.
		var held [][]byte
		for {
			message, err := ws.readMessage()
			if err != nil || message.kind == wsClose {
				stop()
				return
			}
			if message.kind == wsText {
				if jsonline.Classify(message.payload) == jsonline.Batch {
					// JSON-RPC batches are not supported. Answer the array
					// with one Invalid Request error and forward none of its
					// entries.
					if ws.writeMessage(opText, jsonline.BatchRejection()) != nil {
						stop()
						return
					}
				} else {
					held = append(held, agentPayload(message.payload))
				}
			}
			if ws.frameBuffered() {
				continue
			}
			for _, payload := range held {
				// An agent that no longer reads ends the reading but not the
				// delivery of what it still sends.
				if connection.writeToAgent(payload) != nil {
					return
				}
			}
			held = held[:0]
		}
	}()

	send := func() bool {
		for {
			payload, ok := stream.next()
			if !ok {
				return true
			}
			if ws.writeMessage(opText, payload) != nil {
				return false
			}
		}
	}
	finished := false
	for !finished {
		if !send() {
			break
		}
		select {
		case <-stream.box.signal:
		case <-connection.done:
			send()
			finished = true
		case <-aborted:
			finished = true
		}
	}
	_ = ws.Close()
	<-readerDone
	s.discard(connection)
}

// agentPayload is a text frame as the agent reads it, as a POST body is
// forwarded: a JSON-RPC message re-serialized, anything else made one line.
func agentPayload(text []byte) []byte {
	if parsed, ok := parseMessage(text); ok {
		return parsed.encode()
	}
	return lineValue(text)
}
