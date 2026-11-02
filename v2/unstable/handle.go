package unstable

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/unstabledispatch"
	"github.com/BrokkAi/acp-go/internal/v2session"
	stable "github.com/BrokkAi/acp-go/schema/v2"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
)

// ProviderHandler serves the agent side of providers/list, providers/set, and
// providers/disable. Advertise announces capabilities.providers for it.
type ProviderHandler interface {
	ListProviders(context.Context, schema.ListProvidersRequest) (schema.ListProvidersResponse, error)
	SetProvider(context.Context, schema.SetProviderRequest) (schema.SetProviderResponse, error)
	DisableProvider(context.Context, schema.DisableProviderRequest) (schema.DisableProviderResponse, error)
}

// ForkHandler serves the agent side of session/fork. Advertise announces
// capabilities.session.fork for it. The returned response must carry the new
// session's non-blank ID; Handle answers -32603 instead of sending a response
// without one.
type ForkHandler interface {
	ForkSession(context.Context, schema.ForkSessionRequest) (schema.ForkSessionResponse, error)
}

// Handle composes the draft-v2 unstable agent-side dispatch with another
// handler, in the reference SDK's handler-chaining order. Each method handler
// implements (see the handler interfaces in this package) is served here;
// every other request, including an unstable method handler does not
// implement, goes to next. With no next, those requests answer -32601
// (method not found, with the method name as data).
//
// Params that do not decode answer -32602 in the reference SDK's shape, whose
// data echoes the received params, and never reach the handler.
//
// Install wires Handle, Advertise, and the runtime overrides together and is
// the usual way to serve this package's methods. Handle alone is the v2/agent
// runtime's Extensions, so that the runtime's initialization gate and version
// negotiation run in front of it:
//
//	runtime := agent.New(implementation)
//	runtime.Extensions = unstable.Handle(nil, implementation)
//	runtime.InitializeResult = unstable.Advertise(implementation)
func Handle(next acp.Handler, handler any) acp.Handler {
	return unstabledispatch.Handle(next, handler, routes)
}

// routes maps every method Handle serves to the binding that serves it from a
// handler. Serving another unstable method takes its handler interface, one
// entry here, and its capability in Advertise.
var routes = map[string]unstabledispatch.Binding{
	schema.ProvidersListMethodName:    unstabledispatch.Route(ProviderHandler.ListProviders, nil),
	schema.ProvidersSetMethodName:     unstabledispatch.Route(ProviderHandler.SetProvider, nil),
	schema.ProvidersDisableMethodName: unstabledispatch.Route(ProviderHandler.DisableProvider, nil),
	schema.SessionForkMethodName:      unstabledispatch.Route(ForkHandler.ForkSession, checkForkResponse),
	schema.SessionArchiveMethodName:   unstabledispatch.Route(ArchiveHandler.ArchiveSession, nil),
	schema.SessionUnarchiveMethodName: unstabledispatch.Route(ArchiveHandler.UnarchiveSession, nil),
	schema.SessionStatusMethodName:    unstabledispatch.Route(StatusHandler.SessionStatus, nil),
	schema.SessionSetTitleMethodName:  unstabledispatch.Route(TitleHandler.SetSessionTitle, nil),
}

func checkForkResponse(response schema.ForkSessionResponse) error {
	if err := v2session.ValidateForkResponse(response); err != nil {
		return unstabledispatch.InternalError("agent returned a session/fork response without the forked session ID")
	}
	return nil
}

// Advertise returns a hook for the v2/agent runtime's InitializeResult. It
// carries the agent's stable initialize response over to the unstable
// InitializeResponse field for field (through its JSON form, which the
// unstable schema extends), advertises capabilities.providers when handler
// implements ProviderHandler and the capabilities.session entry of each
// session handler it implements (fork for ForkHandler, status for
// StatusHandler, and title for TitleHandler), keeping any value the agent
// already set, and then applies each customize function in order.
//
// Advertise never announces capabilities.session.archive: an agent that does
// must also serve the archived session/list filter, which only Install wires
// up. Install announces it for an ArchiveHandler.
//
// The session entries are added only when the agent advertised the baseline
// session capabilities, which the runtime requires anyway; Advertise never
// advertises the baseline on the agent's behalf.
func Advertise(handler any, customize ...func(*schema.InitializeResponse) error) func(stable.InitializeResponse) (any, error) {
	return advertise(handler, false, customize)
}

// advertise is Advertise; archive also announces the archive capability,
// which only Install, which serves the archived filter, may do.
func advertise(handler any, archive bool, customize []func(*schema.InitializeResponse) error) func(stable.InitializeResponse) (any, error) {
	return func(response stable.InitializeResponse) (any, error) {
		encoded, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}
		var extended schema.InitializeResponse
		if err := json.Unmarshal(encoded, &extended); err != nil {
			return nil, fmt.Errorf("convert initialize response to the unstable schema: %w", err)
		}
		if _, ok := handler.(ProviderHandler); ok {
			if extended.Capabilities == nil {
				extended.Capabilities = &schema.AgentCapabilities{}
			}
			if extended.Capabilities.Providers == nil {
				extended.Capabilities.Providers = &schema.ProvidersCapabilities{}
			}
		}
		if _, ok := handler.(ForkHandler); ok && extended.Capabilities != nil && extended.Capabilities.Session != nil {
			if extended.Capabilities.Session.Fork == nil {
				extended.Capabilities.Session.Fork = &schema.SessionForkCapabilities{}
			}
		}
		advertiseSessionHandlers(handler, archive, &extended)
		for _, apply := range customize {
			if err := apply(&extended); err != nil {
				return nil, err
			}
		}
		return extended, nil
	}
}
