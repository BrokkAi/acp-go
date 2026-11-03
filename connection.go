package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/BrokkAi/acp-go/internal/jsonline"
	"github.com/BrokkAi/acp-go/internal/lines"
	"github.com/BrokkAi/acp-go/internal/rpcctl"
	"github.com/BrokkAi/acp-go/schema"
)

type packet struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}
type outgoing struct {
	data []byte
	sent chan error
}

// Handler serves agent-to-client requests. It must honor cancellation. A
// panic is recovered: the request is answered with -32603 (Internal error),
// or -32800 if it was cancelled, and the panic goes to the WithErrorHandler
// hook. As in the reference SDK, a request whose handler panics after input
// closed (see InputClosed) is not answered.
type Handler func(context.Context, string, json.RawMessage) (any, error)

// Notifications run in wire order, before a subsequent response is delivered.
// They must return promptly and must not call back into this Connection.
//
// A notification has no response, so an error a Notifications function
// returns does not close the connection, as in the reference SDK: it goes to
// the WithErrorHandler hook, or is dropped when there is none.
//
// A $/cancel_request, plain or wrapped in proxy/successor, first cancels the
// inbound request it names and is then delivered like any other
// notification, as the reference SDK dispatches it; a forwarding handler
// must not relay it (see IsCancelRequestNotification), because its requestId
// means nothing on another connection. One whose params do not parse is
// dropped.
type Notifications func(string, json.RawMessage) error

type Connection struct {
	in          io.ReadCloser
	out         io.WriteCloser
	ctx         context.Context
	cancel      context.CancelFunc
	onRequest   Handler
	onNotify    Notifications
	onObserve   RequestObserver
	guard       rpcctl.RequestGuard
	notifyGuard rpcctl.NotificationGuard
	outGuard    rpcctl.OutgoingGuard
	outResponse rpcctl.OutgoingResponseGuard
	inResponse  rpcctl.IncomingResponseGuard
	// sendLock, a one-slot semaphore, orders guarded frames: a guard runs and
	// its frame is queued while holding it, so no frame queued later can
	// overtake one a guard admitted. It is acquired with the caller's context,
	// and only when a guard is installed.
	sendLock   chan struct{}
	writes     chan outgoing
	version    uint16
	onError    func(error)
	mu         sync.Mutex
	sequence   uint64
	inboundSeq uint64
	pending    map[string]pendingCall
	inbound    map[string][]inboundCancellation
	failure    error
	inputDone  chan struct{}
	once       sync.Once
	loops      sync.WaitGroup
	tasks      sync.WaitGroup
	// serving is the parent of every incoming request's context. Shutdown
	// cancels it before draining, as the reference SDK drops its handlers
	// once the foreground succeeds.
	serving     context.Context
	stopServing context.CancelFunc
	// shuttingDown is set when Shutdown begins. From then on no frame is
	// queued, incoming frames are read and discarded, and the drain marker
	// Shutdown queues closes drained once every frame queued before it is
	// written.
	shuttingDown atomic.Bool
	// sealMu orders sealing against queueing: a sender checks shuttingDown
	// and registers in enqueuing under it, and the drain marker is queued
	// only after every registered sender has finished, so no frame accepted
	// before the seal can land behind the marker.
	sealMu      sync.Mutex
	enqueuing   sync.WaitGroup
	drained     chan struct{}
	closeOut    sync.Once
	closeOutErr error
}

// RequestObserver sees each incoming request in wire order, on the read loop,
// before its handler starts, so state it records is ordered with the
// notifications around the request. Like Notifications, it must return
// promptly and must not call back into the Connection. A request rejected
// before dispatch, such as a v2 request with a null id, is not observed. A
// panic is recovered and reported like a handler panic, and the request still
// runs.
type RequestObserver func(method string, params json.RawMessage)

// ConnectOption configures a Connection created by Connect.
type ConnectOption func(*Connection)

// WithRequestObserver sets a RequestObserver; nil sets none.
func WithRequestObserver(observe RequestObserver) ConnectOption {
	return func(c *Connection) { c.onObserve = observe }
}

// WithProtocolVersion records the ACP protocol version this connection's API
// speaks, which a version-neutral Connection cannot learn from the wire. As in
// the reference SDK, which fixes it per connection, it decides how a request
// with a null id is handled: version 2 and later answer it with -32600
// (Invalid request) and id null without running a handler, and the connection
// keeps serving. Without this option, or with version 1, null is a valid
// JSON-RPC id and the request runs normally. v2.Connect and the draft-v2
// agent runtime set version 2.
func WithProtocolVersion(version uint16) ConnectOption {
	return func(c *Connection) { c.version = version }
}

// WithErrorHandler sets a function that receives the failures a Connection
// absorbs instead of closing: an error returned by the Notifications
// function, and a *HandlerPanic recovered from a request handler, the
// Notifications function, or the RequestObserver. It runs on the goroutine
// that failed, possibly concurrently with itself, so it must be safe for
// concurrent use, return promptly, and not call back into this Connection. A
// panic in it is recovered and logged, never reported to it again. Without
// it, panics are written to the standard logger, as net/http does, and
// notification errors are dropped.
func WithErrorHandler(report func(error)) ConnectOption {
	return func(c *Connection) { c.onError = report }
}

