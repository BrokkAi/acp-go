package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/BrokkAi/acp-go"
)

// Serve serves the server directly to one MCP client over newline-delimited
// JSON-RPC (the MCP stdio framing), with no ACP session. Every request is
// handed to the service with a standalone RequestContext; an inner MCP error
// outcome becomes a JSON-RPC error response, and a service error becomes an
// internal error (-32603). Client notifications are ignored.
//
// Serve returns nil when the client closes its output, and ctx's error when
// ctx is cancelled first. In both cases it closes the connection and waits
// for in-flight requests, which are cancelled with ErrConnectionClosed, to
// return.
func (s *Server) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	if err := s.Validate(); err != nil {
		return err
	}
	ready := make(chan struct{})
	var connection *acp.Connection
	connection = acp.Connect(in, out, func(requestCtx context.Context, method string, params json.RawMessage) (any, error) {
		<-ready
		return s.serveStandalone(requestCtx, connection, method, params)
	}, nil)
	close(ready)

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

func (s *Server) serveStandalone(requestCtx context.Context, connection *acp.Connection, method string, params json.RawMessage) (any, error) {
	if string(params) == "null" {
		params = nil
	}
	if len(params) > 0 && params[0] != '{' {
		return nil, &acp.RPCError{Code: codeInvalidParams, Message: "Invalid params", Data: json.RawMessage(`"params must be an object"`)}
	}
	opCtx, cancel := context.WithCancelCause(requestCtx)
	defer cancel(nil)
	go func() {
		select {
		case <-connection.InputClosed():
			cancel(ErrConnectionClosed)
		case <-opCtx.Done():
		}
	}()
	gate := &operationGate{}
	rc := &RequestContext{
		standalone:    true,
		meta:          metaOf(params),
		requestDone:   requestCtx.Done(),
		operationDone: opCtx.Done(),
		notify: func(ctx context.Context, method string, params json.RawMessage) error {
			gate.mu.Lock()
			defer gate.mu.Unlock()
			if gate.done || opCtx.Err() != nil {
				return errors.New("mcp request is no longer active")
			}
			if params == nil {
				params = json.RawMessage("{}")
			}
			return connection.Notify(ctx, method, params)
		},
	}
	outcome, err := s.service.Execute(opCtx, Request{Method: method, Params: params}, rc)
	gate.close()
	if err != nil {
		if opCtx.Err() != nil {
			return nil, &acp.RPCError{Code: -32800, Message: "request cancelled"}
		}
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
