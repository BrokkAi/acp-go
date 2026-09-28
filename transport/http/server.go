package acphttp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// initializeTimeout bounds how long one initialize POST waits for the agent's
// response before the connection is torn down.
const initializeTimeout = 30 * time.Second

// Entry serves one ACP connection over newline-delimited JSON streams. Both
// github.com/BrokkAi/acp-go/agent and .../v2/agent runtimes satisfy it, which is
// what "reusing the existing connection framing" means here.
type Entry interface {
	Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error
}

// Server is an http.Handler serving the draft Streamable HTTP binding. Entry is
// required; Path defaults to DefaultPath.
type Server struct {
	Entry Entry
	Path  string

	mu    sync.Mutex
	conns map[string]*serverConn
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Entry == nil {
		http.Error(w, "ACP HTTP server has no agent entry", http.StatusInternalServerError)
		return
	}
	if path := s.path(); r.URL.Path != path {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, r)
	case http.MethodGet:
		s.handleGet(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "ACP HTTP transport serves GET and POST", http.StatusMethodNotAllowed)
	}
}

func (s *Server) path() string {
	if s.Path == "" {
		return DefaultPath
	}
	return s.Path
}

// Close ends every live connection. It is safe to call more than once and does
// not wait for the agent implementations to return.
func (s *Server) Close() error {
	s.mu.Lock()
	connections := make([]*serverConn, 0, len(s.conns))
	for _, connection := range s.conns {
		connections = append(connections, connection)
	}
	s.mu.Unlock()
	for _, connection := range connections {
		connection.close()
	}
	return nil
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(contentType, jsonMIME) {
		http.Error(w, "Content-Type must be "+jsonMIME, http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxFrame+1))
	if err != nil {
		http.Error(w, "read ACP frame: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(body) > maxFrame {
		http.Error(w, "ACP HTTP frame exceeds 8 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	payload := bytes.TrimSpace(body)
	parsed, err := parseFrame(payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	connectionID := strings.TrimSpace(r.Header.Get(HeaderConnectionID))
	if connectionID == "" {
		if !parsed.isInitialize() {
			http.Error(w, "Acp-Connection-Id header is required after initialize", http.StatusBadRequest)
			return
		}
		s.serveInitialize(w, r, payload, parsed)
		return
	}
	connection := s.lookup(connectionID)
	if connection == nil {
		http.Error(w, "unknown ACP connection", http.StatusNotFound)
		return
	}
	if requiresSessionHeader(parsed.Method) && strings.TrimSpace(r.Header.Get(HeaderSessionID)) == "" {
		http.Error(w, "Acp-Session-Id header is required for "+parsed.Method, http.StatusBadRequest)
		return
	}
	if err := connection.writeToAgent(payload); err != nil {
		http.Error(w, "ACP connection is closed", http.StatusGone)
		return
	}
	w.Header().Set(HeaderConnectionID, connection.id)
	w.WriteHeader(http.StatusAccepted)
}

// serveInitialize creates the connection and answers the initialize request
// inline, which is the only response this binding returns in a POST body.
func (s *Server) serveInitialize(w http.ResponseWriter, r *http.Request, payload []byte, parsed frame) {
	connection, err := s.newConnection()
	if err != nil {
		http.Error(w, "start ACP connection: "+err.Error(), http.StatusInternalServerError)
		return
	}
	waiter := connection.waitFor(string(parsed.ID))
	if err := connection.writeToAgent(payload); err != nil {
		connection.close()
		http.Error(w, "ACP connection is closed", http.StatusGone)
		return
	}
	select {
	case response := <-waiter:
		w.Header().Set("Content-Type", jsonMIME)
		w.Header().Set(HeaderConnectionID, connection.id)
		_, _ = w.Write(response)
	case <-connection.done:
		http.Error(w, "ACP connection closed during initialize", http.StatusBadGateway)
	case <-r.Context().Done():
	case <-time.After(initializeTimeout):
		http.Error(w, "ACP initialize timed out", http.StatusGatewayTimeout)
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Accept"), eventStreamMIME) {
		http.Error(w, "Accept must be "+eventStreamMIME, http.StatusNotAcceptable)
		return
	}
	connectionID := strings.TrimSpace(r.Header.Get(HeaderConnectionID))
	if connectionID == "" {
		http.Error(w, "Acp-Connection-Id header is required", http.StatusBadRequest)
		return
	}
	connection := s.lookup(connectionID)
	if connection == nil {
		http.Error(w, "unknown ACP connection", http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "ACP HTTP streaming requires a flushable response", http.StatusInternalServerError)
		return
	}
	stream, unsubscribe, err := connection.subscribe()
	if err != nil {
		http.Error(w, err.Error(), http.StatusGone)
		return
	}
	defer unsubscribe()
	w.Header().Set("Content-Type", eventStreamMIME)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set(HeaderConnectionID, connection.id)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case payload := <-stream:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return
			}
			flusher.Flush()
		case <-connection.done:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) lookup(id string) *serverConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[id]
}

func (s *Server) newConnection() (*serverConn, error) {
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	connection := &serverConn{
		server:  s,
		id:      newConnectionID(),
		cancel:  cancel,
		in:      inWriter,
		out:     outReader,
		done:    make(chan struct{}),
		subs:    make(map[chan []byte]struct{}),
		waiters: make(map[string]chan []byte),
	}
	s.mu.Lock()
	if s.conns == nil {
		s.conns = make(map[string]*serverConn)
	}
	s.conns[connection.id] = connection
	s.mu.Unlock()

	go func() {
		_ = s.Entry.Serve(ctx, inReader, outWriter)
		_ = outWriter.Close()
	}()
	go connection.route()
	return connection, nil
}

func newConnectionID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

// serverConn owns one agent runtime and its subscribers. A background router
// forwards agent output either to the waiter for an inline initialize response
// or to the connection's event streams.
type serverConn struct {
	server *Server
	id     string
	cancel context.CancelFunc
	in     *io.PipeWriter
	out    *io.PipeReader
	done   chan struct{}

	mu       sync.Mutex
	subs     map[chan []byte]struct{}
	waiters  map[string]chan []byte
	closed   bool
	closeErr error
}

func (c *serverConn) waitFor(id string) chan []byte {
	waiter := make(chan []byte, 1)
	c.mu.Lock()
	c.waiters[id] = waiter
	c.mu.Unlock()
	return waiter
}

func (c *serverConn) writeToAgent(payload []byte) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return io.ErrClosedPipe
	}
	_, err := c.in.Write(append(payload, '\n'))
	return err
}

