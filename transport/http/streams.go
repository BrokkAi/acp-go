package acphttp

import (
	"encoding/json"
	"errors"
	"sync"
)

// maxUnconfirmedSessionStreams is the most session streams a client may hold
// open for sessions the connection does not know yet, as in the Rust crate. A
// client may open a session stream before the session/load or session/resume
// request that makes the session known, so the stream sees the replayed
// updates. Beyond this limit, a GET for an unknown session gets 404.
const maxUnconfirmedSessionStreams = 16

var (
	// errStreamBusy means another subscriber already holds the stream.
	errStreamBusy = errors.New("outbound stream already has a subscriber")
	// errUnknownSession means the session is unknown to the connection and no
	// unconfirmed stream slot is free.
	errUnknownSession = errors.New("unknown Acp-Session-Id")
)

// mailbox is an unbounded, ordered queue of agent frames for one stream. It
// buffers frames until a subscriber leases it, and keeps what a departed
// subscriber did not read for the next one. At most one subscriber holds it.
type mailbox struct {
	queue  [][]byte
	leased bool
	// signal wakes the subscriber after a push; it holds at most one wakeup.
	signal chan struct{}
}

func newMailbox() *mailbox { return &mailbox{signal: make(chan struct{}, 1)} }

// sessionStream is the stream of one session on a connection.
type sessionStream struct {
	box *mailbox
	// confirmed records that the agent has named the session on this
	// connection: in a session/new or session/fork result, in a result for a
	// request scoped to it, or in a request or notification it sent.
	confirmed bool
}

// sessionEffect is what a successful response does to the known sessions.
type sessionEffect int

const (
	effectNone sessionEffect = iota
	// effectOpens: the result names a new session in sessionId.
	effectOpens
	// effectCloses: the result ends the session the request is scoped to.
	effectCloses
)

func effectFor(method string) sessionEffect {
	switch method {
	case "session/new", "session/fork":
		return effectOpens
	case "session/close", "session/delete":
		return effectCloses
	default:
		return effectNone
	}
}

// pendingRoute is how the response to a client request is routed.
type pendingRoute struct {
	// session is the session the request is scoped to; empty with scoped
	// false routes to the connection stream.
	session string
	scoped  bool
	effect  sessionEffect
}

// outbound routes agent frames to the connection stream and the session
// streams of one HTTP connection, following the Rust crate's HttpOutbound.
// A WebSocket connection has no session streams: like the Rust crate's
// WebSocketOutbound, it queues every agent frame on the connection mailbox,
// which the socket holds for its whole life.
type outbound struct {
	mu         sync.Mutex
	websocket  bool
	connection *mailbox
	sessions   map[string]*sessionStream
	pending    map[string][]pendingRoute
}

func newOutbound() *outbound {
	return &outbound{
		connection: newMailbox(),
		sessions:   make(map[string]*sessionStream),
		pending:    make(map[string][]pendingRoute),
	}
}

// recordPending remembers how to route the response to a client request.
// Requests with a null id have no response to route.
func (o *outbound) recordPending(id json.RawMessage, route pendingRoute) {
	key, ok := idKey(id)
	if !ok || o.websocket {
		return
	}
	o.mu.Lock()
	o.pending[key] = append(o.pending[key], route)
	o.mu.Unlock()
}

// route queues one agent message, payload being its serialization, on the
// stream it belongs to.
func (o *outbound) route(parsed frame, payload []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	target := o.connection
	switch {
	case o.websocket:
	case parsed.hasMethod():
		// The agent names the session, so it exists on this connection.
		if sessionID, ok := parsed.sessionID(); ok {
			target = o.confirm(sessionID)
		}
	case parsed.isResponse():
		if box := o.responseTarget(parsed); box != nil {
			target = box
		}
	}
	target.push(payload)
}

// routeMalformed queues an agent frame that is not a JSON-RPC message on the
// connection stream verbatim, as the Rust crate does.
func (o *outbound) routeMalformed(raw []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.connection.push(raw)
}

