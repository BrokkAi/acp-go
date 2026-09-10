package agent

import (
	"context"
	"encoding/json"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
)

type Filesystem interface {
	ReadTextFile(context.Context, schema.ReadTextFileRequest) (schema.ReadTextFileResponse, error)
	WriteTextFile(context.Context, schema.WriteTextFileRequest) (schema.WriteTextFileResponse, error)
}

type Terminal interface {
	CreateTerminal(context.Context, schema.CreateTerminalRequest) (schema.CreateTerminalResponse, error)
	TerminalOutput(context.Context, schema.TerminalOutputRequest) (schema.TerminalOutputResponse, error)
	WaitForTerminalExit(context.Context, schema.WaitForTerminalExitRequest) (schema.WaitForTerminalExitResponse, error)
	KillTerminal(context.Context, schema.KillTerminalRequest) (schema.KillTerminalResponse, error)
	ReleaseTerminal(context.Context, schema.ReleaseTerminalRequest) (schema.ReleaseTerminalResponse, error)
}

type Permissions interface {
	RequestPermission(context.Context, schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error)
}

type Elicitation interface {
	CreateElicitation(context.Context, schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error)
}

// HandleFilesystem adds typed filesystem method dispatch to next.
func HandleFilesystem(next acp.Handler, host Filesystem) acp.Handler {
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if host == nil {
			return passToNext(ctx, next, method, raw)
		}
		switch method {
		case schema.FsReadTextFileMethodName:
			request, err := decodeRequest[schema.ReadTextFileRequest](raw)
			if err != nil {
				return nil, err
			}
			return host.ReadTextFile(ctx, request)
		case schema.FsWriteTextFileMethodName:
			request, err := decodeRequest[schema.WriteTextFileRequest](raw)
			if err != nil {
				return nil, err
			}
			return host.WriteTextFile(ctx, request)
		default:
			return passToNext(ctx, next, method, raw)
		}
	}
}

// HandleTerminal adds typed terminal method dispatch to next.
func HandleTerminal(next acp.Handler, host Terminal) acp.Handler {
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if host == nil {
			return passToNext(ctx, next, method, raw)
		}
		switch method {
		case schema.TerminalCreateMethodName:
			request, err := decodeRequest[schema.CreateTerminalRequest](raw)
			if err != nil {
				return nil, err
			}
			return host.CreateTerminal(ctx, request)
		case schema.TerminalOutputMethodName:
			request, err := decodeRequest[schema.TerminalOutputRequest](raw)
			if err != nil {
				return nil, err
			}
			return host.TerminalOutput(ctx, request)
		case schema.TerminalWaitForExitMethodName:
			request, err := decodeRequest[schema.WaitForTerminalExitRequest](raw)
			if err != nil {
				return nil, err
			}
			return host.WaitForTerminalExit(ctx, request)
		case schema.TerminalKillMethodName:
			request, err := decodeRequest[schema.KillTerminalRequest](raw)
			if err != nil {
				return nil, err
			}
			return host.KillTerminal(ctx, request)
		case schema.TerminalReleaseMethodName:
			request, err := decodeRequest[schema.ReleaseTerminalRequest](raw)
			if err != nil {
				return nil, err
			}
			return host.ReleaseTerminal(ctx, request)
		default:
			return passToNext(ctx, next, method, raw)
		}
	}
}

// HandlePermissions adds typed session/request_permission dispatch to next.
func HandlePermissions(next acp.Handler, host Permissions) acp.Handler {
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if host == nil || method != schema.SessionRequestPermissionMethodName {
			return passToNext(ctx, next, method, raw)
		}
		request, err := decodeRequest[schema.RequestPermissionRequest](raw)
		if err != nil {
			return nil, err
		}
		return host.RequestPermission(ctx, request)
	}
}

// HandleElicitation adds typed elicitation/create dispatch to next.
func HandleElicitation(next acp.Handler, host Elicitation) acp.Handler {
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if host == nil || method != schema.ElicitationCreateMethodName {
			return passToNext(ctx, next, method, raw)
		}
		request, err := decodeRequest[schema.CreateElicitationRequest](raw)
		if err != nil {
			return nil, err
		}
		return host.CreateElicitation(ctx, request)
	}
}

// HandleClientHost composes the typed client-capability handlers. Nil
// components are ignored.
func HandleClientHost(filesystem Filesystem, terminal Terminal, permissions Permissions, elicitation Elicitation) acp.Handler {
	handler := HandleFilesystem(nil, filesystem)
	handler = HandleTerminal(handler, terminal)
	handler = HandlePermissions(handler, permissions)
	return HandleElicitation(handler, elicitation)
}

func passToNext(ctx context.Context, next acp.Handler, method string, raw json.RawMessage) (any, error) {
	if next == nil {
		return nil, methodNotFound(method)
	}
	return next(ctx, method, raw)
}
