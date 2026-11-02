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

// RequestGuard vets each incoming request on the connection's read loop, in
// wire order, before the request is observed or dispatched, as the reference
// SDK's incoming actor applies protocol state to each request as it reads it.
// A non-nil error answers the request with that error, queued in wire order
// like the connection's other read-loop answers, and its handler never runs.
// Notifications and requests rejected before dispatch, such as a v2 request
// with a null id, bypass the guard. Like a Notifications function, it must
// return promptly and must not call back into the connection.
type RequestGuard func(method string, params json.RawMessage) error

// WithRequestGuard returns an acp.ConnectOption, as any, that installs guard.
// The acp package sets it.
var WithRequestGuard func(guard RequestGuard) any

// NotificationGuard vets each incoming notification on the read loop, in wire
// order, before the connection handles it, including $/cancel_request. A
// non-nil error drops the notification, as the reference SDK ignores a
// notification its protocol state refuses; the error goes to the
// connection's WithErrorHandler hook, if any. It must return promptly and must
// not call back into the connection.
type NotificationGuard func(method string) error

// WithNotificationGuard returns an acp.ConnectOption, as any, that installs
// guard. The acp package sets it.
var WithNotificationGuard func(guard NotificationGuard) any

// OutgoingGuard vets each request and notification the connection is about to
// send, before anything is written, as the reference SDK's outgoing protocol
// state does. notification reports which it is, and params is the encoded
// params. A non-nil error is returned to the caller of Call, Notify, or
// CancelRequest, and nothing is sent; otherwise non-nil rewritten params
// replace params. The guard runs, and the frame is queued, under one lock, so
// no frame queued later overtakes it. When the guard admits a request that
// then cannot be written, the IncomingResponseGuard settles the request with
// the write error.
type OutgoingGuard func(method string, params json.RawMessage, notification bool) (rewritten json.RawMessage, err error)

// WithOutgoingGuard returns an acp.ConnectOption, as any, that installs guard.
// The acp package sets it.
var WithOutgoingGuard func(guard OutgoingGuard) any

// OutgoingResponseGuard vets each response to an incoming request the
// connection is about to send, as the reference SDK's outgoing_response_to
// does: it receives the request's id and method and the handler's result or
// error, and returns what is sent instead. A returned error is sent as the
// error response. It runs, and the response is queued, under the same lock as
// OutgoingGuard. Read-loop answers, such as a RequestGuard refusal, are not
// vetted.
type OutgoingResponseGuard func(id json.RawMessage, method string, result json.RawMessage, err error) (json.RawMessage, error)

// WithOutgoingResponseGuard returns an acp.ConnectOption, as any, that
// installs guard. The acp package sets it.
var WithOutgoingResponseGuard func(guard OutgoingResponseGuard) any

// IncomingResponseGuard vets each response to a request the connection sent,
// on the read loop, as the reference SDK's incoming_response does: it receives
// the request's method, whether the request was sent with
// WithAcceptsLowerVersion, and the peer's result or error, and returns what
// the caller receives instead. It runs for every response, including one whose
// caller stopped waiting, which stays pending until it arrives. A request
// admitted by the OutgoingGuard but never written is settled with the write
// error.
type IncomingResponseGuard func(method string, acceptsLower bool, result json.RawMessage, err error) (json.RawMessage, error)

// WithIncomingResponseGuard returns an acp.ConnectOption, as any, that
// installs guard. The acp package sets it.
var WithIncomingResponseGuard func(guard IncomingResponseGuard) any

type acceptsLowerKey struct{}

// WithAcceptsLowerVersion marks a Call context as the reference SDK's
// send_initialize: its initialize response may select a lower version, which
// the caller receives as a success.
func WithAcceptsLowerVersion(ctx context.Context) context.Context {
	return context.WithValue(ctx, acceptsLowerKey{}, true)
}

// AcceptsLowerVersion reports whether WithAcceptsLowerVersion marked ctx.
func AcceptsLowerVersion(ctx context.Context) bool {
	accepts, _ := ctx.Value(acceptsLowerKey{}).(bool)
	return accepts
}
