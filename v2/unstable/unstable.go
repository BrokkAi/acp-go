package unstable

import (
	"context"
	"fmt"

	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	"github.com/BrokkAi/acp-go/internal/v2session"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// requireProviders gates providers/* methods on capabilities.providers.
func requireProviders(initialization schema.InitializeResponse) error {
	if initialization.Capabilities == nil || initialization.Capabilities.Providers == nil {
		return fmt.Errorf("agent did not advertise provider configuration support")
	}
	return nil
}

// ListProviders returns the configurable providers the agent advertised,
// gated on capabilities.providers.
func ListProviders(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse) (schema.ListProvidersResponse, error) {
	var result schema.ListProvidersResponse
	if err := requireProviders(initialization); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.ProvidersListMethodName, schema.ListProvidersRequest{}, &result)
}

// SetProvider installs or replaces one provider routing configuration, gated
// on capabilities.providers. The provider ID must be non-blank.
func SetProvider(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.SetProviderRequest) (schema.SetProviderResponse, error) {
	var result schema.SetProviderResponse
	if err := requireProviders(initialization); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("provider ID", string(request.ProviderID)); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.ProvidersSetMethodName, request, &result)
}

// DisableProvider removes one provider routing configuration, gated on
// capabilities.providers. The provider ID must be non-blank.
func DisableProvider(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, providerID string) (schema.DisableProviderResponse, error) {
	var result schema.DisableProviderResponse
	if err := requireProviders(initialization); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("provider ID", providerID); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.ProvidersDisableMethodName, schema.DisableProviderRequest{ProviderID: schema.ProviderId(providerID)}, &result)
}

// ForkSession sends session/fork, gated on capabilities.session.fork. It
// requires a non-blank source session ID and an absolute cwd; additional
// directories need capabilities.session.additionalDirectories and must each be
// absolute. Inline MCP servers are rejected so MCP stays behind the explicit
// github.com/BrokkAi/acp-go/v2/mcp import, whose ForkSession accepts them. The
// response must carry a non-blank forked session ID.
//
// request.SystemPrompt is forwarded unchanged; this package does not yet check
// it against capabilities.session.systemPrompt.
func ForkSession(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.ForkSessionRequest) (schema.ForkSessionResponse, error) {
	var result schema.ForkSessionResponse
	if err := v2session.ValidateForkRequest(initialization, request); err != nil {
		return result, err
	}
	if len(request.MCPServers) > 0 {
		return result, fmt.Errorf("MCP servers require the explicit github.com/BrokkAi/acp-go/v2/mcp package")
	}
	if err := connection.Call(ctx, schema.SessionForkMethodName, request, &result); err != nil {
		return result, err
	}
	if err := v2session.ValidateForkResponse(result); err != nil {
		return result, err
	}
	return result, nil
}
