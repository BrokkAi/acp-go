/*
Package unstable exposes narrow, opt-in typed facades for ACP's optional
("unstable") methods: agent provider configuration, session fork, and
session archive, status, and title.

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

On the agent side, Install serves the handler interfaces in this package on
the agent runtime and advertises their capabilities:

	runtime := agent.New(implementation)
	if err := unstable.Install(runtime, implementation); err != nil {
		return err
	}
*/
package unstable

import (
	"context"
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
// Additional directories need sessionCapabilities.additionalDirectories, which
// the protocol requires before a client sends them, and must be absolute.
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
	if len(request.AdditionalDirectories) > 0 && !advertisesAdditionalDirectories(initialization) {
		return result, fmt.Errorf("agent did not advertise additionalDirectories support")
	}
	for _, directory := range request.AdditionalDirectories {
		if err := acpvalidate.AbsolutePath("fork additional directory", directory); err != nil {
			return result, err
		}
	}
	if len(request.MCPServers) > 0 {
		return result, fmt.Errorf("MCP servers require the explicit github.com/BrokkAi/acp-go/mcp package")
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

func advertisesAdditionalDirectories(initialization schema.InitializeResponse) bool {
	capabilities := initialization.AgentCapabilities
	return capabilities != nil && capabilities.SessionCapabilities != nil && capabilities.SessionCapabilities.AdditionalDirectories != nil
}