func (c *serverConn) subscribe() (chan []byte, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, nil, fmt.Errorf("ACP connection is closed")
	}
	stream := make(chan []byte, 64)
	c.subs[stream] = struct{}{}
	return stream, func() {
		c.mu.Lock()
		delete(c.subs, stream)
		c.mu.Unlock()
	}, nil
}

func (c *serverConn) route() {
	reader := bufio.NewReader(c.out)
	for {
		line, err := reader.ReadBytes('\n')
		if payload := bytes.TrimSpace(line); len(payload) > 0 {
			c.deliver(payload)
		}
		if err != nil {
			if err != io.EOF && !errors.Is(err, io.ErrClosedPipe) {
				c.finish(err)
				return
			}
			c.finish(nil)
			return
		}
	}
}

func (c *serverConn) deliver(payload []byte) {
	var parsed frame
	_ = json.Unmarshal(payload, &parsed)
	c.mu.Lock()
	if parsed.Method == "" && len(parsed.ID) > 0 {
		if waiter, ok := c.waiters[string(parsed.ID)]; ok {
			delete(c.waiters, string(parsed.ID))
			c.mu.Unlock()
			waiter <- payload
			return
		}
	}
	subscribers := make([]chan []byte, 0, len(c.subs))
	for stream := range c.subs {
		subscribers = append(subscribers, stream)
	}
	c.mu.Unlock()
	for _, stream := range subscribers {
		select {
		case stream <- payload:
		case <-c.done:
			return
		}
	}
}

func (c *serverConn) finish(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.closeErr = err
	close(c.done)
	c.mu.Unlock()

	c.cancel()
	_ = c.in.Close()
	c.server.mu.Lock()
	delete(c.server.conns, c.id)
	c.server.mu.Unlock()
}

func (c *serverConn) close() {
	_ = c.in.Close()
	c.finish(nil)
}
