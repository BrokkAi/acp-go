package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/BrokkAi/acp-go/internal/jsonline"
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

// Handler serves agent-to-client requests. It must honor cancellation.
type Handler func(context.Context, string, json.RawMessage) (any, error)

// Notifications run in wire order, before a subsequent response is delivered.
// They must return promptly and must not call back into this Connection.
type Notifications func(string, json.RawMessage) error

type Connection struct {
	in          io.ReadCloser
	out         io.WriteCloser
	ctx         context.Context
	cancel      context.CancelFunc
	onRequest   Handler
	onNotify    Notifications
	onObserve   RequestObserver
	writes      chan outgoing
	workers     chan struct{}
	mu          sync.Mutex
	sequence    uint64
	inboundSeq  uint64
	pending     map[string]chan packet
	inbound     map[string][]inboundCancellation
	failure     error
	initOnce    bool
	initPending bool
	inputDone   chan struct{}
	once        sync.Once
	loops       sync.WaitGroup
	tasks       sync.WaitGroup
}

// RequestObserver sees each incoming request in wire order, on the read loop,
// before its handler starts, so state it records is ordered with the
// notifications around the request. Like Notifications, it must return
// promptly and must not call back into the Connection.
type RequestObserver func(method string, params json.RawMessage)

// Connect owns both streams until Close. Frames are limited to 8 MiB; at most
// 32 incoming requests run concurrently. Unknown notifications can be ignored.
func Connect(in io.ReadCloser, out io.WriteCloser, h Handler, n Notifications) *Connection {
	return ConnectObserving(in, out, h, n, nil)
}

// ConnectObserving is Connect with an optional RequestObserver.
func ConnectObserving(in io.ReadCloser, out io.WriteCloser, h Handler, n Notifications, observe RequestObserver) *Connection {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Connection{in: in, out: out, ctx: ctx, cancel: cancel, onRequest: h, onNotify: n, onObserve: observe, writes: make(chan outgoing, 32), inputDone: make(chan struct{}), workers: make(chan struct{}, 32), pending: make(map[string]chan packet), inbound: make(map[string][]inboundCancellation)}
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
		_ = c.out.Close()
	})
}
func (c *Connection) Close() error {
	c.stop(io.ErrClosedPipe)
	c.loops.Wait()
	c.tasks.Wait()
	return nil
}
func (c *Connection) Done() <-chan struct{} { return c.ctx.Done() }

// InputClosed is closed once the connection stops reading frames: the peer
// closed its output (EOF), a frame was invalid, or the connection was closed.
// On a clean EOF, Done stays open until in-flight handlers return so their
// responses can still be written; long-running handlers that wait on the peer
// can select on InputClosed to stop instead of waiting forever.
func (c *Connection) InputClosed() <-chan struct{} { return c.inputDone }
func (c *Connection) Err() error                   { c.mu.Lock(); defer c.mu.Unlock(); return c.failure }

func (c *Connection) send(ctx context.Context, p packet) error {
	p.Version = "2.0"
	return c.sendJSON(ctx, p)
}

func (c *Connection) sendJSON(ctx context.Context, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	w := outgoing{data: append(b, '\n'), sent: make(chan error, 1)}
	select {
	case c.writes <- w:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return c.Err()
	}
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
			c.stop(ctx.Err())
			return ctx.Err()
		case <-c.ctx.Done():
			return c.Err()
		}
	case <-c.ctx.Done():
		return c.Err()
	}
}
func (c *Connection) writeLoop() {
	defer c.loops.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case w := <-c.writes:
			_, err := io.Copy(c.out, bytes.NewReader(w.data))
			w.sent <- err
			if err != nil {
				c.stop(err)
				return
			}
		}
	}
}

func (c *Connection) Notify(ctx context.Context, method string, params any) error {
	b, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.send(ctx, packet{Method: method, Params: b})
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
	reply := make(chan packet, 1)
	c.pending[id] = reply
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.send(ctx, packet{ID: json.RawMessage(id), Method: method, Params: b}); err != nil {
		// A fast peer can answer and exit before the writer goroutine reports
		// completion. The queued response takes precedence over that EOF.
		select {
		case p := <-reply:
			return decodeReply(p, result)
		default:
			return err
		}
	}
	select {
	case p := <-reply:
		return decodeReply(p, result)
	case <-ctx.Done():
		cancelCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_ = c.CancelRequest(cancelCtx, json.RawMessage(id))
		cancel()
		return ctx.Err()
	case <-c.ctx.Done():
		// An agent may exit immediately after writing a valid final response.
		select {
		case p := <-reply:
			return decodeReply(p, result)
		default:
			return c.Err()
		}
	}
}

