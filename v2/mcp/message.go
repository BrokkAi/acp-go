package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// MessageOutcome is the inner MCP outcome of one mcp/message request. Exactly
// one field is set: Result (which may be JSON null) or Error. An inner MCP
// error is a successful outer ACP response, never a Go error; binding failures
// are returned from Call as *acp.RPCError instead.
type MessageOutcome struct {
	Result json.RawMessage
	Error  *schema.McpError
}

// IsError reports whether the outcome carries an inner MCP error.
func (o MessageOutcome) IsError() bool { return o.Error != nil }

// MessageNotification is a provider-to-consumer notification for one active
// mcp/message request.
type MessageNotification struct {
	ServerID  string
	RequestID string
	Method    string
	Params    map[string]any
}

// MessageNotificationHandler receives notifications for one active call. It
// runs on the connection's read loop, so it must return promptly.
type MessageNotificationHandler func(MessageNotification) error

type messageKey struct {
	serverID  string
	requestID string
}

func nullableParams(params map[string]any) schema.Nullable[map[string]any] {
	if params == nil {
		return schema.Nullable[map[string]any]{}
	}
	return schema.Nullable[map[string]any]{Set: true, Value: params}
}

func paramsValue(params schema.Nullable[map[string]any]) map[string]any {
	if !params.Set || params.Null {
		return nil
	}
	return params.Value
}

// MessageClient is the consumer side of the request-scoped MCP-over-ACP
// binding. It sends mcp/message requests and routes request-scoped
// notifications to the call that owns them.
type MessageClient struct {
	connection *acpv2.Connection
	mu         sync.Mutex
	active     map[messageKey]MessageNotificationHandler
}

// NewMessageClient returns the consumer side for one connection. Install
// Notifications into that connection so provider notifications reach active
// calls.
func NewMessageClient(connection *acpv2.Connection) *MessageClient {
	return &MessageClient{connection: connection, active: map[messageKey]MessageNotificationHandler{}}
}

// Call sends one mcp/message request and returns its inner MCP outcome. The
// server ID, request ID, and inner MCP method are required; request IDs must be
// unique while active for the same server. Cancelling ctx cancels the outer ACP
// request, and the request is released on return so late notifications are
// dropped.
func (c *MessageClient) Call(ctx context.Context, serverID, requestID, method string, params map[string]any, onNotification MessageNotificationHandler) (MessageOutcome, error) {
	if strings.TrimSpace(serverID) == "" {
		return MessageOutcome{}, errors.New("mcp/message requires a server ID")
	}
	if strings.TrimSpace(requestID) == "" {
		return MessageOutcome{}, errors.New("mcp/message requires a request ID")
	}
	if strings.TrimSpace(method) == "" {
		return MessageOutcome{}, errors.New("mcp/message requires an inner MCP method")
	}
	if err := ctx.Err(); err != nil {
		return MessageOutcome{}, err
	}
	key := messageKey{serverID: serverID, requestID: requestID}
	c.mu.Lock()
	if _, exists := c.active[key]; exists {
		c.mu.Unlock()
		return MessageOutcome{}, fmt.Errorf("mcp/message request %q for server %q is already active", requestID, serverID)
	}
	c.active[key] = onNotification
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.active, key)
		c.mu.Unlock()
	}()

	request := schema.MessageMcpRequest{
		ServerID:  schema.McpServerAcpId(serverID),
		RequestID: schema.McpRequestId(requestID),
		Method:    method,
		Params:    nullableParams(params),
	}
	var response schema.MessageMcpResponse
	if err := c.connection.Call(ctx, schema.McpMessageMethodName, request, &response); err != nil {
		return MessageOutcome{}, err
	}
	switch {
	case response.Error != nil:
		return MessageOutcome{Error: &response.Error.Error}, nil
	case response.Result != nil:
		return MessageOutcome{Result: response.Result.Result}, nil
	default:
		return MessageOutcome{}, errors.New("mcp/message response carried no inner MCP outcome")
	}
}

// Notifications wraps a notification handler so request-scoped mcp/message
// notifications reach the active call that owns them. Unknown, late, and
// malformed notifications are dropped; every other method is delegated to
// next.
func (c *MessageClient) Notifications(next acp.Notifications) acp.Notifications {
	return func(method string, raw json.RawMessage) error {
		if method != schema.McpMessageMethodName {
			if next != nil {
				return next(method, raw)
			}
			return nil
		}
		var notification schema.MessageMcpNotification
		if err := json.Unmarshal(raw, &notification); err != nil {
			return nil
		}
		c.mu.Lock()
		handler := c.active[messageKey{serverID: string(notification.ServerID), requestID: string(notification.RequestID)}]
		c.mu.Unlock()
		if handler == nil {
			return nil
		}
		return handler(MessageNotification{
			ServerID:  string(notification.ServerID),
			RequestID: string(notification.RequestID),
			Method:    notification.Method,
			Params:    paramsValue(notification.Params),
		})
	}
}

