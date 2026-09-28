package acptest

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"time"

	"github.com/BrokkAi/acp-go"
)

// Pair is one in-memory duplex ACP link. Each endpoint satisfies the streams
// acp.Connect expects, and frames keep the release's newline-delimited JSON
// framing.
type Pair struct {
	A *Endpoint
	B *Endpoint
}

// NewPair returns a connected pair without an I/O deadline.
func NewPair() *Pair {
	a, b := net.Pipe()
	return &Pair{A: newEndpoint(a), B: newEndpoint(b)}
}

// NewPairWithDeadline is NewPair with a deadline applied to both endpoints, so
// a stuck test fails instead of blocking forever.
func NewPairWithDeadline(deadline time.Time) *Pair {
	pair := NewPair()
	_ = pair.A.conn.SetDeadline(deadline)
	_ = pair.B.conn.SetDeadline(deadline)
	return pair
}

// Close closes both endpoints.
func (p *Pair) Close() {
	_ = p.A.Close()
	_ = p.B.Close()
}

// Endpoint is one side of a Pair. It is an io.ReadCloser and an io.WriteCloser,
// so it can back both halves of an acp.Connection, and it can also encode and
// decode individual frames.
type Endpoint struct {
	conn    net.Conn
	reader  *bufio.Reader
	decoder *json.Decoder
	encoder *json.Encoder
}

func newEndpoint(conn net.Conn) *Endpoint {
	reader := bufio.NewReader(conn)
	return &Endpoint{
		conn:    conn,
		reader:  reader,
		decoder: json.NewDecoder(reader),
		encoder: json.NewEncoder(conn),
	}
}

// Read implements io.Reader over the raw inbound stream.
func (e *Endpoint) Read(p []byte) (int, error) { return e.conn.Read(p) }

// Write implements io.Writer over the raw outbound stream.
func (e *Endpoint) Write(p []byte) (int, error) { return e.conn.Write(p) }

// Close closes the endpoint.
func (e *Endpoint) Close() error { return e.conn.Close() }

// Connect runs one ACP connection over this endpoint using the standard
// transport framing.
func (e *Endpoint) Connect(h acp.Handler, n acp.Notifications) *acp.Connection {
	return acp.Connect(e.conn, e.conn, h, n)
}

// Decode reads one JSON frame from the endpoint.
func (e *Endpoint) Decode(value any) error { return e.decoder.Decode(value) }

// Encode writes one newline-terminated JSON frame to the endpoint.
func (e *Endpoint) Encode(value any) error { return e.encoder.Encode(value) }

// Reader exposes the raw inbound stream.
func (e *Endpoint) Reader() io.ReadCloser { return e.conn }

// Writer exposes the raw outbound stream.
func (e *Endpoint) Writer() io.WriteCloser { return e.conn }