// HandlerPanic reports a panic recovered from a request handler, a
// Notifications function, or a RequestObserver. A panicking request handler's
// request is answered with -32603 (Internal error), or -32800 when it was
// already cancelled, and the connection keeps serving. The stack is never
// sent to the peer.
type HandlerPanic struct {
	// Method is the method of the request or notification being handled.
	Method string
	// Value is the value passed to panic.
	Value any
	// Stack is the panicking goroutine's stack trace.
	Stack []byte
}

func (p *HandlerPanic) Error() string {
	return fmt.Sprintf("acp: %s handler panicked: %v", p.Method, p.Value)
}

// Connect owns both streams until Close or Shutdown. Frames are limited to 8
// MiB. Each incoming request runs in its own goroutine with no concurrency
// limit, as in the reference SDK, so a peer that pipelines many requests is
// served rather than disconnected. Unknown notifications can be ignored.
//
// One bad message or one failing handler does not close the connection. A
// valid JSON value that is not a JSON-RPC message is answered with -32600 and
// id null, or ignored when it is shaped like a response, as the reference SDK
// does. A notification handler's error is reported, and a handler panic is
// recovered and answered. Only invalid UTF-8, an oversized frame, or a
// transport failure closes the connection.
func Connect(in io.ReadCloser, out io.WriteCloser, h Handler, n Notifications, options ...ConnectOption) *Connection {
	ctx, cancel := context.WithCancel(context.Background())
	serving, stopServing := context.WithCancel(ctx)
	c := &Connection{in: in, out: out, ctx: ctx, cancel: cancel, serving: serving, stopServing: stopServing, onRequest: h, onNotify: n, writes: make(chan outgoing, 32), sendLock: make(chan struct{}, 1), drained: make(chan struct{}), inputDone: make(chan struct{}), pending: make(map[string]pendingCall), inbound: make(map[string][]inboundCancellation)}
	for _, option := range options {
		option(c)
	}
	c.loops.Add(2)
	go c.readLoop()
	go c.writeLoop()
	return c
}
func (c *Connection) stop(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.failure = err
		c.mu.Unlock()
		c.cancel()
		_ = c.in.Close()
		_ = c.closeOutput()
	})
}

// closeOutput closes the output stream once and returns that close's error.
func (c *Connection) closeOutput() error {
	c.closeOut.Do(func() { c.closeOutErr = c.out.Close() })
	return c.closeOutErr
}

// Close aborts the connection: it closes both streams at once, without
// writing frames still queued for the peer, and waits for the connection's
// goroutines and in-flight handlers to return. Calls waiting for a response
// fail with io.ErrClosedPipe. Close is the reference SDK's dropped
// connection; Shutdown is its orderly shutdown. Close always returns nil and
// may be called after Shutdown, or concurrently with it to abort its drain.
func (c *Connection) Close() error {
	c.stop(io.ErrClosedPipe)
	c.loops.Wait()
	c.tasks.Wait()
	return nil
}

// Shutdown closes the connection in order, as the reference SDK does when a
// connect_with foreground returns Ok: it writes every frame already queued for
// the peer, then closes the output stream, then the input stream, and waits
// for the connection's goroutines and in-flight handlers to return, as Close
// does.
//
// Once Shutdown begins, the connection sends nothing new: Notify, Call, and
// CancelRequest fail with io.ErrClosedPipe without writing, the contexts of
// incoming requests are cancelled and responses not yet queued are not sent,
// and incoming frames are read and discarded, so responses arriving during
// the drain complete no call. A call already waiting for its response stops
// waiting, with io.ErrClosedPipe, when the connection closes. Cancelling a
// call's context during the drain does not stop the connection; ctx bounds
// the drain instead.
//
// Shutdown returns nil once the queued frames are written and the output
// stream is closed, or the close's error. If ctx ends first, or ended before
// Shutdown was called, Shutdown aborts like Close and returns ctx's error;
// frames not yet written are dropped, as they are when the reference SDK's
// connection future is dropped. ctx bounds only the drain: the wait for
// in-flight handlers that follows is not bounded, as for Close, so handlers
// must honor cancellation. If the connection had already stopped, or fails
// during the drain, Shutdown returns Err. A connection that stopped on the
// peer's clean EOF returns nil: it wrote every frame queued before it stopped.
// Concurrent calls wait for the same drain; when the ctx of the call that
// began it ends first, that call aborts the connection, and the others return
// io.ErrClosedPipe.
//
// Calling Shutdown or Close from inside a handler, a Notifications function,
// or a RequestObserver deadlocks: both wait for those to return.
func (c *Connection) Shutdown(ctx context.Context) error {
	err := c.drain(ctx)
	c.stop(io.ErrClosedPipe)
	c.loops.Wait()
	c.tasks.Wait()
	return err
}

// seal stops the queue accepting frames and reports whether this call sealed
// it.
func (c *Connection) seal() bool {
	c.sealMu.Lock()
	defer c.sealMu.Unlock()
	return c.shuttingDown.CompareAndSwap(false, true)
}