// MessageRequest is one agent-to-provider MCP operation delivered to a
// registered MessageService. Notify sends a request-scoped notification tied
// to this request; it returns an error once the request completed.
type MessageRequest struct {
	ServerID  string
	RequestID string
	Method    string
	Params    map[string]any
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

type activeMessage struct {
	serverID string
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
}

// MessageRouter is the provider side of the request-scoped MCP-over-ACP
// binding. It serves mcp/message requests for explicitly registered server IDs
// and answers binding failures with the protocol's outer ACP error codes.
type MessageRouter struct {
	connection *acpv2.Connection
	mu         sync.Mutex
	services   map[string]MessageService
	active     map[messageKey]*activeMessage
}

// NewMessageRouter returns the provider side for one connection. The
// connection carries request-scoped notifications emitted by services.
func NewMessageRouter(connection *acpv2.Connection) *MessageRouter {
	return &MessageRouter{
		connection: connection,
		services:   map[string]MessageService{},
		active:     map[messageKey]*activeMessage{},
	}
}

// Register binds one server ID to a service. A server ID cannot be rebound
// while it is registered.
func (r *MessageRouter) Register(serverID string, service MessageService) error {
	if strings.TrimSpace(serverID) == "" {
		return errors.New("mcp server ID is required")
	}
	if service == nil {
		return errors.New("mcp message service is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.services[serverID]; exists {
		return fmt.Errorf("mcp server %q is already registered", serverID)
	}
	r.services[serverID] = service
	return nil
}

// Unregister releases a server registration and cancels its active requests.
// The active request IDs stay owned until each service returns.
func (r *MessageRouter) Unregister(serverID string) {
	r.mu.Lock()
	delete(r.services, serverID)
	cancels := make([]context.CancelFunc, 0)
	for _, active := range r.active {
		if active.serverID == serverID {
			cancels = append(cancels, active.cancel)
		}
	}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
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
		var request schema.MessageMcpRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, &acp.RPCError{Code: -32602, Message: "malformed mcp/message envelope: " + err.Error()}
		}
		serverID := string(request.ServerID)
		requestID := string(request.RequestID)
		if strings.TrimSpace(serverID) == "" || strings.TrimSpace(requestID) == "" || strings.TrimSpace(request.Method) == "" {
			return nil, &acp.RPCError{Code: -32602, Message: "mcp/message requires serverId, requestId, and method"}
		}
		key := messageKey{serverID: serverID, requestID: requestID}
		r.mu.Lock()
		service, registered := r.services[serverID]
		if !registered {
			r.mu.Unlock()
			return nil, &acp.RPCError{Code: -33001, Message: fmt.Sprintf("mcp server %q is not registered", serverID)}
		}
		if _, duplicate := r.active[key]; duplicate {
			r.mu.Unlock()
			return nil, &acp.RPCError{Code: -32602, Message: fmt.Sprintf("mcp/message request %q for server %q is already active", requestID, serverID)}
		}
		requestCtx, cancel := context.WithCancel(ctx)
		active := &activeMessage{serverID: serverID, cancel: cancel}
		r.active[key] = active
		r.mu.Unlock()
		defer func() {
			active.mu.Lock()
			active.closed = true
			active.mu.Unlock()
			cancel()
			r.mu.Lock()
			delete(r.active, key)
			r.mu.Unlock()
		}()

		notify := func(notificationMethod string, params map[string]any) error {
			if strings.TrimSpace(notificationMethod) == "" {
				return errors.New("mcp/message notification requires an inner MCP method")
			}
			active.mu.Lock()
			defer active.mu.Unlock()
			if active.closed {
				return fmt.Errorf("mcp/message request %q for server %q is no longer active", requestID, serverID)
			}
			return r.connection.Notify(requestCtx, schema.McpMessageMethodName, schema.MessageMcpNotification{
				ServerID:  schema.McpServerAcpId(serverID),
				RequestID: schema.McpRequestId(requestID),
				Method:    notificationMethod,
				Params:    nullableParams(params),
			})
		}

		outcome, err := service.Message(requestCtx, MessageRequest{
			ServerID:  serverID,
			RequestID: requestID,
			Method:    request.Method,
			Params:    paramsValue(request.Params),
			Notify:    notify,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || requestCtx.Err() != nil {
				return nil, &acp.RPCError{Code: -32800, Message: "request cancelled"}
			}
			return nil, &acp.RPCError{Code: -33002, Message: err.Error()}
		}
		if outcome.Error != nil {
			return schema.MessageMcpResponse{Error: &schema.MessageMcpResponseError{Error: *outcome.Error}}, nil
		}
		return schema.MessageMcpResponse{Result: &schema.MessageMcpResponseResult{Result: outcome.Result}}, nil
	}
}
