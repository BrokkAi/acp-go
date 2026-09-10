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
// acknowledges acceptance; agents continue work and later report idle through
// SessionUpdater.
type Agent interface {
	Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error)
	NewSession(context.Context, Client, schema.NewSessionRequest) (schema.NewSessionResponse, error)
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
