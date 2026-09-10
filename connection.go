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
	pending     map[string]chan packet
	inbound     map[string]context.CancelFunc
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
	c := &Connection{in: in, out: out, ctx: ctx, cancel: cancel, onRequest: h, onNotify: n, writes: make(chan outgoing, 32), workers: make(chan struct{}, 32), pending: make(map[string]chan packet), inbound: make(map[string]context.CancelFunc)}
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
	b, err := json.Marshal(p)
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
		var p packet
		if !utf8.Valid(s.Bytes()) || json.Unmarshal(s.Bytes(), &p) != nil || p.Version != "2.0" {
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
					c.mu.Lock()
					cancel := c.inbound[string(v.ID)]
					c.mu.Unlock()
					if cancel != nil {
						cancel()
					}
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
		select {
		case c.workers <- struct{}{}:
		default:
			c.stop(errors.New("too many simultaneous ACP requests"))
			return
		}
		ctx, cancel := context.WithCancel(c.ctx)
		c.mu.Lock()
		_, duplicate := c.inbound[string(p.ID)]
		if !duplicate {
			c.inbound[string(p.ID)] = cancel
		}
		c.mu.Unlock()
		if duplicate {
			cancel()
			<-c.workers
			c.stop(errors.New("duplicate inbound ACP request ID"))
			return
		}
		c.tasks.Add(1)
		go c.handle(ctx, cancel, p)
	}
	err := s.Err()
	if err == nil {
		err = io.EOF
	}
	c.stop(err)
}
func (c *Connection) handle(ctx context.Context, cancel context.CancelFunc, p packet) {
	defer c.tasks.Done()
	defer func() { cancel(); <-c.workers }()
	var value any
	var err error = &RPCError{Code: -32601, Message: "unsupported method: " + p.Method}
	if c.onRequest != nil {
		value, err = c.onRequest(ctx, p.Method, p.Params)
	}
	// Retire the request before its response is visible to the peer. A peer
	// may reuse this ID immediately after reading that response.
	c.mu.Lock()
	delete(c.inbound, string(p.ID))
	c.mu.Unlock()
	response := packet{ID: p.ID}
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
	_ = c.send(c.ctx, response)
}
