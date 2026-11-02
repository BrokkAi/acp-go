// Package v2session holds the draft-v2 unstable session-lifecycle checks
// shared by the github.com/BrokkAi/acp-go/v2/unstable and
// github.com/BrokkAi/acp-go/v2/mcp facades, so a rule added for one lifecycle
// path applies to every facade that sends it.
//
// MCP server validation is not here: v2/unstable rejects inline servers, and
// v2/mcp checks them against the advertised transports.
package v2session

import (
	"fmt"

	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
)

// ValidateForkRequest checks a session/fork request against the agent's
// initialize response before it reaches the wire: the agent must advertise
// capabilities.session.fork, the source session ID must be non-blank, cwd must
// be absolute, and additional directories need the additionalDirectories
// capability and must each be absolute.
//
// SystemPrompt is passed through unchanged. Its capability-gated validation
// (session.systemPrompt and mode.override) belongs here once client-provided
// system prompts gain typed support (#58).
func ValidateForkRequest(initialization schema.InitializeResponse, request schema.ForkSessionRequest) error {
	capabilities := sessionCapabilities(initialization)
	if capabilities == nil || capabilities.Fork == nil {
		return fmt.Errorf("agent did not advertise session/fork support")
	}
	if err := acpvalidate.Identifier("session ID", string(request.SessionID)); err != nil {
		return err
	}
	if err := acpvalidate.AbsolutePath("fork working directory", string(request.Cwd)); err != nil {
		return err
	}
	if len(request.AdditionalDirectories) > 0 && capabilities.AdditionalDirectories == nil {
		return fmt.Errorf("agent did not advertise additionalDirectories support")
	}
	for _, directory := range request.AdditionalDirectories {
		if err := acpvalidate.AbsolutePath("fork additional directory", string(directory)); err != nil {
			return err
		}
	}
	return nil
}

// ValidateForkResponse requires the non-blank identifier of the new session.
func ValidateForkResponse(response schema.ForkSessionResponse) error {
	return acpvalidate.Identifier("forked session ID", string(response.SessionID))
}

func sessionCapabilities(initialization schema.InitializeResponse) *schema.SessionCapabilities {
	if initialization.Capabilities == nil {
		return nil
	}
	return initialization.Capabilities.Session
}
