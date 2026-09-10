package v2

import (
	"context"
	"encoding/json"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// Permissions handles the baseline v2 session/request_permission method.
type Permissions interface {
	RequestPermission(context.Context, schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error)
}

// Elicitation handles elicitation/create requests. CompleteElicitation is an
// agent-to-client notification and is delivered through Notifications, not a
// request handler.
type Elicitation interface {
	CreateElicitation(context.Context, schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error)
}

// CancelPermission returns the explicit cancelled outcome used by the
// reference SDK's non-interactive one-shot client.
func CancelPermission() schema.RequestPermissionResponse {
	return schema.RequestPermissionResponse{Outcome: schema.RequestPermissionOutcome{
		Cancelled: &schema.RequestPermissionOutcomeCancelled{},
	}}
}

// HandlePermissions adds typed v2 permission dispatch to next. A nil host
// passes unmatched methods through unchanged.
func HandlePermissions(next acp.Handler, host Permissions) acp.Handler {
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if host == nil || method != schema.SessionRequestPermissionMethodName {
			return passToNext(ctx, next, method, raw)
		}
		request, err := decodeHostRequest[schema.RequestPermissionRequest](raw)
		if err != nil {
			return nil, err
		}
		return host.RequestPermission(ctx, request)
	}
}

// HandleElicitation adds typed v2 elicitation/create dispatch to next.
func HandleElicitation(next acp.Handler, host Elicitation) acp.Handler {
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if host == nil || method != schema.ElicitationCreateMethodName {
			return passToNext(ctx, next, method, raw)
		}
		request, err := decodeHostRequest[schema.CreateElicitationRequest](raw)
		if err != nil {
			return nil, err
		}
		return host.CreateElicitation(ctx, request)
	}
}

// HandleClientHost composes the typed v2 client request surface. Nil
// components are ignored.
func HandleClientHost(permissions Permissions, elicitation Elicitation) acp.Handler {
	return HandleElicitation(HandlePermissions(nil, permissions), elicitation)
}

func passToNext(ctx context.Context, next acp.Handler, method string, raw json.RawMessage) (any, error) {
	if next == nil {
		return nil, &acp.RPCError{Code: -32601, Message: "unsupported method: " + method}
	}
	return next(ctx, method, raw)
}

func decodeHostRequest[T any](raw json.RawMessage) (T, error) {
	var request T
	if err := json.Unmarshal(raw, &request); err != nil {
		return request, &acp.RPCError{Code: -32602, Message: err.Error()}
	}
	return request, nil
}
