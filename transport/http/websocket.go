package acphttp

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// This file is a small RFC 6455 codec, enough for the ACP WebSocket binding.
// It follows the tungstenite 0.29/0.30 WebSocket implementation that the Rust
// crate's server (axum) and client (async-tungstenite) use with their default
// configuration: no extensions or subprotocols, 16 MiB frames, 64 MiB
// messages, text frames checked as UTF-8, and the same checks in the same
// order, so a peer that breaks the protocol fails at the same frame with the
// same error text.

const (
	// wsMaxFrame is tungstenite's default max_frame_size: a frame whose
	// header declares a longer payload fails before the payload is read.
	wsMaxFrame = 16 << 20
	// wsMaxMessage is tungstenite's default max_message_size, the most a
	// fragmented message may hold.
	wsMaxMessage = 64 << 20
	// wsMaxControl is the RFC 6455 limit on a control frame's payload.
	wsMaxControl = 125

	// wsReadChunk is how much of a frame's payload is read at a time.
	wsReadChunk = 1 << 20

	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

// Frame opcodes.
const (
	opContinue byte = 0x0
	opText     byte = 0x1
	opBinary   byte = 0x2
	opClose    byte = 0x8
	opPing     byte = 0x9
	opPong     byte = 0xA
)

// wsAccept is the Sec-WebSocket-Accept value for a Sec-WebSocket-Key,
// computed over the key's bytes as they were sent.
func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// wsError is a codec failure. Its text is how tungstenite displays the
// error, which the Rust client puts after "ws recv: " or "ws send: ".
type wsError struct{ text string }

func (e *wsError) Error() string { return e.text }

func wsProtocolError(reason string) error {
	return &wsError{"WebSocket protocol error: " + reason}
}

func wsTooLong(size, max uint64) error {
	return &wsError{fmt.Sprintf("Space limit exceeded: Message too long: %d > %d", size, max)}
}

func wsUTF8Error(reason string) error { return &wsError{"UTF-8 encoding error: " + reason} }

func wsIOError(err error) error { return &wsError{"IO error: " + err.Error()} }

// errWSReset is tungstenite's error for a stream that ends, between frames
// or inside one, without a close frame.
var errWSReset = wsProtocolError("Connection reset without closing handshake")

type wsRole int

const (
	wsServerRole wsRole = iota
	wsClientRole
)

type wsMessageKind int

const (
	wsText wsMessageKind = iota + 1
	wsBinary
	wsPing
	wsPong
	wsClose
)

// wsMessage is one message read from the peer. A close message carries its
// close frame, nil when the close frame had no payload.
type wsMessage struct {
	kind    wsMessageKind
	payload []byte
	close   *wsCloseFrame
}

// wsCloseFrame is the code and reason of a close frame.
type wsCloseFrame struct {
	code   uint16
	reason []byte
}

// wsConn is one WebSocket connection. One goroutine reads; writes are
// serialized, so the reader can answer a ping while another goroutine
// writes messages.
type wsConn struct {
	conn io.ReadWriteCloser
	r    *bufio.Reader
	role wsRole

	wmu sync.Mutex
	// incomplete is the fragmented message being read, if any.
	incomplete *wsIncomplete
}

func newWSConn(conn io.ReadWriteCloser, r *bufio.Reader, role wsRole) *wsConn {
	if r == nil {
		r = bufio.NewReader(conn)
	}
	return &wsConn{conn: conn, r: r, role: role}
}

func (c *wsConn) Close() error { return c.conn.Close() }

// writeMessage writes one unfragmented frame. A client masks it with a fresh
// random key, as RFC 6455 requires; a server does not. The length uses the
// shortest form.
func (c *wsConn) writeMessage(op byte, payload []byte) error {
	frame := make([]byte, 0, len(payload)+14)
	frame = append(frame, 0x80|op)
	var maskBit byte
	if c.role == wsClientRole {
		maskBit = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, maskBit|byte(n))
	case n <= 0xFFFF:
		frame = append(frame, maskBit|126)
		frame = binary.BigEndian.AppendUint16(frame, uint16(n))
	default:
		frame = append(frame, maskBit|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	if c.role == wsClientRole {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return wsIOError(err)
		}
		frame = append(frame, key[:]...)
		start := len(frame)
		frame = append(frame, payload...)
		maskBytes(frame[start:], key)
	} else {
		frame = append(frame, payload...)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := c.conn.Write(frame); err != nil {
		return wsIOError(err)
	}
	return nil
}

func maskBytes(data []byte, key [4]byte) {
	for i := range data {
		data[i] ^= key[i&3]
	}
}

// frameBuffered reports whether the bytes already read hold a whole frame,
// or a frame header that fails as soon as it is read: an invalid opcode or a
// frame over the size limit.
func (c *wsConn) frameBuffered() bool {
	n := c.r.Buffered()
	if n < 2 {
		return false
	}
	data, _ := c.r.Peek(n)
	header := 2
	length := uint64(data[1] & 0x7F)
	switch length {
	case 126:
		header += 2
	case 127:
		header += 8
	}
	if data[1]&0x80 != 0 {
		header += 4
	}
	if n < header {
		return false
	}
	switch length {
	case 126:
		length = uint64(binary.BigEndian.Uint16(data[2:]))
	case 127:
		length = binary.BigEndian.Uint64(data[2:])
	}
	if op := data[0] & 0x0F; (op >= 3 && op <= 7) || op >= 11 || length > wsMaxFrame {
		return true
	}
	return uint64(n-header) >= length
}

// readMessage reads frames until one makes a message, answering each ping
// with a pong as tungstenite does. It fails as tungstenite's read does, with
// the same checks in the same order: an invalid opcode and an oversized frame
// as soon as the header is read, then, once the payload is read, a missing
// or unexpected mask, reserved bits, control frame rules, and fragmentation
// and UTF-8 rules for data frames.
func (c *wsConn) readMessage() (wsMessage, error) {
	for {
		message, ok, err := c.readFrame()
		if err != nil || ok {
			return message, err
		}
	}
}

func (c *wsConn) readFrame() (wsMessage, bool, error) {
	var head [2]byte
	if err := c.readFull(head[:]); err != nil {
		return wsMessage{}, false, err
	}
	fin := head[0]&0x80 != 0
	rsv := head[0] & 0x70
	op := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if err := c.readFull(ext[:]); err != nil {
			return wsMessage{}, false, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if err := c.readFull(ext[:]); err != nil {
			return wsMessage{}, false, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var key [4]byte
	if masked {
		if err := c.readFull(key[:]); err != nil {
			return wsMessage{}, false, err
		}
	}
	if (op >= 3 && op <= 7) || op >= 11 {
		return wsMessage{}, false, wsProtocolError("Encountered invalid opcode: " + strconv.Itoa(int(op)))
	}
	if length > wsMaxFrame {
		return wsMessage{}, false, wsTooLong(length, wsMaxFrame)
	}
	// The payload grows as it arrives, so a peer that declares a large
	// frame and sends it slowly holds only what it has sent.
	payload := make([]byte, 0, min(length, wsReadChunk))
	for uint64(len(payload)) < length {
		chunk := int(min(length-uint64(len(payload)), wsReadChunk))
		start := len(payload)
		payload = append(payload, make([]byte, chunk)...)
		if err := c.readFull(payload[start:]); err != nil {
			return wsMessage{}, false, err
		}
	}
	if c.role == wsServerRole {
		if !masked {
			return wsMessage{}, false, wsProtocolError("Received an unmasked frame from client")
		}
		maskBytes(payload, key)
	}
	if rsv != 0 {
		return wsMessage{}, false, wsProtocolError("Reserved bits are non-zero")
	}
	if c.role == wsClientRole && masked {
		return wsMessage{}, false, wsProtocolError("Received a masked frame from server")
	}
	if op >= opClose {
		return c.controlFrame(op, fin, payload)
	}
	return c.dataFrame(op, fin, payload)
}

// readFull reads exactly len(p) bytes. The stream ending, at a frame
// boundary or inside a frame, is tungstenite's reset error.
func (c *wsConn) readFull(p []byte) error {
	if _, err := io.ReadFull(c.r, p); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errWSReset
		}
		return wsIOError(err)
	}
	return nil
}

func (c *wsConn) controlFrame(op byte, fin bool, payload []byte) (wsMessage, bool, error) {
	switch {
	case !fin:
		return wsMessage{}, false, wsProtocolError("Fragmented control frame")
	case len(payload) > wsMaxControl:
		return wsMessage{}, false, wsProtocolError("Control frame too big (payload must be 125 bytes or less)")
	}
	switch op {
	case opClose:
		frame, err := parseCloseFrame(payload)
		if err != nil {
			return wsMessage{}, false, err
		}
		return wsMessage{kind: wsClose, close: frame}, true, nil
	case opPing:
		if err := c.writeMessage(opPong, payload); err != nil {
			return wsMessage{}, false, err
		}
		return wsMessage{kind: wsPing, payload: payload}, true, nil
	default:
		return wsMessage{kind: wsPong, payload: payload}, true, nil
	}
}

func (c *wsConn) dataFrame(op byte, fin bool, payload []byte) (wsMessage, bool, error) {
	switch {
	case op == opContinue && c.incomplete == nil:
		return wsMessage{}, false, wsProtocolError("Continue frame but nothing to continue")
	case op == opContinue:
		if err := c.incomplete.extend(payload); err != nil {
			return wsMessage{}, false, err
		}
		if !fin {
			return wsMessage{}, false, nil
		}
		incomplete := c.incomplete
		c.incomplete = nil
		message, err := incomplete.complete()
		return message, err == nil, err
	case c.incomplete != nil:
		name := "BINARY"
		if op == opText {
			name = "TEXT"
		}
		return wsMessage{}, false, wsProtocolError("While waiting for more fragments received: " + name)
	case fin:
		if op == opText {
			if text := utf8Error(payload); text != "" {
				return wsMessage{}, false, wsUTF8Error(text)
			}
			return wsMessage{kind: wsText, payload: payload}, true, nil
		}
		return wsMessage{kind: wsBinary, payload: payload}, true, nil
	default:
		incomplete := &wsIncomplete{text: op == opText}
		if err := incomplete.extend(payload); err != nil {
			return wsMessage{}, false, err
		}
		c.incomplete = incomplete
		return wsMessage{}, false, nil
	}
}

// parseCloseFrame reads a close frame's payload as tungstenite does: empty
// is no close frame, one byte is an invalid close sequence, and the reason
// must be UTF-8. A code that may not be sent on the wire becomes 1002 with
// the reason "Protocol violation".
func parseCloseFrame(payload []byte) (*wsCloseFrame, error) {
	switch len(payload) {
	case 0:
		return nil, nil
	case 1:
		return nil, wsProtocolError("Invalid close sequence")
	}
	reason := payload[2:]
	if text := utf8Error(reason); text != "" {
		return nil, wsUTF8Error(text)
	}
	frame := &wsCloseFrame{code: binary.BigEndian.Uint16(payload), reason: reason}
	if _, allowed := closeCodeName(frame.code); !allowed {
		frame = &wsCloseFrame{code: 1002, reason: []byte("Protocol violation")}
	}
	return frame, nil
}

// closeCodeName is how tungstenite's CloseCode debug-prints a code, and
// whether the code may be received.
func closeCodeName(code uint16) (string, bool) {
	names := map[uint16]string{
		1000: "Normal", 1001: "Away", 1002: "Protocol", 1003: "Unsupported",
		1007: "Invalid", 1008: "Policy", 1009: "Size", 1010: "Extension",
		1011: "Error", 1012: "Restart", 1013: "Again",
	}
	if name, ok := names[code]; ok {
		return name, true
	}
	switch {
	case code == 1005:
		return "Status", false
	case code == 1006:
		return "Abnormal", false
	case code == 1015:
		return "Tls", false
	case code >= 1016 && code <= 2999:
		return fmt.Sprintf("Reserved(%d)", code), false
	case code >= 3000 && code <= 3999:
		return fmt.Sprintf("Iana(%d)", code), true
	case code >= 4000 && code <= 4999:
		return fmt.Sprintf("Library(%d)", code), true
	default:
		return fmt.Sprintf("Bad(%d)", code), false
	}
}

// closeFrameDebug is how Rust debug-prints the Option<CloseFrame> of a close
// message, which the Rust client puts in its error for a server's close.
func closeFrameDebug(frame *wsCloseFrame) string {
	if frame == nil {
		return "None"
	}
	name, _ := closeCodeName(frame.code)
	return "Some(CloseFrame { code: " + name + ", reason: Utf8Bytes(" + bytesDebug(frame.reason) + ") })"
}

// bytesDebug is the bytes crate's Debug for a byte string.
func bytesDebug(data []byte) string {
	var out strings.Builder
	out.WriteString(`b"`)
	for _, b := range data {
		switch {
		case b == '\n':
			out.WriteString(`\n`)
		case b == '\r':
			out.WriteString(`\r`)
		case b == '\t':
			out.WriteString(`\t`)
		case b == '\\' || b == '"':
			out.WriteByte('\\')
			out.WriteByte(b)
		case b == 0:
			out.WriteString(`\0`)
		case b >= 0x20 && b < 0x7f:
			out.WriteByte(b)
		default:
			fmt.Fprintf(&out, `\x%02x`, b)
		}
	}
	out.WriteByte('"')
	return out.String()
}

// wsIncomplete collects a fragmented message as tungstenite's
// IncompleteMessage does. A text message is checked as UTF-8 fragment by
// fragment, keeping a sequence cut at a fragment boundary until the next
// fragment completes it.
type wsIncomplete struct {
	text bool
	data []byte
	// pending holds the start of a UTF-8 sequence that the fragments so far
	// cut short, as tungstenite's Incomplete buffer.
	pending    [4]byte
	pendingLen int
	hasPending bool
}

func (m *wsIncomplete) size() int {
	if m.hasPending {
		return len(m.data) + m.pendingLen
	}
	return len(m.data)
}

func (m *wsIncomplete) extend(tail []byte) error {
	mine, portion := uint64(m.size()), uint64(len(tail))
	if mine > wsMaxMessage || portion > wsMaxMessage-mine {
		return wsTooLong(mine+portion, wsMaxMessage)
	}
	if !m.text {
		m.data = append(m.data, tail...)
		return nil
	}
	input := tail
	if m.hasPending {
		m.hasPending = false
		consumed, valid, done := m.tryComplete(input)
		if !done {
			input = nil
			m.hasPending = true
		} else {
			input = input[consumed:]
			completed := m.pending[:m.pendingLen]
			m.pendingLen = 0
			if !valid {
				return wsUTF8Error(lossy(completed))
			}
			m.data = append(m.data, completed...)
		}
	}
	if len(input) == 0 {
		return nil
	}
	validUpTo, errorLen := utf8Prefix(input)
	m.data = append(m.data, input[:validUpTo]...)
	switch {
	case validUpTo == len(input):
		return nil
	case errorLen > 0:
		return wsUTF8Error(lossy(input[validUpTo : validUpTo+errorLen]))
	default:
		rest := input[validUpTo:]
		m.pending = [4]byte{}
		copy(m.pending[:], rest)
		m.pendingLen = len(rest)
		m.hasPending = true
		return nil
	}
}

// tryComplete is tungstenite's Incomplete::try_complete_offsets: it feeds
// input to the pending sequence and reports how much input it took, whether
// the bytes now pending are valid, and whether the sequence is decided.
func (m *wsIncomplete) tryComplete(input []byte) (consumed int, valid, done bool) {
	initial := m.pendingLen
	copied := min(4-initial, len(input))
	copy(m.pending[initial:], input[:copied])
	spliced := m.pending[:initial+copied]
	validUpTo, errorLen := utf8Prefix(spliced)
	switch {
	case validUpTo == len(spliced):
		m.pendingLen = len(spliced)
		return copied, true, true
	case validUpTo > 0:
		m.pendingLen = validUpTo
		return validUpTo - initial, true, true
	case errorLen > 0:
		m.pendingLen = errorLen
		return errorLen - initial, false, true
	default:
		m.pendingLen = len(spliced)
		return copied, false, false
	}
}

func (m *wsIncomplete) complete() (wsMessage, error) {
	if !m.text {
		return wsMessage{kind: wsBinary, payload: m.data}, nil
	}
	if m.hasPending {
		b := m.pending
		return wsMessage{}, wsUTF8Error(fmt.Sprintf("incomplete string: Incomplete { buffer: [%d, %d, %d, %d], buffer_len: %d }", b[0], b[1], b[2], b[3], m.pendingLen))
	}
	return wsMessage{kind: wsText, payload: m.data}, nil
}

// utf8Prefix is Rust's Utf8Error for data: the length of the valid prefix,
// and the length of the invalid sequence after it, 0 when data only ends in
// the middle of a sequence. A valid data returns len(data).
func utf8Prefix(data []byte) (validUpTo, errorLen int) {
	for i := 0; i < len(data); {
		if data[i] < utf8.RuneSelf {
			i++
			continue
		}
		width, invalid := utf8SequenceAt(data[i:])
		switch {
		case invalid < 0:
			return i, 0
		case invalid > 0:
			return i, invalid
		}
		i += width
	}
	return len(data), 0
}

// lossy is String::from_utf8_lossy for one invalid sequence.
func lossy(data []byte) string {
	return strings.ToValidUTF8(string(data), "�")
}