// admitFrame registers a sender about to queue a frame, unless the queue is
// sealed. An admitted sender calls c.enqueuing.Done once it has queued its
// frame or given up.
func (c *Connection) admitFrame() bool {
	c.sealMu.Lock()
	defer c.sealMu.Unlock()
	if c.shuttingDown.Load() {
		return false
	}
	c.enqueuing.Add(1)
	return true
}

// queueDrainMarker queues the marker that closes drained once every frame
// queued before it is written. It takes the send lock, so a frame a guard
// admitted is queued before it. It gives up when ctx or the connection ends.
func (c *Connection) queueDrainMarker(ctx context.Context) {
	// Senders that passed the seal check finish queueing first.
	settled := make(chan struct{})
	go func() {
		c.enqueuing.Wait()
		close(settled)
	}()
	select {
	case <-settled:
	case <-ctx.Done():
		return
	case <-c.ctx.Done():
		return
	}
	release, err := c.lockSend(ctx)
	if err != nil {
		return
	}
	defer release()
	select {
	case c.writes <- outgoing{}:
	case <-ctx.Done():
	case <-c.ctx.Done():
	}
}

// drain begins the shutdown, waits until the frames queued before it are
// written, and closes the output stream.
func (c *Connection) drain(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.seal() {
		c.stopServing()
		c.queueDrainMarker(ctx)
	}
	select {
	case <-c.drained:
		return c.closeOutput()
	case <-ctx.Done():
	case <-c.ctx.Done():
	}
	// A completed drain wins over a connection stopped after it, and a
	// stopped connection's reason over the deadline.
	if closedChannel(c.drained) {
		return c.closeOutput()
	}
	if !closedChannel(c.ctx.Done()) {
		return ctx.Err()
	}
	if err := c.Err(); !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (c *Connection) Done() <-chan struct{} { return c.ctx.Done() }

// InputClosed is closed once the connection stops reading frames: the peer
// closed its output (EOF), reading failed, a frame was not valid UTF-8, or the
// connection was closed. Frames have no size limit, as in the reference SDK.
// On a clean EOF, Done stays open until in-flight handlers return and every
// frame queued by then, theirs included, is written, as the reference SDK
// keeps its output working after incoming EOF. Once the handlers have
// returned, nothing new is sent: Notify, Call, and CancelRequest fail with
// io.ErrClosedPipe, as during Shutdown, and a Shutdown called then waits for
// the same flush. Like the reference SDK's, the flush has no time limit: a
// peer that stops reading keeps Done open, so bound it with Shutdown, whose
// ctx aborts the flush when it ends, or with Close. Long-running handlers
// that wait on the peer can select on InputClosed to stop instead of waiting
// forever.
func (c *Connection) InputClosed() <-chan struct{} { return c.inputDone }
func (c *Connection) Err() error                   { c.mu.Lock(); defer c.mu.Unlock(); return c.failure }

// guarded reports whether an outgoing or response guard is installed, which
// is when frames must be ordered under sendLock.
func (c *Connection) guarded() bool {
	return c.outGuard != nil || c.outResponse != nil
}

// lockSend acquires sendLock unless ctx or the connection ends first. It
// returns a release function, which is a no-op when no guard is installed.
func (c *Connection) lockSend(ctx context.Context) (func(), error) {
	if !c.guarded() {
		return func() {}, nil
	}
	select {
	case c.sendLock <- struct{}{}:
		return func() { <-c.sendLock }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, c.Err()
	}
}

// queue builds a frame and queues it for writing. When a guard is installed,
// build runs under sendLock immediately before the frame is queued, so a
// frame a guard admitted inside build cannot be overtaken by one queued after
// it. A caller whose ctx ends while it waits for the lock gives up before
// build runs, so nothing is begun. built reports whether build succeeded, so a
// caller can settle what a guard began when queueing then fails.
func (c *Connection) queue(ctx context.Context, build func() (any, error)) (w outgoing, built bool, err error) {
	// A sealed queue refuses with io.ErrClosedPipe even after the connection
	// stops, guarded or not.
	if c.shuttingDown.Load() {
		return outgoing{}, false, io.ErrClosedPipe
	}
	release, err := c.lockSend(ctx)
	if err != nil {
		return outgoing{}, false, err
	}
	defer release()
	if !c.admitFrame() {
		return outgoing{}, false, io.ErrClosedPipe
	}
	defer c.enqueuing.Done()
	value, err := build()
	if err != nil {
		return outgoing{}, false, err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return outgoing{}, true, err
	}
	w = outgoing{data: append(b, '\n'), sent: make(chan error, 1)}
	select {
	case c.writes <- w:
		return w, true, nil
	case <-ctx.Done():
		return outgoing{}, true, ctx.Err()
	case <-c.ctx.Done():
		return outgoing{}, true, c.Err()
	}
}

// awaitWrite waits until a queued frame is written.
func (c *Connection) awaitWrite(ctx context.Context, w outgoing) error {
	select {
	case err := <-w.sent:
		return err
	case <-ctx.Done():
		// A peer can receive a request and cancel its context before Write
		// returns. Give an in-flight frame time to finish so Call can send
		// $/cancel_request without disconnecting an otherwise healthy peer.
		// A genuinely blocked writer still needs its pipe closed to unblock.
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case err := <-w.sent:
			return err
		case <-timer.C:
			// Once the queue is sealed, by Shutdown or the EOF flush, the
			// frame stays queued: the drain writes it or is aborted.
			if !c.shuttingDown.Load() {
				c.stop(ctx.Err())
			}
			return ctx.Err()
		case <-c.ctx.Done():
			return c.sentOr(w)
		}
	case <-c.ctx.Done():
		return c.sentOr(w)
	}
}

