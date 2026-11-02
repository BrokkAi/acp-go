// Package unstabledispatch is the agent-side dispatch shared by the v1
// github.com/BrokkAi/acp-go/unstable and draft-v2
// github.com/BrokkAi/acp-go/v2/unstable Handle functions, so both versions
// route, decode, and fail identically.
//
// It mirrors the reference SDK's handler chaining: a method is served by the
// first handler that implements it, and anything left unhandled reaches the
// next handler, ending in method-not-found. Error shapes follow the reference
// SDK's constructors.
package unstabledispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/BrokkAi/acp-go"
)

// MergeOverrides binds handler to each of overrides, the stable runtime
// methods whose unstable form an Install serves, and returns a copy of
// existing (a runtime's Overrides) with the bound methods added. It refuses a
// method existing already serves, so that Install never silently replaces an
// override the caller set. With nothing bound it returns existing unchanged.
func MergeOverrides(existing map[string]acp.Handler, handler any, overrides map[string]Binding) (map[string]acp.Handler, error) {
	methods := make([]string, 0, len(overrides))
	for method := range overrides {
		methods = append(methods, method)
	}
	// A fixed order makes the reported conflict deterministic.
	sort.Strings(methods)
	bound := make(map[string]acp.Handler, len(methods))
	for _, method := range methods {
		serve := overrides[method](handler)
		if serve == nil {
			continue
		}
		if _, taken := existing[method]; taken {
			return nil, fmt.Errorf("runtime override for %s is already set", method)
		}
		bound[method] = serve
	}
	if len(bound) == 0 {
		return existing, nil
	}
	for method, serve := range existing {
		bound[method] = serve
	}
	return bound, nil
}

// Binding adapts a handler to one method, or returns nil when the handler
// does not implement that method's interface.
type Binding func(handler any) acp.Handler

// Route builds the Binding for one interface method, given as a method
// expression such as ForkHandler.ForkSession. It decodes the params, calls
// the method, and runs check, when non-nil, on a successful response before it
// is sent.
func Route[H, Request, Response any](method func(H, context.Context, Request) (Response, error), check func(Response) error) Binding {
	return RouteChecked(method, nil, nil, check)
}

// RouteChecked is Route with two more checks, each run when non-nil: strict
// on the raw params before they are decoded, whose error answers as
// InvalidParams, for values the reference SDK refuses to deserialize but
// encoding/json accepts; and accept on the decoded request before the
// handler, whose error is answered as it is.
func RouteChecked[H, Request, Response any](method func(H, context.Context, Request) (Response, error), strict func(json.RawMessage) error, accept func(context.Context, Request) error, check func(Response) error) Binding {
	return func(handler any) acp.Handler {
		implementation, ok := handler.(H)
		if !ok {
			return nil
		}
		return func(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
			if strict != nil {
				if err := strict(raw); err != nil {
					return nil, InvalidParams(err, raw)
				}
			}
			var request Request
			if err := json.Unmarshal(raw, &request); err != nil {
				return nil, InvalidParams(err, raw)
			}
			if accept != nil {
				if err := accept(ctx, request); err != nil {
					return nil, err
				}
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

// Handle serves each method in routes that handler implements and passes
// every other request, including a routed method handler does not implement,
// to next. With no next, those requests answer MethodNotFound.
func Handle(next acp.Handler, handler any, routes map[string]Binding) acp.Handler {
	bound := make(map[string]acp.Handler, len(routes))
	for method, bind := range routes {
		if serve := bind(handler); serve != nil {
			bound[method] = serve
		}
	}
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if serve, ok := bound[method]; ok {
			return serve(ctx, method, raw)
		}
		if next != nil {
			return next(ctx, method, raw)
		}
		return nil, MethodNotFound(method)
	}
}

// MethodNotFound is the reference SDK's -32601: the generic message, with the
// method name as data.
func MethodNotFound(method string) error {
	data, _ := json.Marshal(method)
	return &acp.RPCError{Code: -32601, Message: "Method not found", Data: data}
}

// InvalidParams is the reference SDK's -32602 for params that do not decode
// (json_cast_params): data carries the decode error, the received params (null
// when absent), and the failing phase.
func InvalidParams(err error, raw json.RawMessage) error {
	params := json.RawMessage("null")
	if len(raw) > 0 {
		params = raw
	}
	data, _ := json.Marshal(struct {
		Error string          `json:"error"`
		JSON  json.RawMessage `json:"json"`
		Phase string          `json:"phase"`
	}{err.Error(), params, "deserialization"})
	return &acp.RPCError{Code: -32602, Message: "Invalid params", Data: data}
}

// InternalError is the reference SDK's -32603: the generic message, with the
// detail as string data.
func InternalError(detail string) error {
	data, _ := json.Marshal(detail)
	return &acp.RPCError{Code: -32603, Message: "Internal error", Data: data}
}
