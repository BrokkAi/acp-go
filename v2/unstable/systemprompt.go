package unstable

import (
	"context"
	"errors"

	"github.com/BrokkAi/acp-go/internal/systemprompt"
	"github.com/BrokkAi/acp-go/internal/unstabledispatch"
	"github.com/BrokkAi/acp-go/internal/v2session"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
)

// SystemPrompt is the client-provided system prompt a client may send on
// session/new, session/resume, and session/fork once the agent advertises
// capabilities.session.systemPrompt.
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
	// the agent's capabilities.session.systemPrompt.mode.override.
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

// ValidateSessionSystemPrompt is the client-side check NewSession,
// ResumeSession, ForkSession, and the github.com/BrokkAi/acp-go/v2/mcp session
// helpers run on a systemPrompt before it reaches the wire: the agent must
// advertise capabilities.session.systemPrompt, the mode must be present and
// either Append or Override, Override needs
// capabilities.session.systemPrompt.mode.override, and the content must be a
// non-empty list of text blocks. A nil prompt is valid.
func ValidateSessionSystemPrompt(initialization schema.InitializeResponse, prompt *SystemPrompt) error {
	return v2session.ValidateSystemPrompt(initialization, prompt)
}

// SystemPromptHandler serves session/new and session/resume with the
// client-provided system prompt, which the stable draft-v2 requests the
// v2/agent runtime decodes do not carry. Install serves both from these
// methods, in place of the Agent's NewSession and ResumeSession, through
// runtime overrides, and announces capabilities.session.systemPrompt ({},
// append only) when session/fork, if advertised, also receives the prompt
// through ForkHandler. A customize function adds mode.override for an agent
// that supports override; Advertise alone never announces the capability.
//
// A method receives only a prompt the agent can apply; a nil prompt means the
// client supplied none. Before calling it, Install's route (and Handle's, for
// ForkSession) checks the systemPrompt the same way the agent must: a value
// the reference SDK does not deserialize (a missing or null mode or content,
// a text block without text) answers -32602 in the deserialization shape of
// other malformed params, and a prompt this connection's initialize response
// did not advertise support for, an unknown mode, override without
// mode.override, empty content, or a non-text content block (including an
// unknown content type preserved as Other) answers the reference SDK's -32602
// with the reason as string data, without reaching the handler. The
// capability checked is the one the runtime sent on the connection, so a
// handler cannot accept a prompt that was never advertised. The
// connection's client is agent.ClientFromContext(ctx), the client the stable
// dispatch passes to NewSession and ResumeSession.
type SystemPromptHandler interface {
	NewSessionWithSystemPrompt(context.Context, schema.NewSessionRequest) (schema.NewSessionResponse, error)
	ResumeSessionWithSystemPrompt(context.Context, schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error)
}

// servesSystemPrompt reports whether handler receives the systemPrompt on
// every session lifecycle request response advertises.
func servesSystemPrompt(handler any, response schema.InitializeResponse) bool {
	if _, ok := handler.(SystemPromptHandler); !ok {
		return false
	}
	if session := sessionCapabilities(response); session != nil && session.Fork != nil {
		if _, ok := handler.(ForkHandler); !ok {
			return false
		}
	}
	return true
}

// advertiseSystemPrompt announces capabilities.session.systemPrompt for
// Install when handler serves it on every advertised lifecycle request. Like
// the other session entries, it is added only alongside the baseline session
// capabilities.
func advertiseSystemPrompt(handler any, response *schema.InitializeResponse) {
	session := sessionCapabilities(*response)
	if session == nil || session.SystemPrompt != nil || !servesSystemPrompt(handler, *response) {
		return
	}
	session.SystemPrompt = &schema.SessionSystemPromptCapabilities{}
}

// checkSystemPromptServed refuses, after the customize functions ran, a
// systemPrompt capability that some advertised lifecycle request would
// silently drop.
func checkSystemPromptServed(handler any, response schema.InitializeResponse) error {
	session := sessionCapabilities(response)
	if session == nil || session.SystemPrompt == nil || servesSystemPrompt(handler, response) {
		return nil
	}
	return errors.New("unstable.Install: capabilities.session.systemPrompt is advertised, but the handler does not serve it on every advertised session lifecycle request (SystemPromptHandler, plus ForkHandler for fork)")
}

// systemPromptRoute is the binding for a lifecycle method whose request
// carries a systemPrompt, given as prompt: it refuses a systemPrompt the
// reference SDK does not deserialize and one the connection's announced
// capability does not cover before the handler runs (see
// SystemPromptHandler).
func systemPromptRoute[H, Request, Response any](method func(H, context.Context, Request) (Response, error), prompt func(Request) *SystemPrompt, check func(Response) error) unstabledispatch.Binding {
	return unstabledispatch.RouteChecked(method, systemprompt.StrictV2, func(ctx context.Context, request Request) error {
		return systemprompt.AgentV2(ctx, prompt(request))
	}, check)
}
