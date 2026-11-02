package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/mcpserver"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// MessageRequest is one agent-to-provider MCP operation delivered to a
// registered MessageService. Notify sends a request-scoped notification tied
// to this request; it returns an error once the request completed or was
// cancelled.
type MessageRequest struct {
	ServerID  string
	RequestID string
	Method    string
	Params    map[string]any
	// RawParams is the inner params object exactly as received, or nil when
	// the request carried none or null. Params is decoded from it.
	RawParams json.RawMessage
	Notify    func(method string, params map[string]any) error
}

// MessageService serves the mcp/message requests of one registered server ID.
type MessageService interface {
	Message(context.Context, MessageRequest) (MessageOutcome, error)
}

// MessageServiceFunc adapts a function to MessageService.
type MessageServiceFunc func(context.Context, MessageRequest) (MessageOutcome, error)

// Message implements MessageService.
func (f MessageServiceFunc) Message(ctx context.Context, request MessageRequest) (MessageOutcome, error) {
	return f(ctx, request)
}

// MessageRouter is the provider side of the request-scoped MCP-over-ACP
// binding for one connection. It serves mcp/message requests for registered
// server IDs and answers binding failures with the protocol's outer ACP error
// codes: -32602 for a malformed envelope or a duplicate active request ID,
// -32800 when the caller cancels, -33000 when the SetMaxActive limit is hit,
// -33001 for an unknown or removed server, and -33002 when a service fails
// without an MCP outcome.
//
// Low-level services registered with Register see every request unchanged.
// Servers attached with Attach, or with the attached-session helpers, get a
// generated server ID, validated MCP 2026-07-28 request metadata, and a
// RequestContext.
//
// An operation is cancelled when its outer request is cancelled, its
// registration is removed, or the connection's input closes. The router waits
// for the service to return, holding the logical request ID until then, and
// discards a result that loses that race.
type MessageRouter struct {
	connection *acpv2.Connection
	binding    *mcpserver.Binding
}

// NewMessageRouter returns the provider side for one connection. The
// connection carries request-scoped notifications emitted by services and
// the session setup requests of the attached-session helpers.
func NewMessageRouter(connection *acpv2.Connection) *MessageRouter {
	var notifier mcpserver.Connection
	if connection != nil {
		notifier = connection
	}
	return &MessageRouter{connection: connection, binding: mcpserver.NewBinding(notifier, codec)}
}

// Register binds one server ID to a low-level service. A server ID is never
// rebound on the router: not while it is registered, and not after it was
// unregistered.
func (r *MessageRouter) Register(serverID string, service MessageService) error {
	if service == nil {
		return errors.New("mcp message service is required")
	}
	_, err := r.binding.Register(serverID, messageService{service}, false)
	return err
}

// Unregister releases a server registration and cancels its active requests,
// which are answered with -33001. It does not wait: the active request IDs
// stay owned until each service returns. UnregisterAndWait also waits.
func (r *MessageRouter) Unregister(serverID string) {
	r.binding.Unregister(serverID)
}

// UnregisterAndWait releases a server registration, cancels its active
// requests, and waits until every one of them has returned or ctx is done.
func (r *MessageRouter) UnregisterAndWait(ctx context.Context, serverID string) error {
	return r.binding.UnregisterAndWait(ctx, serverID)
}

// SetMaxActive limits how many mcp/message requests may be active at once
// across all registrations; further requests are refused with -33000
// (ErrorCodeResourceLimitExceeded). Zero, the default, means no limit.
// acp.Connection runs at most 32 inbound requests at a time and closes the
// connection beyond that, so a limit below 32 keeps room for other requests.
func (r *MessageRouter) SetMaxActive(limit int) {
	r.binding.SetMaxActive(limit)
}

// Handle composes the provider-side dispatch with another handler. Pass nil
// for next when mcp/message is the only hosted method. Requests for other
// methods are delegated to next; a missing next answers method-not-found.
func (r *MessageRouter) Handle(next acp.Handler) acp.Handler {
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if method != schema.McpMessageMethodName {
			if next != nil {
				return next(ctx, method, raw)
			}
			return nil, &acp.RPCError{Code: -32601, Message: "unsupported method: " + method}
		}
		return r.binding.Handle(ctx, raw)
	}
}

// messageService adapts a low-level MessageService to the binding.
type messageService struct{ service MessageService }

func (s messageService) Execute(ctx context.Context, request mcpserver.Request, rc *mcpserver.RequestContext) (mcpserver.Outcome, error) {
	var params map[string]any
	if request.Params != nil {
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return mcpserver.Outcome{}, err
		}
	}
	outcome, err := s.service.Message(ctx, MessageRequest{
		ServerID:  rc.ServerID(),
		RequestID: rc.RequestID(),
		Method:    request.Method,
		Params:    params,
		RawParams: request.Params,
		Notify: func(method string, params map[string]any) error {
			var value any
			if params != nil {
				value = params
			}
			return rc.Notify(ctx, method, value)
		},
	})
	if err != nil {
		return mcpserver.Outcome{}, err
	}
	if outcome.Error != nil {
		return mcpserver.Outcome{Error: &mcpserver.Error{Code: int(outcome.Error.Code), Message: outcome.Error.Message, Data: outcome.Error.Data}}, nil
	}
	return mcpserver.Outcome{Result: outcome.Result}, nil
}

// codec carries the binding over the draft v2 unstable mcp/message types.
var codec = mcpserver.Codec{
	Method: schema.McpMessageMethodName,
	Decode: func(raw json.RawMessage) (mcpserver.Envelope, error) {
		var request schema.MessageMcpRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return mcpserver.Envelope{}, err
		}
		var params struct {
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(raw, &params); err != nil {
			return mcpserver.Envelope{}, err
		}
		inner := bytes.TrimSpace(params.Params)
		if len(inner) == 0 || string(inner) == "null" {
			inner = nil
		}
		return mcpserver.Envelope{
			ServerID:  string(request.ServerID),
			RequestID: string(request.RequestID),
			Method:    request.Method,
			Params:    inner,
		}, nil
	},
	Response: func(outcome mcpserver.Outcome) (any, error) {
		if outcome.Error != nil {
			if outcome.Error.Code < math.MinInt32 || outcome.Error.Code > math.MaxInt32 {
				return nil, fmt.Errorf("inner MCP error code %d does not fit int32", outcome.Error.Code)
			}
			return schema.MessageMcpResponse{Error: &schema.MessageMcpResponseError{Error: schema.McpError{
				Code:    int32(outcome.Error.Code),
				Message: outcome.Error.Message,
				Data:    outcome.Error.Data,
			}}}, nil
		}
		result := outcome.Result
		if result == nil {
			result = json.RawMessage("null")
		}
		return schema.MessageMcpResponse{Result: &schema.MessageMcpResponseResult{Result: result}}, nil
	},
	Notification: func(serverID, requestID, method string, params json.RawMessage) (any, error) {
		notification := schema.MessageMcpNotification{
			ServerID:  schema.McpServerAcpId(serverID),
			RequestID: schema.McpRequestId(requestID),
			Method:    method,
		}
		if params != nil {
			var decoded map[string]any
			decoder := json.NewDecoder(bytes.NewReader(params))
			decoder.UseNumber()
			if err := decoder.Decode(&decoded); err != nil {
				return nil, err
			}
			notification.Params = schema.Nullable[map[string]any]{Set: true, Value: decoded}
		}
		return notification, nil
	},
}