// sentOr reports a stopped connection's error for w, unless w was written,
// which wins when both are ready.
func (c *Connection) sentOr(w outgoing) error {
	select {
	case err := <-w.sent:
		return err
	default:
		return c.Err()
	}
}

// guardOutgoing runs the internal outgoing guard, returning the params to
// send. As in the reference SDK, the guard sees the message first; the params
// it admits are then serialized as a JSON-RPC frame, where they must be an
// object, an array, or null, and null params are omitted. admitted reports
// whether the guard admitted the message, so a caller can settle what the
// guard began when the params cannot be sent.
func (c *Connection) guardOutgoing(method string, params json.RawMessage, notification bool) (json.RawMessage, bool, error) {
	if c.outGuard != nil {
		rewritten, err := c.outGuard(method, params, notification)
		if err != nil {
			return nil, false, err
		}
		if rewritten != nil {
			params = rewritten
		}
	}
	if err := checkWireParams(params); err != nil {
		return nil, true, err
	}
	return wireParams(params), true, nil
}

// errDroppedNotification marks a notification whose params cannot be sent.
// As in the reference SDK, whose outgoing actor logs and drops such a frame
// after the send already succeeded, the send reports success and the error
// goes to the WithErrorHandler hook.
type errDroppedNotification struct{ err error }

func (e errDroppedNotification) Error() string { return e.err.Error() }

// buildNotification is the frame builder of Notify and queueNotification.
func (c *Connection) buildNotification(method string, params json.RawMessage) func() (any, error) {
	return func() (any, error) {
		guarded, admitted, err := c.guardOutgoing(method, params, true)
		if err != nil {
			if admitted {
				return nil, errDroppedNotification{err}
			}
			return nil, err
		}
		return packet{Version: "2.0", Method: method, Params: guarded}, nil
	}
}

// droppedNotification reports a notification dropped for its params and
// reports whether err was one.
func (c *Connection) droppedNotification(method string, err error) bool {
	dropped, ok := err.(errDroppedNotification)
	if !ok {
		return false
	}
	if c.onError != nil {
		c.report(fmt.Errorf("acp: dropped %s notification: %w", method, dropped.err))
	}
	return true
}

func (c *Connection) Notify(ctx context.Context, method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	w, _, err := c.queue(ctx, c.buildNotification(method, b))
	if c.droppedNotification(method, err) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.awaitWrite(ctx, w)
}

func (c *Connection) writeLoop() {
	defer c.loops.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case w := <-c.writes:
			if w.data == nil {
				// Shutdown's marker: every frame queued before it is written.
				close(c.drained)
				return
			}
			_, err := io.Copy(c.out, bytes.NewReader(w.data))
			w.sent <- err
			if err != nil {
				c.stop(err)
				return
			}
		}
	}
}

// CancelRequest asks the peer to cancel one outstanding request by sending a
// $/cancel_request notification. Either side may cancel the other's in-flight
// request; the peer answers the original request with a -32800 error or a valid
// response, and both outcomes are normal completion.
//
// requestID identifies the request exactly as the peer saw it on the wire: a
// JSON string or number, supplied as a Go string, an integer, or a
// json.RawMessage for exact bytes. Call numbers its own outbound requests
// from 1, so those IDs are integers. Null, empty, and non-scalar IDs
// are rejected.
func (c *Connection) CancelRequest(ctx context.Context, requestID any) error {
	id, err := encodeRequestID(requestID)
	if err != nil {
		return err
	}
	return c.Notify(ctx, schema.CancelRequestMethodName, schema.CancelRequestNotification{RequestID: id})
}

// encodeRequestID validates a caller-supplied request ID and returns its exact
// JSON encoding, so a json.RawMessage passes through without re-quoting.
func encodeRequestID(requestID any) (json.RawMessage, error) {
	if requestID == nil {
		return nil, errors.New("cancel request requires a request ID")
	}
	raw, err := json.Marshal(requestID)
	if err != nil {
		return nil, fmt.Errorf("encode request ID: %w", err)
	}
	switch {
	case len(raw) == 0 || string(raw) == "null":
		return nil, errors.New("cancel request requires a request ID")
	case raw[0] == '"':
		if string(raw) == `""` {
			return nil, errors.New("cancel request requires a request ID")
		}
		return raw, nil
	case raw[0] == '-' || (raw[0] >= '0' && raw[0] <= '9'):
		return raw, nil
	default:
		return nil, fmt.Errorf("request ID must be a JSON string or number, got %s", raw)
	}
}

