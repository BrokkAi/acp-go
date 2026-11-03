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
	connection Caller
	mu         sync.Mutex
	active     map[messageKey]MessageNotificationHandler
}

// Caller sends one ACP request, as acp.Connection.Call does. The consumer
// side of MCP-over-ACP is the agent, so pass the agent's connection to its
// client: the *acp.Connection an agent serves on, or the
// github.com/BrokkAi/acp-go/v2/agent.Client a draft-v2 agent runtime hands its
// handlers. A github.com/BrokkAi/acp-go/v2.Connection is a client connection
// and follows the client's initialization lifecycle.
type Caller interface {
	Call(ctx context.Context, method string, params, result any) error
}

// NewMessageClient returns the consumer side for one connection. Install
// Notifications into that connection so provider notifications reach active
// calls.
func NewMessageClient(connection Caller) *MessageClient {
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
