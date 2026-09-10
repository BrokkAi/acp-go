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
)

// RPCError preserves the peer's JSON-RPC error, including extension data.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("ACP error %d: %s", e.Code, e.Message) }

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
	once        sync.Once
	loops       sync.WaitGroup
	tasks       sync.WaitGroup
}

// Connect owns both streams until Close. Frames are limited to 8 MiB; at most
// 32 incoming requests run concurrently. Unknown notifications can be ignored.
func Connect(in io.ReadCloser, out io.WriteCloser, h Handler, n Notifications) *Connection {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Connection{in: in, out: out, ctx: ctx, cancel: cancel, onRequest: h, onNotify: n, writes: make(chan outgoing, 32), workers: make(chan struct{}, 32), pending: make(map[string]chan packet), inbound: make(map[string][]inboundCancellation)}
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
func (c *Connection) Err() error            { c.mu.Lock(); defer c.mu.Unlock(); return c.failure }

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
		c.stop(ctx.Err())
		return ctx.Err() // Interrupt a blocked pipe write.
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
		_ = c.Notify(cancelCtx, "$/cancel_request", map[string]any{"requestId": json.RawMessage(id)})
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
	s := bufio.NewScanner(c.in)
	s.Buffer(make([]byte, 4096), 8<<20)
	for s.Scan() {
		trimmed := bytes.TrimSpace(s.Bytes())
		if len(trimmed) > 0 && trimmed[0] == '[' {
			c.startBatch(append([]byte(nil), trimmed...))
			continue
		}
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
		c.startRequest(p)
	}
	err := s.Err()
	if err == nil {
		c.tasks.Wait()
		err = io.EOF
	}
	c.stop(err)
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
		defer func() { cancel(); c.releaseWorker() }()
		response := c.executeRequest(ctx, p)
		c.unregisterInbound(string(p.ID), generation)
		_ = c.send(c.ctx, response)
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

func (c *Connection) executeRequest(ctx context.Context, p packet) packet {
	var value any
	var err error = &RPCError{Code: -32601, Message: "unsupported method: " + p.Method}
	if c.onRequest != nil {
		value, err = c.onRequest(ctx, p.Method, p.Params)
	}
	response := packet{Version: "2.0", ID: p.ID}
	if err != nil {
		if !errors.As(err, &response.Error) {
			response.Error = &RPCError{Code: -32603, Message: err.Error()}
		}
		if errors.Is(err, context.Canceled) {
			response.Error = &RPCError{Code: -32800, Message: "request cancelled"}
		}
	} else {
		response.Result, err = json.Marshal(value)
		if err != nil {
			response.Error = &RPCError{Code: -32603, Message: err.Error()}
			response.Result = nil
		}
	}
	return response
}
