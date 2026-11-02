package unstable

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	"github.com/BrokkAi/acp-go/internal/unstabledispatch"
	stable "github.com/BrokkAi/acp-go/schema"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

// ProviderHandler serves the agent side of providers/list, providers/set, and
// providers/disable. Advertise announces agentCapabilities.providers for it.
type ProviderHandler interface {
	ListProviders(context.Context, schema.ListProvidersRequest) (schema.ListProvidersResponse, error)
	SetProvider(context.Context, schema.SetProviderRequest) (schema.SetProviderResponse, error)
	DisableProvider(context.Context, schema.DisableProviderRequest) (schema.DisableProviderResponse, error)
}

// ForkHandler serves the agent side of session/fork. Advertise announces
// sessionCapabilities.fork for it. The returned response must carry the new
// session's non-blank ID; Handle answers -32603 instead of sending a response
// without one. A systemPrompt on the request is checked as on the other
// lifecycle requests before ForkSession runs (see SystemPromptHandler), so
// one the connection's initialize response did not advertise never reaches
// it.
type ForkHandler interface {
	ForkSession(context.Context, schema.ForkSessionRequest) (schema.ForkSessionResponse, error)
}

// Handle composes the unstable agent-side dispatch with another handler, in
// the reference SDK's handler-chaining order. Each method handler implements
// (see the handler interfaces in this package) is served here; every other
// request, including an unstable method handler does not implement, goes to
// next. With no next, those requests answer -32601 (method not found, with
// the method name as data).
//
// Params that do not decode answer -32602 in the reference SDK's shape, whose
// data echoes the received params, and never reach the handler.
//
// Install wires Handle, Advertise, and the runtime overrides together and is
// the usual way to serve this package's methods. Handle alone is the agent
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
	schema.SessionForkMethodName: systemPromptRoute(ForkHandler.ForkSession,
		func(request schema.ForkSessionRequest) *SystemPrompt { return request.SystemPrompt }, checkForkResponse),
	schema.SessionArchiveMethodName:   unstabledispatch.Route(ArchiveHandler.ArchiveSession, nil),
	schema.SessionUnarchiveMethodName: unstabledispatch.Route(ArchiveHandler.UnarchiveSession, nil),
	schema.SessionStatusMethodName:    unstabledispatch.Route(StatusHandler.SessionStatus, nil),
	schema.SessionSetTitleMethodName:  unstabledispatch.Route(TitleHandler.SetSessionTitle, nil),
}

func checkForkResponse(response schema.ForkSessionResponse) error {
	if err := acpvalidate.Identifier("forked session ID", string(response.SessionID)); err != nil {
		return unstabledispatch.InternalError("agent returned a session/fork response without the forked session ID")
	}
	return nil
}

// Advertise returns a hook for the agent runtime's InitializeResult. It
// carries the agent's stable initialize response over to the unstable
// InitializeResponse field for field (through its JSON form, which the
// unstable schema extends), advertises agentCapabilities.providers when
// handler implements ProviderHandler and the sessionCapabilities entry of each
// session handler it implements (fork for ForkHandler, status for
// StatusHandler, and title for TitleHandler), keeping any value the agent
// already set, and then applies each customize function in order.
//
// Advertise never announces sessionCapabilities.archive: an agent that does
// must also serve the archived session/list filter, which only Install wires
// up. Install announces it for an ArchiveHandler, and only when the agent
// advertised sessionCapabilities.list, which the protocol requires alongside
// it; neither advertises list on the agent's behalf. Advertise never
// announces sessionCapabilities.systemPrompt either, because the stable
// session/new, session/load, and session/resume the runtime dispatches drop
// the prompt; Install announces it for a SystemPromptHandler that serves the
// prompt on every advertised lifecycle request.
func Advertise(handler any, customize ...func(*schema.InitializeResponse) error) func(stable.InitializeResponse) (any, error) {
	return advertise(handler, false, customize)
}

// advertise is Advertise; install also announces the archive and systemPrompt
// capabilities, which only Install, which serves the archived filter and the
// system prompt through runtime overrides, may do, and then refuses a
// systemPrompt capability the handler does not serve.
func advertise(handler any, install bool, customize []func(*schema.InitializeResponse) error) func(stable.InitializeResponse) (any, error) {
	return func(response stable.InitializeResponse) (any, error) {
		encoded, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}
		var extended schema.InitializeResponse
		if err := json.Unmarshal(encoded, &extended); err != nil {
			return nil, fmt.Errorf("convert initialize response to the unstable schema: %w", err)
		}
		_, providers := handler.(ProviderHandler)
		_, fork := handler.(ForkHandler)
		if (providers || fork) && extended.AgentCapabilities == nil {
			extended.AgentCapabilities = &schema.AgentCapabilities{}
		}
		if providers && extended.AgentCapabilities.Providers == nil {
			extended.AgentCapabilities.Providers = &schema.ProvidersCapabilities{}
		}
		if fork {
			if extended.AgentCapabilities.SessionCapabilities == nil {
				extended.AgentCapabilities.SessionCapabilities = &schema.SessionCapabilities{}
			}
			if extended.AgentCapabilities.SessionCapabilities.Fork == nil {
				extended.AgentCapabilities.SessionCapabilities.Fork = &schema.SessionForkCapabilities{}
			}
		}
		advertiseSessionHandlers(handler, install, &extended)
		if install {
			advertiseSystemPrompt(handler, &extended)
		}
		for _, apply := range customize {
			if err := apply(&extended); err != nil {
				return nil, err
			}
		}
		if install {
			if err := checkSystemPromptServed(handler, extended); err != nil {
				return nil, err
			}
		}
		return extended, nil
	}
}
