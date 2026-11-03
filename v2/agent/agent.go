// Package agent serves draft ACP v2 agents over stdio and gives them typed
// access to editor-provided client methods.
package agent

import (
	"context"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// SessionUpdater reports a v2 session/update notification. The runtime adds
// the enclosing session ID. Updates may continue after session/prompt returns
// because v2 prompt responses only acknowledge acceptance.
type SessionUpdater interface {
	Update(schema.SessionUpdate) error
}

// UpdateFunc adapts an ordinary function to SessionUpdater.
type UpdateFunc func(schema.SessionUpdate) error

func (f UpdateFunc) Update(update schema.SessionUpdate) error { return f(update) }

// Client is the typed editor-side API available to a v2 agent. The runtime
// rejects host methods that the client did not advertise during initialize.
type Client interface {
	Call(ctx context.Context, method string, params, result any) error
	Notify(ctx context.Context, method string, params any) error

	RequestPermission(context.Context, schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error)
	CreateElicitation(context.Context, schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error)
	CompleteElicitation(context.Context, schema.ElicitationId) error
}

// Agent implements the baseline draft-v2 surface. session/prompt only
// acknowledges that the user message was inserted into the conversation;
// agents continue work and later report idle through SessionUpdater.
//
// Prompt must return a PromptResponse whose MessageID identifies the inserted
// user message. The field is required and non-empty on the wire, and the
// matching user_message session update must carry the same identifier. That
// update may be emitted before or after Prompt returns. The runtime refuses to
// answer session/prompt when MessageID is empty rather than sending an invalid
// response.
type Agent interface {
	Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error)
	NewSession(context.Context, Client, schema.NewSessionRequest) (schema.NewSessionResponse, error)
	// Prompt must return a non-empty PromptResponse.MessageID.
	Prompt(context.Context, Client, schema.PromptRequest, SessionUpdater) (schema.PromptResponse, error)
	ListSessions(context.Context, Client, schema.ListSessionsRequest) (schema.ListSessionsResponse, error)
	ResumeSession(context.Context, Client, schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error)
	CloseSession(context.Context, Client, schema.CloseSessionRequest) (schema.CloseSessionResponse, error)
	CancelSession(schema.CancelSessionNotification) error
}

type AuthLoginer interface {
	AuthLogin(context.Context, Client, schema.LoginAuthRequest) (schema.LoginAuthResponse, error)
}

type AuthLogouter interface {
	AuthLogout(context.Context, Client, schema.LogoutAuthRequest) (schema.LogoutAuthResponse, error)
}

type SessionDeleter interface {
	DeleteSession(context.Context, Client, schema.DeleteSessionRequest) (schema.DeleteSessionResponse, error)
}

type ConfigOptionSetter interface {
	SetConfigOption(context.Context, Client, schema.SetSessionConfigOptionRequest) (schema.SetSessionConfigOptionResponse, error)
}

// ToolCallCanceller serves tool_call/cancel, which stops one tool call that
// advertises capabilities.cancel, in the foreground or in the background. The
// method has no initialize-time capability. An Agent without it answers every
// tool_call/cancel with -32602 (Invalid params), as the reference SDK's v2
// agent does, since it has no tool call it can cancel (see
// ToolCallNotCancellableError).
//
// Answer with the empty response once the cancellation is accepted, not once
// the work has stopped; confirm it with a tool_call_update to a final status,
// normally cancelled (completed or failed if the work finished first).
// Answer ToolCallNotCancellableError for a tool call that is unknown,
// already final, or not cancellable. Cancelling a tool call does not change
// the foreground state, and the Agent should stop advertising cancel once
// the tool call is final.
type ToolCallCanceller interface {
	CancelToolCall(context.Context, Client, schema.CancelToolCallRequest) (schema.CancelToolCallResponse, error)
}

// TerminalCanceller serves terminal/cancel, which stops the process of one
// terminal that advertises capabilities.cancel. An Agent without it answers
// every terminal/cancel with -32602 (see TerminalNotCancellableError).
//
// Answer with the empty response once the cancellation is accepted, and
// confirm it with a terminal_update that sets exitStatus. Answer
// TerminalNotCancellableError for a terminal that is unknown, has already
// exited, or is not cancellable. Cancelling does not release the terminal,
// whose output stays available, and changes neither the foreground state nor
// the status of a tool call that references the terminal.
type TerminalCanceller interface {
	CancelTerminal(context.Context, Client, schema.CancelTerminalRequest) (schema.CancelTerminalResponse, error)
}
