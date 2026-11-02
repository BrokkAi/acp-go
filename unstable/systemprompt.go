package unstable

import (
	"context"
	"errors"

	"github.com/BrokkAi/acp-go/internal/systemprompt"
	"github.com/BrokkAi/acp-go/internal/unstabledispatch"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

// SystemPrompt is the client-provided system prompt a client may send on
// session/new, session/load, session/resume, and session/fork once the agent
// advertises agentCapabilities.sessionCapabilities.systemPrompt.
type SystemPrompt = schema.SystemPrompt

// SystemPromptMode says how the agent combines a SystemPrompt with its own
// instructions. Unknown modes decode and re-encode unchanged, but the facades
// here send and accept only Append and Override.
type SystemPromptMode = schema.SystemPromptMode

const (
	// Append adds the content to the agent's own instructions. Every agent
	// that advertises the systemPrompt capability supports it.
	Append = schema.SystemPromptModeAppend
	// Override replaces the agent's built-in default instructions. It needs
	// the agent's sessionCapabilities.systemPrompt.mode.override.
	Override = schema.SystemPromptModeOverride
)

// NewSystemPrompt returns an Append system prompt with content. Set Mode to
// Override to replace the agent's default instructions instead.
func NewSystemPrompt(content ...schema.ContentBlock) SystemPrompt {
	return SystemPrompt{Mode: Append, Content: content}
}

// NewTextSystemPrompt returns an Append system prompt with one text block,
// the content type every agent advertising the capability must accept.
func NewTextSystemPrompt(text string) SystemPrompt {
	return NewSystemPrompt(schema.ContentBlock{Kind: schema.ContentBlockKindText, Text: &schema.TextContent{Text: text}})
}

// SystemPromptHandler serves session/new with the client-provided system
// prompt, which the stable request the agent runtime decodes does not carry.
// Install serves session/new from NewSessionWithSystemPrompt, in place of the
// Agent's NewSession, through a runtime override, and announces
// sessionCapabilities.systemPrompt ({}, append only) when every session
// lifecycle method the agent advertises also receives the prompt:
// session/load needs SystemPromptLoader, session/resume needs
// SystemPromptResumer, and session/fork needs ForkHandler. A customize
// function adds mode.override for an agent that supports override; Advertise
// alone never announces the capability.
//
// A method receives only a prompt the agent can apply; a nil prompt means the
// client supplied none. Before calling it, Install's route (and Handle's, for
// ForkSession) checks the systemPrompt the same way the agent must: a value
// the reference SDK does not deserialize (a missing or null mode or content,
// a text block without text) answers -32602 in the deserialization shape of
// other malformed params, and a prompt this connection's initialize response
// did not advertise support for, an unknown mode, override without
// mode.override, empty content, or a non-text content block answers the
// reference SDK's -32602 with the reason as string data, without reaching the
// handler. The capability checked is the one the runtime sent on the
// connection, so a handler cannot accept a prompt that was never advertised.
// The connection's client is agent.ClientFromContext(ctx), the client the
// stable dispatch passes to NewSession and LoadSession, so
// LoadSessionWithSystemPrompt can replay the conversation with session/update
// before it answers.
type SystemPromptHandler interface {
	NewSessionWithSystemPrompt(context.Context, schema.NewSessionRequest) (schema.NewSessionResponse, error)
}

// SystemPromptLoader serves session/load with the client-provided system
// prompt, in place of the Agent's LoadSession, once Install is used. See
// SystemPromptHandler.
type SystemPromptLoader interface {
	LoadSessionWithSystemPrompt(context.Context, schema.LoadSessionRequest) (schema.LoadSessionResponse, error)
}

// SystemPromptResumer serves session/resume with the client-provided system
// prompt, in place of the Agent's ResumeSession, once Install is used. See
// SystemPromptHandler.
type SystemPromptResumer interface {
	ResumeSessionWithSystemPrompt(context.Context, schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error)
}

// servesSystemPrompt reports whether handler receives the systemPrompt on
// every session lifecycle request response advertises.
func servesSystemPrompt(handler any, response schema.InitializeResponse) bool {
	if _, ok := handler.(SystemPromptHandler); !ok {
		return false
	}
	capabilities := response.AgentCapabilities
	if capabilities == nil {
		return true
	}
	if capabilities.LoadSession != nil && *capabilities.LoadSession {
		if _, ok := handler.(SystemPromptLoader); !ok {
			return false
		}
	}
	if session := capabilities.SessionCapabilities; session != nil {
		if _, ok := handler.(SystemPromptResumer); session.Resume != nil && !ok {
			return false
		}
		if _, ok := handler.(ForkHandler); session.Fork != nil && !ok {
			return false
		}
	}
	return true
}

// advertiseSystemPrompt announces sessionCapabilities.systemPrompt for
// Install when handler serves it on every advertised lifecycle request.
func advertiseSystemPrompt(handler any, response *schema.InitializeResponse) {
	if !servesSystemPrompt(handler, *response) {
		return
	}
	if response.AgentCapabilities == nil {
		response.AgentCapabilities = &schema.AgentCapabilities{}
	}
	if response.AgentCapabilities.SessionCapabilities == nil {
		response.AgentCapabilities.SessionCapabilities = &schema.SessionCapabilities{}
	}
	if response.AgentCapabilities.SessionCapabilities.SystemPrompt == nil {
		response.AgentCapabilities.SessionCapabilities.SystemPrompt = &schema.SessionSystemPromptCapabilities{}
	}
}

// checkSystemPromptServed refuses, after the customize functions ran, a
// systemPrompt capability that some advertised lifecycle request would
// silently drop.
func checkSystemPromptServed(handler any, response schema.InitializeResponse) error {
	session := sessionCapabilities(response)
	if session == nil || session.SystemPrompt == nil || servesSystemPrompt(handler, response) {
		return nil
	}
	return errors.New("unstable.Install: sessionCapabilities.systemPrompt is advertised, but the handler does not serve it on every advertised session lifecycle request (SystemPromptHandler, plus SystemPromptLoader for loadSession, SystemPromptResumer for resume, and ForkHandler for fork)")
}

// systemPromptRoute is the binding for a lifecycle method whose request
// carries a systemPrompt, given as prompt: it refuses a systemPrompt the
// reference SDK does not deserialize and one the connection's announced
// capability does not cover before the handler runs (see
// SystemPromptHandler).
func systemPromptRoute[H, Request, Response any](method func(H, context.Context, Request) (Response, error), prompt func(Request) *SystemPrompt, check func(Response) error) unstabledispatch.Binding {
	return unstabledispatch.RouteChecked(method, systemprompt.StrictV1, func(ctx context.Context, request Request) error {
		return systemprompt.AgentV1(ctx, prompt(request))
	}, check)
}