// responseTarget updates the known sessions for a response and picks its
// session stream. Nil routes the response to the connection stream: a
// response scoped to a session with no stream goes there too, so a POST naming
// an unknown session allocates nothing and its error still reaches the client.
func (o *outbound) responseTarget(response frame) *mailbox {
	key, ok := idKey(response.ID)
	if !ok {
		return nil
	}
	routes := o.pending[key]
	if len(routes) == 0 {
		return nil
	}
	pending := routes[0]
	if len(routes) == 1 {
		delete(o.pending, key)
	} else {
		o.pending[key] = routes[1:]
	}
	succeeded := response.Error == nil

	// Record a created session before its response is queued, so a GET sent
	// right after the client reads the response finds it.
	if succeeded && pending.effect == effectOpens {
		if sessionID, ok := stringMember(response.Result, "sessionId"); ok {
			o.confirm(sessionID)
		}
	}
	if !pending.scoped {
		return nil
	}
	var target *mailbox
	if stream, ok := o.sessions[pending.session]; ok {
		target = stream.box
	}
	if succeeded {
		if pending.effect == effectCloses {
			// A queued response would be lost with the removed stream, so it
			// goes to the connection stream unless a client is reading the
			// session stream.
			if target != nil && !target.leased {
				target = nil
			}
			delete(o.sessions, pending.session)
		} else {
			o.confirm(pending.session)
		}
	}
	return target
}

// confirm marks a session as known, creating its stream if needed. The caller
// holds o.mu.
func (o *outbound) confirm(sessionID string) *mailbox {
	stream, ok := o.sessions[sessionID]
	if !ok {
		stream = &sessionStream{box: newMailbox()}
		o.sessions[sessionID] = stream
	}
	stream.confirmed = true
	return stream.box
}

func (o *outbound) unconfirmed() int {
	count := 0
	for _, stream := range o.sessions {
		if !stream.confirmed {
			count++
		}
	}
	return count
}

// push queues a frame and wakes the subscriber. The caller holds the
// outbound lock.
func (m *mailbox) push(payload []byte) {
	m.queue = append(m.queue, payload)
	select {
	case m.signal <- struct{}{}:
	default:
	}
}

// lease is one subscriber's hold on a stream.
type lease struct {
	out     *outbound
	box     *mailbox
	session string
	scoped  bool
}

// leaseConnection takes the connection stream.
func (o *outbound) leaseConnection() (*lease, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.connection.leased {
		return nil, errStreamBusy
	}
	o.connection.leased = true
	return &lease{out: o, box: o.connection}, nil
}

// leaseSession takes the stream of a session the connection knows, or of an
// unknown session while fewer than maxUnconfirmedSessionStreams unconfirmed
// streams exist. A WebSocket connection knows no sessions.
func (o *outbound) leaseSession(sessionID string) (*lease, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	stream, ok := o.sessions[sessionID]
	if !ok {
		if o.websocket || o.unconfirmed() >= maxUnconfirmedSessionStreams {
			return nil, errUnknownSession
		}
		stream = &sessionStream{box: newMailbox()}
		o.sessions[sessionID] = stream
	}
	if stream.box.leased {
		return nil, errStreamBusy
	}
	stream.box.leased = true
	return &lease{out: o, box: stream.box, session: sessionID, scoped: true}, nil
}

// next removes and returns the oldest queued frame. Frames are taken one at a
// time, as the Rust crate receives them from its channel, so a subscriber
// whose write fails leaves every later frame queued for the next one.
func (l *lease) next() ([]byte, bool) {
	l.out.mu.Lock()
	defer l.out.mu.Unlock()
	if len(l.box.queue) == 0 {
		return nil, false
	}
	payload := l.box.queue[0]
	l.box.queue[0] = nil
	l.box.queue = l.box.queue[1:]
	if len(l.box.queue) == 0 {
		l.box.queue = nil
	}
	return payload, true
}

// release returns the stream for the next subscriber. An unconfirmed session
// stream only exists while a client holds it, so it is forgotten.
func (l *lease) release() {
	l.out.mu.Lock()
	defer l.out.mu.Unlock()
	l.box.leased = false
	if !l.scoped {
		return
	}
	if stream, ok := l.out.sessions[l.session]; ok && stream.box == l.box && !stream.confirmed {
		delete(l.out.sessions, l.session)
	}
}
