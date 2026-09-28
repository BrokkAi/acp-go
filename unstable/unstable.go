/*
Package unstable exposes narrow, opt-in typed facades for ACP's optional
("unstable") methods: agent provider configuration and session fork.

Every method here lives behind its own package import because the ACP release
marks the surface unstable and may change or remove it. Nothing in this package
is reachable from the stable `acp` or `v2` facades, and the generated bindings
stay in `schema/unstable` and `schema/v2/unstable`.

Capability gating reads the unstable initialize response, whose capability
objects carry fields the stable schema does not publish. Decode the agent's
initialize result into unstable.InitializeResponse to use these facades:

	var initialization unstable.InitializeResponse
	json.Unmarshal(encodedInitializeResult, &initialization)
	providers, err := unstable.ListProviders(ctx, connection, initialization)
*/
package unstable

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

// requireProviders gates providers/* methods on the advertised capability.
func requireProviders(initialization schema.InitializeResponse) error {
	if initialization.AgentCapabilities == nil || initialization.AgentCapabilities.Providers == nil {
		return fmt.Errorf("agent did not advertise provider configuration support")
	}
	return nil
}

// ListProviders returns the configurable providers the agent advertised.
func ListProviders(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse) (schema.ListProvidersResponse, error) {
	var result schema.ListProvidersResponse
	if err := requireProviders(initialization); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.ProvidersListMethodName, schema.ListProvidersRequest{}, &result)
}

// SetProvider installs or replaces one provider routing configuration.
func SetProvider(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, request schema.SetProviderRequest) (schema.SetProviderResponse, error) {
	var result schema.SetProviderResponse
	if err := requireProviders(initialization); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("provider ID", string(request.ProviderID)); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.ProvidersSetMethodName, request, &result)
}

// DisableProvider removes one provider routing configuration.
func DisableProvider(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, providerID string) (schema.DisableProviderResponse, error) {
	var result schema.DisableProviderResponse
	if err := requireProviders(initialization); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("provider ID", providerID); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.ProvidersDisableMethodName, schema.DisableProviderRequest{ProviderID: schema.ProviderId(providerID)}, &result)
}

// ForkSession forks an existing session, gated on sessionCapabilities.fork.
func ForkSession(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, request schema.ForkSessionRequest) (schema.ForkSessionResponse, error) {
	var result schema.ForkSessionResponse
	if err := requireFork(initialization); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("session ID", string(request.SessionID)); err != nil {
		return result, err
	}
	if err := acpvalidate.AbsolutePath("fork working directory", request.Cwd); err != nil {
		return result, err
	}
	for _, directory := range request.AdditionalDirectories {
		if err := acpvalidate.AbsolutePath("fork additional directory", directory); err != nil {
			return result, err
		}
	}
	if len(request.MCPServers) > 0 {
		return result, fmt.Errorf("MCP servers require an explicit MCP package import")
	}
	if err := connection.Call(ctx, schema.SessionForkMethodName, request, &result); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("forked session ID", string(result.SessionID)); err != nil {
		return result, err
	}
	return result, nil
}

func requireFork(initialization schema.InitializeResponse) error {
	var capabilities *schema.SessionCapabilities
	if initialization.AgentCapabilities != nil {
		capabilities = initialization.AgentCapabilities.SessionCapabilities
	}
	if capabilities == nil || capabilities.Fork == nil {
		return fmt.Errorf("agent did not advertise session/fork support")
	}
	return nil
}

// ProviderHandler serves the agent side of providers/list, providers/set, and
// providers/disable.
type ProviderHandler interface {
	ListProviders(context.Context, schema.ListProvidersRequest) (schema.ListProvidersResponse, error)
	SetProvider(context.Context, schema.SetProviderRequest) (schema.SetProviderResponse, error)
	DisableProvider(context.Context, schema.DisableProviderRequest) (schema.DisableProviderResponse, error)
}

// ForkHandler serves the agent side of session/fork.
type ForkHandler interface {
	ForkSession(context.Context, schema.ForkSessionRequest) (schema.ForkSessionResponse, error)
}

// Handle composes the unstable agent-side dispatch with another handler. Pass
// nil for next when unstable methods are the only ones hosted. Each handler is
// optional: a method whose handler is missing answers method-not-found, and any
// method outside this package is delegated to next unchanged.
func Handle(next acp.Handler, handler any) acp.Handler {
	providers, _ := handler.(ProviderHandler)
	fork, _ := handler.(ForkHandler)
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case schema.ProvidersListMethodName:
			if providers == nil {
				return nil, methodNotFound(method)
			}
			request, err := decode[schema.ListProvidersRequest](raw)
			if err != nil {
				return nil, err
			}
			return providers.ListProviders(ctx, request)
		case schema.ProvidersSetMethodName:
			if providers == nil {
				return nil, methodNotFound(method)
			}
			request, err := decode[schema.SetProviderRequest](raw)
			if err != nil {
				return nil, err
			}
			return providers.SetProvider(ctx, request)
		case schema.ProvidersDisableMethodName:
			if providers == nil {
				return nil, methodNotFound(method)
			}
			request, err := decode[schema.DisableProviderRequest](raw)
			if err != nil {
				return nil, err
			}
			return providers.DisableProvider(ctx, request)
		case schema.SessionForkMethodName:
			if fork == nil {
				return nil, methodNotFound(method)
			}
			request, err := decode[schema.ForkSessionRequest](raw)
			if err != nil {
				return nil, err
			}
			return fork.ForkSession(ctx, request)
		default:
			if next == nil {
				return nil, methodNotFound(method)
			}
			return next(ctx, method, raw)
		}
	}
}

func decode[T any](raw json.RawMessage) (T, error) {
	var request T
	if err := json.Unmarshal(raw, &request); err != nil {
		return request, &acp.RPCError{Code: -32602, Message: err.Error()}
	}
	return request, nil
}

func methodNotFound(method string) error {
	return &acp.RPCError{Code: -32601, Message: "unsupported method: " + method}
}
