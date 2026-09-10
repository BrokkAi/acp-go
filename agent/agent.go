// Package agent serves ACP agents over stdio while giving them typed access
// to editor-provided client capabilities.
package agent

import (
	"context"

	"github.com/BrokkAi/acp-go/schema"
)

// SessionUpdater reports progress while a prompt turn runs. The runtime
// supplies the enclosing session ID and emits the session/update notification.
type SessionUpdater interface {
	Update(schema.SessionUpdate) error
}

// UpdateFunc adapts an ordinary function to SessionUpdater.
type UpdateFunc func(schema.SessionUpdate) error

func (f UpdateFunc) Update(update schema.SessionUpdate) error { return f(update) }

// Client is the typed editor-side API available to an agent. The runtime
// rejects host methods that the client did not advertise during initialize.
type Client interface {
	// Call and Notify allow protocol extensions while the typed methods below
	// remain the preferred surface for every registered ACP method.
	Call(ctx context.Context, method string, params, result any) error
	Notify(ctx context.Context, method string, params any) error

	ReadTextFile(context.Context, schema.ReadTextFileRequest) (schema.ReadTextFileResponse, error)
	WriteTextFile(context.Context, schema.WriteTextFileRequest) (schema.WriteTextFileResponse, error)
	RequestPermission(context.Context, schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error)
	CreateElicitation(context.Context, schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error)
	CompleteElicitation(context.Context, schema.ElicitationId) error

	CreateTerminal(context.Context, schema.CreateTerminalRequest) (schema.CreateTerminalResponse, error)
	TerminalOutput(context.Context, schema.TerminalOutputRequest) (schema.TerminalOutputResponse, error)
	WaitForTerminalExit(context.Context, schema.WaitForTerminalExitRequest) (schema.WaitForTerminalExitResponse, error)
	KillTerminal(context.Context, schema.KillTerminalRequest) (schema.KillTerminalResponse, error)
	ReleaseTerminal(context.Context, schema.ReleaseTerminalRequest) (schema.ReleaseTerminalResponse, error)
}

// Agent implements the mandatory ACP v1 agent surface. Optional methods are
// discovered through the small interfaces below; the runtime returns
// method-not-found when an agent does not implement one.
type Agent interface {
	Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error)
	NewSession(context.Context, Client, schema.NewSessionRequest) (schema.NewSessionResponse, error)
	Prompt(context.Context, Client, schema.PromptRequest, SessionUpdater) (schema.PromptResponse, error)
}

type Authenticator interface {
	Authenticate(context.Context, Client, schema.AuthenticateRequest) (schema.AuthenticateResponse, error)
}

type SessionLoader interface {
	LoadSession(context.Context, Client, schema.LoadSessionRequest) (schema.LoadSessionResponse, error)
}

type SessionResumer interface {
	ResumeSession(context.Context, Client, schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error)
}

type SessionCloser interface {
	CloseSession(context.Context, Client, schema.CloseSessionRequest) (schema.CloseSessionResponse, error)
}

type SessionLister interface {
	ListSessions(context.Context, Client, schema.ListSessionsRequest) (schema.ListSessionsResponse, error)
}

type SessionDeleter interface {
	DeleteSession(context.Context, Client, schema.DeleteSessionRequest) (schema.DeleteSessionResponse, error)
}

type SessionCanceler interface {
	CancelSession(context.Context, schema.CancelNotification) error
}

type ModeSetter interface {
	SetMode(context.Context, Client, schema.SetSessionModeRequest) (schema.SetSessionModeResponse, error)
}

type ConfigOptionSetter interface {
	SetConfigOption(context.Context, Client, schema.SetSessionConfigOptionRequest) (schema.SetSessionConfigOptionResponse, error)
}

type LoggerOuter interface {
	Logout(context.Context, Client, schema.LogoutRequest) (schema.LogoutResponse, error)
}
