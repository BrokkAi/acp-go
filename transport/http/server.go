package acphttp

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/BrokkAi/acp-go/internal/jsonline"
	"github.com/BrokkAi/acp-go/internal/lines"
)

// keepAliveInterval is how long an idle event stream waits before it sends an
// empty SSE comment, as the Rust crate does.
const keepAliveInterval = 15 * time.Second

// Entry serves one ACP connection over newline-delimited JSON streams. Both
// github.com/BrokkAi/acp-go/agent and .../v2/agent runtimes satisfy it, which is
// what "reusing the existing connection framing" means here.
type Entry interface {
	Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error
}

// Server is an http.Handler serving the draft Streamable HTTP binding. Entry is
// required; the zero Options are the defaults. Each initialize POST starts one
// Entry.Serve call, which runs until the agent returns, the client sends
// DELETE, or Close is called.
//
// The agent's frames are buffered per stream until the client opens it. A
// session-scoped GET (with Acp-Session-Id) opens the stream of a session the
// agent has named on that connection: in a session/new or session/fork
// result, in a result for a request scoped to the session, or in a request or
// notification it sent. A client may also open up to 16 streams for sessions
// not named yet, such as one it is about to session/load; those are dropped
// when the client closes them before the agent names the session. Any other
// session gets 404. A successful session/close or session/delete forgets the
// session.
type Server struct {
	Entry   Entry
	Options ServerOptions

	mu    sync.Mutex
	conns map[string]*serverConn
}