func decodeReply(p packet, result any) error {
	if p.Error != nil {
		return p.Error
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(p.Result, result)
}

func (c *Connection) readLoop() {
	defer c.loops.Done()
	inputDone := sync.OnceFunc(func() { close(c.inputDone) })
	defer inputDone()
	s := bufio.NewScanner(c.in)
	s.Buffer(make([]byte, 4096), 8<<20)
	for s.Scan() {
		line := s.Bytes()
		if utf8.Valid(line) {
			// A value the reference SDK cannot read as one JSON-RPC message
			// gets its single id-null error, and the connection keeps reading.
			switch jsonline.Classify(line) {
			case jsonline.Invalid:
				c.answerFrame(jsonline.ParseError(line))
				continue
			case jsonline.Batch:
				// JSON-RPC batches are not supported on any protocol
				// version; none of the array's entries runs.
				c.answerFrame(jsonline.BatchRejection())
				continue
			}
		}
		trimmed := bytes.TrimSpace(line)
		var p packet
		if !utf8.Valid(trimmed) || json.Unmarshal(trimmed, &p) != nil || p.Version != "2.0" {
			c.stop(errors.New("invalid ACP JSON-RPC frame"))
			return
		}
		if p.Method == "" {
			if len(p.ID) == 0 || (p.Result == nil) == (p.Error == nil) {
				c.stop(errors.New("invalid ACP response"))
				return
			}
			c.mu.Lock()
			ch := c.pending[string(p.ID)]
			c.mu.Unlock()
			if ch != nil {
				select {
				case ch <- p:
				default:
					c.stop(errors.New("duplicate ACP response"))
					return
				}
			}
			continue
		}
		if len(p.ID) == 0 {
			if p.Method == "$/cancel_request" {
				var v struct {
					ID json.RawMessage `json:"requestId"`
				}
				if json.Unmarshal(p.Params, &v) == nil {
					c.cancelInbound(string(v.ID))
				}
			} else if c.onNotify != nil {
				if err := c.onNotify(p.Method, p.Params); err != nil {
					c.stop(err)
					return
				}
			}
			continue
		}
		if string(p.ID) == "null" {
			c.stop(errors.New("null ACP request ID"))
			return
		}
		if c.onObserve != nil {
			c.onObserve(p.Method, p.Params)
		}
		c.startRequest(p)
	}
	err := s.Err()
	inputDone()
	if err == nil {
		c.tasks.Wait()
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
	w := outgoing{data: append(response, '\n'), sent: make(chan error, 1)}
	select {
	case c.writes <- w:
	case <-c.ctx.Done():
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

func (c *Connection) startRequest(p packet) {
	if err := c.reserveWorker(); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	generation := c.registerInbound(string(p.ID), cancel)
	c.tasks.Add(1)
	go func() {
		defer c.tasks.Done()
		defer cancel()
		response, answer := c.executeRequest(rpcctl.WithRequestID(ctx, p.ID), p)
		c.unregisterInbound(string(p.ID), generation)
		// Free handler capacity before the peer can observe the response.
		c.releaseWorker()
		if answer {
			_ = c.send(c.ctx, response)
		}
	}()
}

func (c *Connection) reserveWorker() error {
	select {
	case c.workers <- struct{}{}:
		return nil
	default:
		err := errors.New("too many simultaneous ACP requests")
		c.stop(err)
		return err
	}
}

func (c *Connection) releaseWorker() {
	<-c.workers
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
// asked for the request to go unanswered (rpcctl.ErrNoResponse).
func (c *Connection) executeRequest(ctx context.Context, p packet) (response packet, answer bool) {
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
		if !errors.As(err, &response.Error) {
			response.Error = &RPCError{Code: -32603, Message: err.Error()}
		}
		if errors.Is(err, context.Canceled) {
			response.Error = &RPCError{Code: -32800, Message: "request cancelled"}
		}
		// A handler error with invalid Data or Extra JSON would otherwise fail
		// to encode and leave the peer waiting for a response forever.
		if _, encodeErr := json.Marshal(response.Error); encodeErr != nil {
			response.Error = &RPCError{Code: -32603, Message: "encode error response: " + encodeErr.Error()}
		}
	} else {
		response.Result, err = json.Marshal(value)
		if err != nil {
			response.Error = &RPCError{Code: -32603, Message: err.Error()}
			response.Result = nil
		}
	}
	return response, true
}
