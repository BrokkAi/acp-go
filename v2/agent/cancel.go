package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/jsonline"
	"github.com/BrokkAi/acp-go/internal/serdejson"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// ToolCallNotCancellableError is the -32602 (Invalid params) error for a
// tool_call/cancel naming a tool call that is unknown, already final, or not
// cancellable. It is the error the runtime answers when the Agent does not
// implement ToolCallCanceller, exactly as the reference SDK's v2 agent does:
// message "the tool call is unknown or not cancellable" and data
// {"toolCallId": id}.
func ToolCallNotCancellableError(toolCallID schema.ToolCallId) *acp.RPCError {
	return entityCancelError("toolCallId", "tool call", string(toolCallID))
}

// TerminalNotCancellableError is the -32602 (Invalid params) error for a
// terminal/cancel naming a terminal that is unknown, has already exited, or
// is not cancellable, with message "the terminal is unknown or not
// cancellable" and data {"terminalId": id}, as the runtime answers when the
// Agent does not implement TerminalCanceller.
func TerminalNotCancellableError(terminalID schema.TerminalId) *acp.RPCError {
	return entityCancelError("terminalId", "terminal", string(terminalID))
}

func entityCancelError(key, label, id string) *acp.RPCError {
	err := &acp.RPCError{Code: -32602, Message: "the " + label + " is unknown or not cancellable"}
	err.Data = entityCancelData(key, id)
	return err
}

// entityCancelData encodes {key: id} as serde_json's json! macro does.
func entityCancelData(key, id string) json.RawMessage {
	data := []byte("{")
	data = append(data, jsonline.Quote([]byte(key))...)
	data = append(data, ':')
	data = append(data, jsonline.Quote([]byte(id))...)
	return append(data, '}')
}

// unhandledEntityCancel answers a tool_call/cancel or terminal/cancel the
// Agent does not serve, as the reference SDK's unhandled_v2_entity_cancel
// does: without decoding the request, -32602 whose data holds the request's
// toolCallId or terminalId when the params are an object carrying it as a
// string, and no data otherwise.
func unhandledEntityCancel(method string, raw json.RawMessage) *acp.RPCError {
	key, label := "terminalId", "terminal"
	if method == schema.ToolCallCancelMethodName {
		key, label = "toolCallId", "tool call"
	}
	err := &acp.RPCError{Code: -32602, Message: "the " + label + " is unknown or not cancellable"}
	if params, parseErr := serdejson.Parse(raw); parseErr == nil {
		if id, ok := params.Get(key); ok && id.Kind == serdejson.String {
			err.Data = entityCancelData(key, id.String)
		}
	}
	return err
}

// errForegroundCancelled is the cause ForegroundPermissions gives the
// requests it cancels.
var errForegroundCancelled = errors.New("foreground work cancelled")

// ForegroundPermissions tracks the permission requests an Agent sends for its
// sessions' foreground work, so that it can stop waiting for them when the
// work is cancelled. On session/cancel the v2 spec requires the Agent to stop
// waiting for the cancelled foreground work's pending permission requests,
// treat them as not approved, and cancel each with $/cancel_request so the
// Client can dismiss the prompt. Permission requests for work in the latest
// background list are not affected: send those with Client.RequestPermission
// directly, not through ForegroundPermissions.
//
// On session/close the Agent must cancel every pending permission request of
// the session with $/cancel_request, background ones included.
// ForegroundPermissions covers only the requests sent through it: cancel
// those with CancelSession, and the ones sent directly by cancelling their
// contexts.
//
// A typical Agent calls CancelSession from its Agent.CancelSession:
//
//	func (a *myAgent) CancelSession(n schema.CancelSessionNotification) error {
//		a.permissions.CancelSession(n.SessionID)
//		a.stopForegroundWork(n.SessionID)
//		return nil
//	}
//
// The zero value is ready to use.
type ForegroundPermissions struct {
	mu       sync.Mutex
	sequence uint64
	pending  map[schema.SessionId]map[uint64]context.CancelCauseFunc
}

// RequestPermission sends request through client as part of its session's
// foreground work. When CancelSession cancels it before RequestPermission
// returns, the request is cancelled with $/cancel_request and
// RequestPermission returns the cancelled outcome, which is not an approval,
// even if the Client's answer raced with the cancellation. A
// cancellation of ctx itself is returned as ctx's error, as Client returns
// it.
func (p *ForegroundPermissions) RequestPermission(ctx context.Context, client Client, request schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	id := p.register(request.SessionID, cancel)
	defer p.unregister(request.SessionID, id)

	response, err := client.RequestPermission(ctx, request)
	// An answer that raced with the cancellation is not an approval either.
	if errors.Is(context.Cause(ctx), errForegroundCancelled) {
		return schema.RequestPermissionResponse{Outcome: schema.RequestPermissionOutcome{
			Cancelled: &schema.CancelledPermissionOutcome{},
		}}, nil
	}
	return response, err
}

// CancelSession stops waiting for every pending foreground permission
// request of the session, which RequestPermission then reports as
// cancelled, and returns how many it cancelled. Requests sent after it
// returns are not affected.
func (p *ForegroundPermissions) CancelSession(sessionID schema.SessionId) int {
	p.mu.Lock()
	pending := p.pending[sessionID]
	delete(p.pending, sessionID)
	p.mu.Unlock()
	for _, cancel := range pending {
		cancel(errForegroundCancelled)
	}
	return len(pending)
}

func (p *ForegroundPermissions) register(sessionID schema.SessionId, cancel context.CancelCauseFunc) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending == nil {
		p.pending = make(map[schema.SessionId]map[uint64]context.CancelCauseFunc)
	}
	if p.pending[sessionID] == nil {
		p.pending[sessionID] = make(map[uint64]context.CancelCauseFunc)
	}
	p.sequence++
	p.pending[sessionID][p.sequence] = cancel
	return p.sequence
}

func (p *ForegroundPermissions) unregister(sessionID schema.SessionId, id uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pending[sessionID], id)
	if len(p.pending[sessionID]) == 0 {
		delete(p.pending, sessionID)
	}
}