// NewServer returns a Server for entry, refusing a nil entry and options that
// fail Validate.
func NewServer(entry Entry, options ServerOptions) (*Server, error) {
	if entry == nil {
		return nil, errors.New("ACP HTTP server needs an agent entry")
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	return &Server{Entry: entry, Options: options}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.Entry == nil {
		writeText(w, http.StatusInternalServerError, "ACP HTTP server has no agent entry")
		return
	}
	if err := s.Options.Validate(); err != nil {
		writeText(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.Options.CORS.applyCORS(w, r) {
		return
	}
	switch {
	case r.URL.Path == s.Options.path():
		switch r.Method {
		case http.MethodPost:
			s.handlePost(w, r)
		case http.MethodGet, http.MethodHead:
			s.handleGet(w, r)
		case http.MethodDelete:
			s.handleDelete(w, r)
		default:
			w.Header().Set("Allow", "POST,GET,HEAD,DELETE")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	case !s.Options.DisableHealth && r.URL.Path == DefaultHealthPath:
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			writeText(w, http.StatusOK, "ok")
		default:
			w.Header().Set("Allow", "GET,HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// Close ends every live connection. It is safe to call more than once and does
// not wait for the agent implementations to return.
func (s *Server) Close() error {
	s.mu.Lock()
	connections := make([]*serverConn, 0, len(s.conns))
	for id, connection := range s.conns {
		connections = append(connections, connection)
		delete(s.conns, id)
	}
	s.mu.Unlock()
	for _, connection := range connections {
		connection.shutdown()
	}
	return nil
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), jsonMIME) {
		writeText(w, http.StatusUnsupportedMediaType, "Content-Type must be "+jsonMIME)
		return
	}
	connectionID, hasConnection := headerValue(r.Header, HeaderConnectionID)
	sessionID, hasSession := headerValue(r.Header, HeaderSessionID)
	limit := s.Options.maxBodyBytes()
	if r.ContentLength > limit {
		writeText(w, http.StatusRequestEntityTooLarge, "POST body too large")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if int64(len(body)) > limit {
		writeText(w, http.StatusRequestEntityTooLarge, "POST body too large")
		return
	}
	if text := utf8Error(body); text != "" {
		writeText(w, http.StatusBadRequest, "Invalid JSON-RPC: "+text)
		return
	}
	if jsonline.Classify(body) == jsonline.Batch {
		// JSON-RPC batches are not supported. Answer the array with one
		// Invalid Request error, forward none of its entries, and create no
		// connection, whatever the headers say.
		w.Header().Set("Content-Type", jsonMIME)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(jsonline.BatchRejection())
		return
	}
	// A body that is not a JSON-RPC message, valid JSON or not, is forwarded
	// to the agent as the Rust crate forwards its malformed frames: the agent
	// answers it on the connection stream while the POST gets 202. A message
	// is forwarded as the Rust crate re-serializes it.
	parsed, isMessage := parseMessage(body)
	// An initialize request always opens a new connection, as in the Rust
	// crate, whatever Acp-Connection-Id says.
	if isMessage && parsed.isInitialize() {
		s.serveInitialize(w, r, parsed)
		return
	}
	if !hasConnection {
		writeText(w, http.StatusBadRequest, "Acp-Connection-Id header required")
		return
	}
	connection := s.lookup(connectionID)
	if connection == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// A POST naming a session does not make it known: only the agent does
	// that, so a client cannot allocate session streams for made-up session
	// ids. A frame that is not a message carries no route.
	var payload []byte
	if !isMessage {
		payload = lineValue(body)
	} else {
		if parsed.hasMethod() {
			if hasSession {
				var err error
				if parsed, err = applySessionHeader(parsed, sessionID); err != nil {
					writeText(w, http.StatusBadRequest, err.Error())
					return
				}
			}
			route := pendingRoute{effect: effectFor(parsed.Method)}
			if session, ok := parsed.sessionID(); ok {
				route.session, route.scoped = session, true
			} else if requiresSessionHeader(parsed.Method) {
				writeText(w, http.StatusBadRequest, "Acp-Session-Id header required")
				return
			}
			if parsed.isRequest() {
				connection.outbound.recordPending(parsed.ID, route)
			}
		}
		payload = parsed.encode()
	}
	if err := connection.writeToAgent(payload); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// initializeAnswer is the agent's response to the initialize request, as the
// Rust crate serializes it, and whether it is an error response.
type initializeAnswer struct {
	payload []byte
	failed  bool
}

// serveInitialize creates the connection and answers the initialize request
// inline, which is the only response this binding returns in a POST body.
// Frames the agent sends before that response are buffered for the streams.
// An error response rejects the connection: the client gets the error with no
// connection id, and the agent is shut down.
func (s *Server) serveInitialize(w http.ResponseWriter, r *http.Request, request frame) {
	connection := s.newConnection(request.ID, request.encode())
	var answer initializeAnswer
	select {
	case answer = <-connection.initialized:
	case <-connection.done:
		// The router hands over the response before it finishes.
		select {
		case answer = <-connection.initialized:
		default:
			s.discard(connection)
			writeText(w, http.StatusInternalServerError, "agent closed before initialize response")
			return
		}
	case <-r.Context().Done():
		s.discard(connection)
		return
	}
	if answer.failed {
		s.discard(connection)
		w.Header().Set("Content-Type", jsonMIME)
		_, _ = w.Write(answer.payload)
		return
	}
	w.Header().Set("Content-Type", jsonMIME)
	w.Header().Set(HeaderConnectionID, connection.id)
	_, _ = w.Write(answer.payload)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.Header.Get("Accept"), eventStreamMIME) {
		writeText(w, http.StatusNotAcceptable, "client must accept "+eventStreamMIME)
		return
	}
	connectionID, ok := headerValue(r.Header, HeaderConnectionID)
	if !ok {
		writeText(w, http.StatusBadRequest, "Acp-Connection-Id header required")
		return
	}
	connection := s.lookup(connectionID)
	if connection == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeText(w, http.StatusInternalServerError, "ACP HTTP streaming requires a flushable response")
		return
	}
	sessionID, scoped := headerValue(r.Header, HeaderSessionID)
	var stream *lease
	var err error
	if scoped {
		stream, err = connection.outbound.leaseSession(sessionID)
	} else {
		stream, err = connection.outbound.leaseConnection()
	}
	switch {
	case errors.Is(err, errStreamBusy):
		writeText(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		writeText(w, http.StatusNotFound, err.Error())
		return
	}
	defer stream.release()
	w.Header().Set("Content-Type", eventStreamMIME)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set(HeaderConnectionID, connection.id)
	if scoped {
		w.Header().Set(HeaderSessionID, sessionID)
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	if r.Method == http.MethodHead {
		return
	}

	keepAlive := time.NewTimer(keepAliveInterval)
	defer keepAlive.Stop()
	// send writes the queued frames one at a time. A frame is only taken off
	// the queue when it is about to be written, so a failed write loses at
	// most that frame and the rest stay queued for the next subscriber.
	send := func() bool {
		for {
			payload, ok := stream.next()
			if !ok {
				return true
			}
			if _, err := w.Write(sseEvent(payload)); err != nil {
				return false
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return false
			}
			keepAlive.Reset(keepAliveInterval)
		}
	}
	for {
		if !send() {
			return
		}
		select {
		case <-stream.box.signal:
		case <-connection.done:
			// Deliver what was queued before the connection closed.
			send()
			return
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			if _, err := io.WriteString(w, ": \n\n"); err != nil {
				return
			}
			flusher.Flush()
			keepAlive.Reset(keepAliveInterval)
		}
	}
}

// handleDelete tears a connection down: its agent is stopped and its streams
// end after delivering what is already queued.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	connectionID, ok := headerValue(r.Header, HeaderConnectionID)
	if !ok {
		writeText(w, http.StatusBadRequest, "Acp-Connection-Id header required")
		return
	}
	s.mu.Lock()
	connection := s.conns[connectionID]
	delete(s.conns, connectionID)
	s.mu.Unlock()
	if connection == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	connection.shutdown()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) lookup(id string) *serverConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[id]
}

// discard unregisters a connection and shuts it down.
func (s *Server) discard(connection *serverConn) {
	s.forget(connection)
	connection.shutdown()
}

func (s *Server) forget(connection *serverConn) {
	s.mu.Lock()
	if s.conns[connection.id] == connection {
		delete(s.conns, connection.id)
	}
	s.mu.Unlock()
}

// newConnection registers a connection and starts its agent with the
// initialize request already queued, so the agent reads it before it can have
// returned, as a Rust agent is spawned with the request in its channel.
func (s *Server) newConnection(initializeID, initialize []byte) *serverConn {
	outReader, outWriter := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	connection := &serverConn{
		server:       s,
		id:           newConnectionID(),
		cancel:       cancel,
		out:          outReader,
		done:         make(chan struct{}),
		outbound:     newOutbound(),
		initializeID: append([]byte(nil), initializeID...),
		initialized:  make(chan initializeAnswer, 1),
		inboxSignal:  make(chan struct{}, 1),
		inbox:        [][]byte{append(append([]byte(nil), initialize...), '\n')},
	}
	s.mu.Lock()
	if s.conns == nil {
		s.conns = make(map[string]*serverConn)
	}
	s.conns[connection.id] = connection
	s.mu.Unlock()

	input := &agentInput{conn: connection}
	go func() {
		_ = s.Entry.Serve(ctx, input, outWriter)
		// An agent that has returned no longer reads, like a Rust agent
		// dropping its channel.
		_ = input.Close()
		_ = outWriter.Close()
	}()
	go connection.route()
	return connection
}

// newConnectionID returns a random UUID v4, the Rust crate's id format.
func newConnectionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("ACP HTTP connection id: %v", err))
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// serverConn owns one agent runtime and its streams. A background router
// hands the initialize response to the waiting POST and queues every other
// agent frame on the stream it belongs to.
type serverConn struct {
	server   *Server
	id       string
	cancel   context.CancelFunc
	out      *io.PipeReader
	outbound *outbound
	// done is closed when the connection ends; open streams then deliver
	// what is queued and finish.
	done chan struct{}

	// initializeID is the id of the initialize request; the router hands the
	// first valid response with that id to initialized.
	initializeID []byte
	initialized  chan initializeAnswer

	mu     sync.Mutex
	closed bool
	// agentGone records that the agent closed its input or returned. Later
	// frames are refused, as the Rust crate's channel send fails once the
	// agent drops its receiver.
	agentGone bool
	// inbox queues client frames for the agent without a bound, as the Rust
	// crate's channel does, so a POST never waits for the agent to read.
	inbox       [][]byte
	inboxSignal chan struct{}
}

// writeToAgent queues a client frame for the agent. It fails once the
// connection has ended or the agent has stopped reading.
func (c *serverConn) writeToAgent(payload []byte) error {
	c.mu.Lock()
	if c.closed || c.agentGone {
		c.mu.Unlock()
		return io.ErrClosedPipe
	}
	c.inbox = append(c.inbox, append(append([]byte(nil), payload...), '\n'))
	c.mu.Unlock()
	select {
	case c.inboxSignal <- struct{}{}:
	default:
	}
	return nil
}

// agentInput is the agent's input stream. It reads the queued client frames
// in order; closing it, or the agent returning, makes later POSTs fail.
type agentInput struct {
	conn    *serverConn
	pending []byte
}

func (in *agentInput) Read(p []byte) (int, error) {
	c := in.conn
	for len(in.pending) == 0 {
		c.mu.Lock()
		switch {
		case c.closed || c.agentGone:
			c.mu.Unlock()
			return 0, io.EOF
		case len(c.inbox) > 0:
			in.pending = c.inbox[0]
			c.inbox[0] = nil
			c.inbox = c.inbox[1:]
			c.mu.Unlock()
			continue
		}
		c.mu.Unlock()
		select {
		case <-c.inboxSignal:
		case <-c.done:
		}
	}
	n := copy(p, in.pending)
	in.pending = in.pending[n:]
	return n, nil
}

// Close stops the agent's input: queued frames are dropped and later POSTs
// get 500.
func (in *agentInput) Close() error {
	c := in.conn
	c.mu.Lock()
	c.agentGone = true
	c.inbox = nil
	c.mu.Unlock()
	select {
	case c.inboxSignal <- struct{}{}:
	default:
	}
	return nil
}

// route reads the agent's lines as the Rust SDK's line reader does, a line
// ending at "\n" or "\r\n", and handles each as the Rust crate handles the
// agent's frames. A JSON-RPC message is re-serialized, and the first valid
// response to the initialize request goes to the waiting POST. Anything else
// the agent writes, an empty or blank line included, is a malformed frame and
// goes to the connection stream verbatim.
func (c *serverConn) route() {
	reader := bufio.NewReader(c.out)
	initializing := true
	for {
		line, err := lines.ReadLine(reader)
		if err == nil {
			if !utf8.Valid(line) {
				// The Rust line reader fails on a line that is not UTF-8,
				// which ends the agent's transport as if it had returned:
				// what it sent before is delivered, the streams end, the
				// connection is gone, and nothing more is read from the agent.
				c.server.forget(c)
				c.finish()
				_ = c.out.CloseWithError(io.ErrClosedPipe)
				return
			}
			parsed, isMessage := parseMessage(line)
			switch {
			case initializing && isMessage && c.isInitializeResponse(parsed):
				initializing = false
				c.initialized <- initializeAnswer{payload: parsed.encode(), failed: parsed.Error != nil}
			case c.isClosed():
			case isMessage:
				c.outbound.route(parsed, parsed.encode())
			default:
				c.outbound.routeMalformed(line)
			}
		}
		if err != nil {
			// The agent has finished and every frame it wrote is queued:
			// unregister the connection, then end its streams.
			c.server.forget(c)
			c.finish()
			return
		}
	}
}

// isInitializeResponse mirrors the Rust crate's initialize_response_failed
// finding an answer: a response, result or error, whose id is the initialize
// request's.
func (c *serverConn) isInitializeResponse(parsed frame) bool {
	if !parsed.isResponse() {
		return false
	}
	want, wantOK := responseKey(c.initializeID)
	got, gotOK := responseKey(parsed.ID)
	return wantOK && gotOK && want == got
}

func (c *serverConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// finish marks the connection closed and ends its streams.
func (c *serverConn) finish() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	close(c.done)
	c.mu.Unlock()
	c.cancel()
}

// shutdown is the abortive teardown for DELETE and Server.Close: streams end
// after delivering what is queued, and the agent is cancelled.
func (c *serverConn) shutdown() {
	c.finish()
	_ = c.out.CloseWithError(io.ErrClosedPipe)
}

// headerValue reads the first value of a header the way the Rust crate's
// HeaderValue::to_str does: a value with bytes outside visible ASCII, space,
// and tab counts as absent.
func headerValue(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	if len(values) == 0 {
		return "", false
	}
	value := values[0]
	for i := 0; i < len(value); i++ {
		if b := value[i]; b != '\t' && (b < 0x20 || b >= 0x7f) {
			return "", false
		}
	}
	return value, true
}

// writeText answers with a plain-text body, as axum does for a string.
func writeText(w http.ResponseWriter, status int, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, text)
}

// sseEvent is the event the Rust crate's axum Event::data writes for a frame:
// the frame in data fields, a new field after each "\r" or "\n" in it, and a
// blank line. Empty data writes no field, so an empty frame is a blank line
// alone.
func sseEvent(payload []byte) []byte {
	if len(payload) == 0 {
		return []byte("\n")
	}
	event := make([]byte, 0, len(payload)+len("data: \n\n"))
	event = append(event, "data: "...)
	for _, b := range payload {
		event = append(event, b)
		if b == '\r' || b == '\n' {
			event = append(event, "data: "...)
		}
	}
	return append(event, "\n\n"...)
}