// pendingCall is a request awaiting its response. The response is settled
// once, on the read loop, even when the caller stopped waiting.
type pendingCall struct {
	method       string
	acceptsLower bool
	// settled is the rpcctl.WithSettled hook of the call's context.
	settled func(method string, err error)
	reply   chan reply
}

type reply struct {
	result json.RawMessage
	err    error
}

// settle passes a call's outcome through the internal incoming-response
// guard, as the reference SDK checks every response against its protocol
// state, and delivers it to the caller if it is still waiting.
func (c *Connection) settle(call pendingCall, result json.RawMessage, err error) {
	if c.inResponse != nil {
		result, err = c.inResponse(call.method, call.acceptsLower, result, err)
	}
	if call.settled != nil {
		call.settled(call.method, err)
	}
	call.reply <- reply{result: result, err: err}
}

func rpcErrorOrNil(e *RPCError) error {
	if e == nil {
		return nil
	}
	return e
}

func (c *Connection) Call(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.sequence++
	id := strconv.FormatUint(c.sequence, 10)
	call := pendingCall{method: method, acceptsLower: rpcctl.AcceptsLowerVersion(ctx), settled: rpcctl.Settled(ctx), reply: make(chan reply, 1)}
	c.pending[id] = call
	c.mu.Unlock()
	forget := func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }
	// abandon settles a call whose request was admitted but never answered,
	// so the protocol state it began is not left waiting.
	abandon := func(err error) {
		c.mu.Lock()
		_, waiting := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if waiting {
			c.settle(call, nil, err)
		}
	}
	// admitted is set when the guard admitted the request, even if its
	// params then cannot be sent: the call is settled, not forgotten, so
	// what the guard began does not stay in progress.
	admitted := false
	w, built, err := c.queue(ctx, func() (any, error) {
		guarded, ok, err := c.guardOutgoing(method, b, false)
		admitted = ok
		if err != nil {
			return nil, err
		}
		return packet{Version: "2.0", ID: json.RawMessage(id), Method: method, Params: guarded}, nil
	})
	if err != nil {
		if built || admitted {
			abandon(err)
		} else {
			forget()
		}
		return err
	}
	if err := c.awaitWrite(ctx, w); err != nil {
		// A fast peer can answer and exit before the writer goroutine reports
		// completion. The queued response takes precedence over that EOF.
		select {
		case r := <-call.reply:
			return decodeReply(r, result)
		default:
			abandon(err)
			return err
		}
	}
	select {
	case r := <-call.reply:
		return decodeReply(r, result)
	case <-ctx.Done():
		// As in the reference SDK, the call stays pending: its response,
		// when it arrives, still passes through the protocol state.
		if c.inResponse == nil {
			forget()
		}
		cancelCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_ = c.CancelRequest(cancelCtx, json.RawMessage(id))
		cancel()
		return ctx.Err()
	case <-c.ctx.Done():
		// An agent may exit immediately after writing a valid final response.
		select {
		case r := <-call.reply:
			return decodeReply(r, result)
		default:
			forget()
			return c.Err()
		}
	}
}

