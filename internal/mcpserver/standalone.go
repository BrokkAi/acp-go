package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/rpcctl"
)

// Serve serves the server directly to one MCP client over newline-delimited
// JSON-RPC (the MCP stdio framing), with no ACP session. Every request is
// handed to the service with a standalone RequestContext; an inner MCP error
// outcome becomes a JSON-RPC error response, and a service error becomes an
// internal error (-32603).
//
// A notifications/cancelled from the client cancels the named request with
// ErrRequestCancelled, and, as in the reference SDK, the request then gets no
// response. Other client notifications are ignored.
//
// Serve returns nil when the client closes its output, and ctx's error when
// ctx is cancelled first. Either way the connection is closed, in-flight
// requests are cancelled with ErrConnectionClosed and get no response, and
// Serve waits for them to return.
func (s *Server) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	if err := s.Validate(); err != nil {
		return err
	}
	session := &standaloneSession{server: s, active: map[string]context.CancelCauseFunc{}}
	ready := make(chan struct{})
	session.connection = acp.Connect(in, out, func(requestCtx context.Context, method string, params json.RawMessage) (any, error) {
		<-ready
		return session.serve(requestCtx, method, params)
	}, session.notification)
	close(ready)
	connection := session.connection

	select {
	case <-connection.Done():
	case <-ctx.Done():
		_ = connection.Close()
		return ctx.Err()
	}
	err := connection.Err()
	_ = connection.Close()
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}

type standaloneSession struct {
	server     *Server
	connection *acp.Connection

	mu     sync.Mutex
	active map[string]context.CancelCauseFunc
}

// requestKey normalizes a JSON-RPC ID so a cancellation matches its request.
func requestKey(id json.RawMessage) string {
	var compact bytes.Buffer
	if json.Compact(&compact, id) != nil {
		return string(id)
	}
	return compact.String()
}

// notification handles client notifications on the read loop. Only
// notifications/cancelled has an effect.
func (s *standaloneSession) notification(method string, params json.RawMessage) error {
	if method != "notifications/cancelled" {
		return nil
	}
	var cancelledRequest struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &cancelledRequest) != nil || len(cancelledRequest.RequestID) == 0 {
		return nil
	}
	s.mu.Lock()
	cancel := s.active[requestKey(cancelledRequest.RequestID)]
	s.mu.Unlock()
	if cancel != nil {
		cancel(ErrRequestCancelled)
	}
	return nil
}

func (s *standaloneSession) serve(requestCtx context.Context, method string, params json.RawMessage) (any, error) {
	connection := s.connection
	if string(bytes.TrimSpace(params)) == "null" {
		params = nil
	}
	opCtx, cancel := context.WithCancelCause(context.WithoutCancel(requestCtx))
	defer cancel(nil)
	if id, ok := rpcctl.RequestID(requestCtx); ok {
		key := requestKey(id)
		s.mu.Lock()
		s.active[key] = cancel
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.active, key)
			s.mu.Unlock()
		}()
	}
	go func() {
		select {
		case <-opCtx.Done():
		case <-connection.InputClosed():
			cancel(ErrConnectionClosed)
		case <-requestCtx.Done():
			if closedChannel(connection.Done()) {
				cancel(ErrConnectionClosed)
			} else {
				cancel(ErrRequestCancelled)
			}
		}
	}()
	gate := &operationGate{}
	rc := &RequestContext{
		standalone:    true,
		meta:          metaOf(params),
		requestDone:   requestCtx.Done(),
		operationDone: opCtx.Done(),
		notify: func(ctx context.Context, method string, params json.RawMessage) error {
			if params == nil {
				params = json.RawMessage("{}")
			}
			queueCtx, cancelQueue := context.WithCancel(ctx)
			defer cancelQueue()
			stopQueue := context.AfterFunc(opCtx, cancelQueue)
			defer stopQueue()
			gate.mu.Lock()
			if gate.done || opCtx.Err() != nil {
				gate.mu.Unlock()
				return errors.New("mcp request is no longer active")
			}
			wait, err := rpcctl.QueueNotification(connection, queueCtx, method, params)
			gate.mu.Unlock()
			if err != nil {
				return err
			}
			return wait(queueCtx)
		},
	}
	var outcome Outcome
	var err error
	if opCtx.Err() == nil && !closedChannel(connection.InputClosed()) {
		outcome, err = s.server.service.Execute(opCtx, Request{Method: method, Params: params}, rc)
	} else {
		cancel(ErrConnectionClosed)
	}
	gate.close()
	if opCtx.Err() != nil || closedChannel(connection.InputClosed()) {
		// A request the client cancelled, or one outlived by its input, gets
		// no response.
		return nil, rpcctl.ErrNoResponse
	}
	if err != nil {
		return nil, &acp.RPCError{Code: codeInternalError, Message: err.Error()}
	}
	if outcome.Error != nil {
		return nil, &acp.RPCError{Code: outcome.Error.Code, Message: outcome.Error.Message, Data: outcome.Error.Data}
	}
	if outcome.Result == nil {
		return json.RawMessage("null"), nil
	}
	return outcome.Result, nil
}
