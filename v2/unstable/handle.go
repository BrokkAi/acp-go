package unstable

import (
	"context"
	"encoding/json"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/v2session"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
)

// ProviderHandler serves the agent side of providers/list, providers/set, and
// providers/disable. An agent that implements it should advertise
// capabilities.providers in its initialize response.
type ProviderHandler interface {
	ListProviders(context.Context, schema.ListProvidersRequest) (schema.ListProvidersResponse, error)
	SetProvider(context.Context, schema.SetProviderRequest) (schema.SetProviderResponse, error)
	DisableProvider(context.Context, schema.DisableProviderRequest) (schema.DisableProviderResponse, error)
}

// ForkHandler serves the agent side of session/fork. An agent that implements
// it should advertise capabilities.session.fork in its initialize response.
// The returned response must carry the new session's non-blank ID; Handle
// answers -32603 instead of sending a response without one.
type ForkHandler interface {
	ForkSession(context.Context, schema.ForkSessionRequest) (schema.ForkSessionResponse, error)
}

// Handle composes the draft-v2 unstable agent-side dispatch with another
// handler. Pass nil for next when unstable methods are the only ones hosted.
//
// handler may implement any subset of the handler interfaces in this package.
// A method this package owns whose interface handler does not implement
// answers -32601 (method not found); malformed params answer -32602 (invalid
// params) without reaching the handler. Every other method is delegated to
// next unchanged, or answers -32601 when next is nil.
//
// Handle does not check capabilities: the agent decides what it advertises in
// its initialize response, and should implement the matching interfaces.
func Handle(next acp.Handler, handler any) acp.Handler {
	bound := make(map[string]acp.Handler, len(routes))
	for method, bind := range routes {
		bound[method] = bind(handler)
	}
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		serve, owned := bound[method]
		switch {
		case serve != nil:
			return serve(ctx, method, raw)
		case owned, next == nil:
			return nil, methodNotFound(method)
		default:
			return next(ctx, method, raw)
		}
	}
}

// routes maps every method Handle owns to the binding that serves it from a
// handler. Serving another unstable method takes its handler interface and one
// entry here.
var routes = map[string]binding{
	schema.ProvidersListMethodName:    route(ProviderHandler.ListProviders, nil),
	schema.ProvidersSetMethodName:     route(ProviderHandler.SetProvider, nil),
	schema.ProvidersDisableMethodName: route(ProviderHandler.DisableProvider, nil),
	schema.SessionForkMethodName:      route(ForkHandler.ForkSession, checkForkResponse),
}

// binding adapts a handler to one method, or returns nil when the handler
// does not implement that method's interface.
type binding func(handler any) acp.Handler

// route builds the binding for one interface method: it decodes the params,
// calls the method, and runs check, when non-nil, on a successful response
// before it is sent.
func route[H, Request, Response any](method func(H, context.Context, Request) (Response, error), check func(Response) error) binding {
	return func(handler any) acp.Handler {
		implementation, ok := handler.(H)
		if !ok {
			return nil
		}
		return func(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
			var request Request
			if err := json.Unmarshal(raw, &request); err != nil {
				return nil, invalidParams(err)
			}
			response, err := method(implementation, ctx, request)
			if err != nil {
				return nil, err
			}
			if check != nil {
				if err := check(response); err != nil {
					return nil, err
				}
			}
			return response, nil
		}
	}
}

func checkForkResponse(response schema.ForkSessionResponse) error {
	if err := v2session.ValidateForkResponse(response); err != nil {
		return &acp.RPCError{Code: -32603, Message: "agent returned a session/fork response without the forked session ID"}
	}
	return nil
}

// methodNotFound answers -32601 in the reference SDK's shape: the generic
// message, with the method name as data.
func methodNotFound(method string) error {
	data, _ := json.Marshal(method)
	return &acp.RPCError{Code: -32601, Message: "Method not found", Data: data}
}

// invalidParams answers -32602 in the reference SDK's shape. Unlike the
// reference, data does not echo the params back: providers/set carries
// authorization headers.
func invalidParams(err error) error {
	data, _ := json.Marshal(map[string]string{"error": err.Error(), "phase": "deserialization"})
	return &acp.RPCError{Code: -32602, Message: "Invalid params", Data: data}
}