func decodeReply(r reply, result any) error {
	if r.err != nil {
		return r.err
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(r.result, result)
}

func (c *Connection) readLoop() {
	defer c.loops.Done()
	inputDone := sync.OnceFunc(func() { close(c.inputDone) })
	defer inputDone()
	// As in the reference SDK, a frame of any size is read; the line buffer
	// grows only for a frame that needs it.
	frames := lines.NewReader(bufio.NewReaderSize(c.in, 64<<10))
	var err error
	for {
		var line []byte
		if line, err = frames.Next(); err != nil {
			break
		}
		if !utf8.Valid(line) {
			// The reference SDK reads lines as UTF-8 text, so this is a
			// transport failure there, not a malformed message.
			c.stop(errors.New("invalid ACP JSON-RPC frame"))
			return
		}
		if c.shuttingDown.Load() {
			// As in the reference SDK, Shutdown stops delivery but keeps
			// reading, so a peer still writing is not failed mid-drain.
			continue
		}
		// A value the reference SDK cannot read as one JSON-RPC message gets
		// its single id-null error, and the connection keeps reading.
		switch jsonline.Classify(line) {
		case jsonline.Invalid:
			c.answerFrame(jsonline.ParseError(line))
			continue
		case jsonline.Batch:
			// JSON-RPC batches are not supported on any protocol version;
			// none of the array's entries runs.
			c.answerFrame(jsonline.BatchRejection())
			continue
		}
		p, kind := decodeMessage(line)
		switch kind {
		case malformedCall:
			c.answerFrame(jsonline.InvalidRequest())
		case malformedResponse:
			// Like the reference SDK, a malformed value shaped like a
			// response is dropped: it completes no call and gets no answer.
		case responseMessage:
			c.mu.Lock()
			call, ok := c.pending[string(p.ID)]
			// The first response settles the call; a repeated one finds no
			// pending call and is ignored, as in the reference SDK.
			delete(c.pending, string(p.ID))
			c.mu.Unlock()
			if ok {
				c.settle(call, p.Result, rpcErrorOrNil(p.Error))
			}
		case notificationMessage:
			if c.notifyGuard != nil && !c.admitNotification(p.Method) {
				continue
			}
			// As in the reference SDK, a $/cancel_request, plain or wrapped
			// in proxy/successor, cancels the request it names and then
			// reaches the handler like any notification; one whose params do
			// not parse cancels nothing and is dropped.
			id, kind := cancelRequestID(p.Method, p.Params)
			if kind == malformedCancellation {
				continue
			}
			if kind == cancelsRequest {
				c.cancelInbound(string(id))
			}
			if c.onNotify != nil {
				c.notify(p.Method, p.Params)
			}
		case requestMessage:
			if string(p.ID) == "null" && c.version >= 2 {
				// Protocol v2 forbids null request ids. The reference SDK
				// answers before dispatch and keeps serving.
				c.answerFrame(jsonline.NullV2RequestID())
				continue
			}
			if c.guard != nil && !c.admit(p) {
				continue
			}
			if c.onObserve != nil {
				c.observe(p.Method, p.Params)
			}
			c.startRequest(p)
		}
	}
	if errors.Is(err, io.EOF) {
		err = nil
	}
	inputDone()
	if err == nil {
		// A clean EOF during Shutdown leaves the drain to finish; a read
		// failure still stops it, as in the reference SDK.
		if c.shuttingDown.Load() {
			return
		}
		c.tasks.Wait()
		// As in the reference SDK, incoming EOF does not stop output:
		// every frame already queued is written before the connection
		// stops. The flush seals the queue first, as Shutdown does, so no
		// frame is accepted behind its marker, and a Shutdown that starts
		// during the flush waits for this one instead of queueing its own.
		// A write failure during the flush stops the connection first and
		// becomes its error. A Shutdown that began first owns the drain.
		if !c.seal() {
			return
		}
		c.queueDrainMarker(c.ctx)
		select {
		case <-c.drained:
		case <-c.ctx.Done():
		}
		err = io.EOF
	}
	c.stop(err)
}

// answerFrame queues the id-null error answering a malformed frame before the
// read loop handles the next frame, as the reference SDK does, so nothing a
// later frame causes, such as a request's response or a notification
// handler's own send, can overtake it. Queueing blocks only while the write
// queue is full, which is backpressure from a peer that is not reading. Only
// waiting for the write runs as a task, which EOF still waits for.
func (c *Connection) answerFrame(response []byte) {
	if c.shuttingDown.Load() {
		return
	}
	w := outgoing{data: append(response, '\n'), sent: make(chan error, 1)}
	release, err := c.lockSend(c.ctx)
	if err != nil {
		return
	}
	if !c.admitFrame() {
		release()
		return
	}
	select {
	case c.writes <- w:
		c.enqueuing.Done()
		release()
	case <-c.ctx.Done():
		c.enqueuing.Done()
		release()
		return
	}
	c.tasks.Add(1)
	go func() {
		defer c.tasks.Done()
		select {
		case <-w.sent:
		case <-c.ctx.Done():
		}
	}()
}

// messageKind is how the reference SDK reads one JSON value that is valid JSON
// and not an array.
type messageKind int

const (
	requestMessage messageKind = iota
	notificationMessage
	responseMessage
	// malformedCall is not a JSON-RPC message and not shaped like a response:
	// it gets -32600 with id null.
	malformedCall
	// malformedResponse is shaped like a response (an object with "result"
	// or "error" and no "method") but is not a valid one: it is dropped.
	malformedResponse
)

// decodeMessage reads line, valid JSON that is not an array, as the reference
// SDK's RawJsonRpcMessage deserializer does, with member names matched
// exactly:
//
//   - with "method" and neither "result" nor "error": a request when it has
//     "id", otherwise a notification. "jsonrpc" must be "2.0", "method" a
//     string, "params" absent, null, an object, or an array, and "id" null, a
//     string, or an integer in the int64 range.
//   - with "id", no "method", and exactly one of "result" and "error": a
//     response, with the same "jsonrpc" and "id" rules and an error object
//     that RPCError accepts.
//   - anything else is malformed. It is dropped when it is shaped like a
//     response (incoming_actor frame_entry, is_response_only_shape) and
//     answered otherwise.
//
// Other members are ignored. A repeated member, including one of the error
// object, keeps its last value, as serde_json does.
func decodeMessage(line []byte) (packet, messageKind) {
	var members map[string]json.RawMessage
	if trimmed := bytes.TrimLeft(line, " \t\r\n"); trimmed[0] != '{' || json.Unmarshal(line, &members) != nil {
		return packet{}, malformedCall
	}
	id, hasID := members["id"]
	method, hasMethod := members["method"]
	result, hasResult := members["result"]
	rawError, hasError := members["error"]
	p := packet{Version: "2.0", ID: id, Params: members["params"]}
	var version string
	validVersion := json.Unmarshal(members["jsonrpc"], &version) == nil && version == "2.0"
	switch {
	case hasMethod && !hasResult && !hasError:
		if !validVersion || method[0] != '"' || json.Unmarshal(method, &p.Method) != nil || !validParams(p.Params) || (hasID && !validRequestID(id)) {
			return packet{}, malformedCall
		}
		if hasID {
			// Ids are compared and echoed as the reference SDK decodes them.
			p.ID = canonicalID(id)
			return p, requestMessage
		}
		return p, notificationMessage
	case !hasMethod && hasID && hasResult != hasError:
		if !validVersion || !validRequestID(id) {
			return packet{}, malformedResponse
		}
		p.ID = canonicalID(id)
		p.Params = nil
		if hasResult {
			p.Result = result
			return p, responseMessage
		}
		// The reference SDK reads the whole message as a serde_json value
		// first, so a repeated member of the error object keeps its last
		// value there instead of being rejected as RPCError rejects it.
		var errorMembers map[string]json.RawMessage
		if json.Unmarshal(rawError, &errorMembers) != nil || errorMembers == nil {
			return packet{}, malformedResponse
		}
		canonical, err := json.Marshal(errorMembers)
		p.Error = new(RPCError)
		if err != nil || p.Error.UnmarshalJSON(canonical) != nil {
			return packet{}, malformedResponse
		}
		return p, responseMessage
	case !hasMethod && (hasResult || hasError):
		return packet{}, malformedResponse
	default:
		return packet{}, malformedCall
	}
}

// validParams reports whether params, a member's raw JSON or nil when absent,
// is absent, null, an object, or an array.
func validParams(params json.RawMessage) bool {
	return len(params) == 0 || string(params) == "null" || params[0] == '{' || params[0] == '['
}

// validRequestID reports whether id is a JSON-RPC id the reference SDK reads:
// null, a string, or an integer that fits int64. A number with a fraction or
// an exponent, and -0, are floats there, so they are not ids.
func validRequestID(id json.RawMessage) bool {
	switch {
	case string(id) == "null" || id[0] == '"':
		return true
	case id[0] == '-' || (id[0] >= '0' && id[0] <= '9'):
		if string(id) == "-0" || bytes.ContainsAny(id, ".eE") {
			return false
		}
		_, err := strconv.ParseInt(string(id), 10, 64)
		return err == nil
	default:
		return false
	}
}

// notify runs the Notifications function. Its error or panic is reported
// instead of closing the connection: a notification has nothing to answer.
func (c *Connection) notify(method string, params json.RawMessage) {
	defer func() {
		if value := recover(); value != nil {
			c.reportPanic(method, value)
		}
	}()
	if err := c.onNotify(method, params); err != nil && c.onError != nil {
		c.report(fmt.Errorf("acp: %s notification handler: %w", method, err))
	}
}

// observe runs the RequestObserver, reporting a panic instead of crashing.
func (c *Connection) observe(method string, params json.RawMessage) {
	defer func() {
		if value := recover(); value != nil {
			c.reportPanic(method, value)
		}
	}()
	c.onObserve(method, params)
}

// reportPanic hands a recovered handler panic to the error hook, or to the
// standard logger when there is none.
func (c *Connection) reportPanic(method string, value any) {
	failure := &HandlerPanic{Method: method, Value: value, Stack: debug.Stack()}
	if c.onError != nil {
		c.report(failure)
		return
	}
	log.Printf("%v\n%s", failure, failure.Stack)
}

// report calls the error hook. A panic in the hook is recovered and written
// to the standard logger, never handed back to the hook, so a faulty hook can
// neither crash the process nor recurse.
func (c *Connection) report(err error) {
	defer func() {
		if value := recover(); value != nil {
			log.Printf("acp: error handler panicked while reporting %v: %v\n%s", err, value, debug.Stack())
		}
	}()
	c.onError(err)
}

func (c *Connection) startRequest(p packet) {
	ctx, cancel := context.WithCancel(c.serving)
	generation := c.registerInbound(string(p.ID), cancel)
	c.tasks.Add(1)
	go func() {
		defer c.tasks.Done()
		defer cancel()
		response, answer := c.executeRequest(rpcctl.WithRequestID(ctx, p.ID), p)
		c.unregisterInbound(string(p.ID), generation)
		if !answer {
			return
		}
		w, _, err := c.queue(c.ctx, func() (any, error) {
			if c.outResponse != nil {
				result, err := c.outResponse(p.ID, p.Method, response.Result, rpcErrorOrNil(response.Error))
				response.Result, response.Error = result, nil
				if err != nil {
					response.Result, response.Error = nil, errorObject(err)
				}
			}
			return response, nil
		})
		if err == nil {
			_ = c.awaitWrite(c.ctx, w)
		}
	}()
}

type inboundCancellation struct {
	cancel     context.CancelFunc
	generation uint64
}

func (c *Connection) registerInbound(id string, cancel context.CancelFunc) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inboundSeq++
	c.inbound[id] = append(c.inbound[id], inboundCancellation{cancel: cancel, generation: c.inboundSeq})
	return c.inboundSeq
}

