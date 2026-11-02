// Package rpcctl carries connection controls between the acp package and the
// SDK's internal protocol bindings without adding them to the public API.
package rpcctl

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrNoResponse, returned (or wrapped) by a handler, tells the connection to
// send no response to the request. A binding uses it when the request lost its
// authority to answer, for example after input EOF or a peer cancellation
// whose protocol forbids a response.
var ErrNoResponse = errors.New("rpcctl: request is not answered")

type requestIDKey struct{}

// WithRequestID records the inbound JSON-RPC request ID, exactly as it
// appeared on the wire, in a handler context.
func WithRequestID(ctx context.Context, id json.RawMessage) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the inbound JSON-RPC request ID of a handler context.
func RequestID(ctx context.Context) (json.RawMessage, bool) {
	id, ok := ctx.Value(requestIDKey{}).(json.RawMessage)
	return id, ok
}

// QueueNotification queues a notification on connection (an *acp.Connection)
// and returns once the frame is queued for writing, or when ctx or the
// connection ends first, without waiting for the write. Cancelling ctx after
// the frame is queued never tears the connection down. wait reports the
// write's outcome; its context only bounds the wait. The acp package sets it.
var QueueNotification func(connection any, ctx context.Context, method string, params any) (wait func(context.Context) error, err error)
