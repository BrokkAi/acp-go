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
	"github.com/BrokkAi/acp-go/internal/systemprompt"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
)

// ValidateNewSessionRequest checks a session/new request against the agent's
// initialize response before it reaches the wire: the agent must advertise
// the baseline capabilities.session surface, cwd must be absolute, additional
// directories need the additionalDirectories capability and must each be
// absolute, and a systemPrompt must pass ValidateSystemPrompt.
func ValidateNewSessionRequest(initialization schema.InitializeResponse, request schema.NewSessionRequest) error {
	capabilities := sessionCapabilities(initialization)
	if capabilities == nil {
		return fmt.Errorf("agent did not advertise the ACP v2 session method surface")
	}
	if err := validatePaths(capabilities, "session working directory", request.Cwd, "session additional directory", request.AdditionalDirectories); err != nil {
		return err
	}
	return ValidateSystemPrompt(initialization, request.SystemPrompt)
}

// ValidateResumeSessionRequest is ValidateNewSessionRequest for
// session/resume, which also requires a non-blank session ID.
func ValidateResumeSessionRequest(initialization schema.InitializeResponse, request schema.ResumeSessionRequest) error {
	capabilities := sessionCapabilities(initialization)
	if capabilities == nil {
		return fmt.Errorf("agent did not advertise the ACP v2 session method surface")
	}
	if err := acpvalidate.Identifier("session ID", string(request.SessionID)); err != nil {
		return err
	}
	if err := validatePaths(capabilities, "session working directory", request.Cwd, "session additional directory", request.AdditionalDirectories); err != nil {
		return err
	}
	return ValidateSystemPrompt(initialization, request.SystemPrompt)
}

// ValidateForkRequest checks a session/fork request against the agent's
// initialize response before it reaches the wire: the agent must advertise
// capabilities.session.fork, the source session ID must be non-blank, cwd must
// be absolute, additional directories need the additionalDirectories
// capability and must each be absolute, and a systemPrompt must pass
// ValidateSystemPrompt.
func ValidateForkRequest(initialization schema.InitializeResponse, request schema.ForkSessionRequest) error {
	capabilities := sessionCapabilities(initialization)
	if capabilities == nil || capabilities.Fork == nil {
		return fmt.Errorf("agent did not advertise session/fork support")
	}
	if err := acpvalidate.Identifier("session ID", string(request.SessionID)); err != nil {
		return err
	}
	if err := validatePaths(capabilities, "fork working directory", request.Cwd, "fork additional directory", request.AdditionalDirectories); err != nil {
		return err
	}
	return ValidateSystemPrompt(initialization, request.SystemPrompt)
}

// ValidateSystemPrompt checks a client-provided system prompt: the agent must
// advertise capabilities.session.systemPrompt, the mode must be present and
// either append or override, override needs the nested mode.override
// capability, and the content must be a non-empty list of text blocks. A nil
// prompt is valid.
func ValidateSystemPrompt(initialization schema.InitializeResponse, prompt *schema.SystemPrompt) error {
	return systemprompt.V2Client(initialization, prompt)
}

// ValidateForkResponse requires the non-blank identifier of the new session.
func ValidateForkResponse(response schema.ForkSessionResponse) error {
	return acpvalidate.Identifier("forked session ID", string(response.SessionID))
}

func validatePaths(capabilities *schema.SessionCapabilities, cwdLabel string, cwd schema.AbsolutePath, additionalLabel string, additional []schema.AbsolutePath) error {
	if err := acpvalidate.AbsolutePath(cwdLabel, string(cwd)); err != nil {
		return err
	}
	if len(additional) > 0 && capabilities.AdditionalDirectories == nil {
		return fmt.Errorf("agent did not advertise additionalDirectories support")
	}
	for _, directory := range additional {
		if err := acpvalidate.AbsolutePath(additionalLabel, string(directory)); err != nil {
			return err
		}
	}
	return nil
}

func sessionCapabilities(initialization schema.InitializeResponse) *schema.SessionCapabilities {
	if initialization.Capabilities == nil {
		return nil
	}
	return initialization.Capabilities.Session
}