func (c *Connection) unregisterInbound(id string, generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cancels := c.inbound[id]
	for i, candidate := range cancels {
		if candidate.generation == generation {
			c.inbound[id] = append(cancels[:i], cancels[i+1:]...)
			break
		}
	}
	if len(c.inbound[id]) == 0 {
		delete(c.inbound, id)
	}
}

func (c *Connection) cancelInbound(id string) {
	c.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(c.inbound[id]))
	for _, candidate := range c.inbound[id] {
		cancels = append(cancels, candidate.cancel)
	}
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// executeRequest runs the handler. answer is false when an internal binding
// asked for the request to go unanswered (rpcctl.ErrNoResponse), or when the
// handler panicked after input closed.
func (c *Connection) executeRequest(ctx context.Context, p packet) (response packet, answer bool) {
	defer func() {
		if value := recover(); value != nil {
			c.reportPanic(p.Method, value)
			// The reference SDK does not answer a dropped responder while the
			// connection is shutting down (incoming closed), the same rule
			// the bindings follow with rpcctl.ErrNoResponse after input EOF.
			if closedChannel(c.inputDone) {
				response, answer = packet{}, false
				return
			}
			response, answer = packet{Version: "2.0", ID: p.ID, Error: panicError(ctx, p.Method)}, true
		}
	}()
	var value any
	var err error = &RPCError{Code: -32601, Message: "unsupported method: " + p.Method}
	if c.onRequest != nil {
		value, err = c.onRequest(ctx, p.Method, p.Params)
	}
	if errors.Is(err, rpcctl.ErrNoResponse) {
		return packet{}, false
	}
	response = packet{Version: "2.0", ID: p.ID}
	if err != nil {
		response.Error = errorObject(err)
	} else {
		response.Result, err = json.Marshal(value)
		if err != nil {
			response.Error = &RPCError{Code: -32603, Message: err.Error()}
			response.Result = nil
		}
	}
	return response, true
}

// errorObject converts a handler or guard error to the error object answering
// its request: an *RPCError as is, cancellation as -32800, and anything else
// as -32603 with the error's text.
func errorObject(err error) *RPCError {
	var object *RPCError
	if !errors.As(err, &object) {
		object = &RPCError{Code: -32603, Message: err.Error()}
	}
	if errors.Is(err, context.Canceled) {
		object = &RPCError{Code: -32800, Message: "Request cancelled"}
	}
	// An error with invalid Data or Extra JSON would otherwise fail to encode
	// and leave the peer waiting for a response forever.
	if _, encodeErr := json.Marshal(object); encodeErr != nil {
		object = &RPCError{Code: -32603, Message: "encode error response: " + encodeErr.Error()}
	}
	return object
}

// admit runs the internal request guard on the read loop. A refused request is
// answered in wire order, before the read loop handles the next frame, and is
// neither observed nor dispatched. A panicking guard is reported and its
// request answered as a panicking handler's would be.
func (c *Connection) admit(p packet) (admitted bool) {
	var refusal *RPCError
	func() {
		defer func() {
			if value := recover(); value != nil {
				c.reportPanic(p.Method, value)
				refusal = panicError(c.ctx, p.Method)
			}
		}()
		if err := c.guard(p.Method, p.Params); err != nil {
			refusal = errorObject(err)
		}
	}()
	if refusal == nil {
		return true
	}
	frame, err := json.Marshal(packet{Version: "2.0", ID: p.ID, Error: refusal})
	if err != nil {
		frame, _ = json.Marshal(packet{Version: "2.0", ID: p.ID, Error: &RPCError{Code: -32603, Message: "encode error response: " + err.Error()}})
	}
	c.answerFrame(frame)
	return false
}

// admitNotification runs the internal notification guard on the read loop. A
// refused notification is dropped, as the reference SDK ignores it, and the
// refusal goes to the error hook. A panicking guard is reported and its
// notification dropped.
func (c *Connection) admitNotification(method string) (admitted bool) {
	defer func() {
		if value := recover(); value != nil {
			c.reportPanic(method, value)
			admitted = false
		}
	}()
	if err := c.notifyGuard(method); err != nil {
		if c.onError != nil {
			c.report(fmt.Errorf("acp: %s notification ignored: %w", method, err))
		}
		return false
	}
	return true
}

// closedChannel reports whether done is closed.
func closedChannel(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// panicError answers a request whose handler panicked. The reference SDK does
// not catch panics; its closest wire behavior is the answer to a request whose
// handler dropped its responder without responding (rust-sdk #462): -32800
// when the request was cancelled, and otherwise -32603 (Internal error) with
// data naming the method. A panic is answered exactly that way, as if the
// responder were dropped. Neither the panic value nor the stack reaches the
// peer.
func panicError(ctx context.Context, method string) *RPCError {
	if ctx.Err() != nil {
		return &RPCError{Code: -32800, Message: "Request cancelled"}
	}
	// The method passed JSON decoding, so it is valid UTF-8.
	data := jsonline.Quote([]byte("request handler dropped its responder for `" + method + "` without responding"))
	return &RPCError{Code: -32603, Message: "Internal error", Data: data}
}
