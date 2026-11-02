package unstable

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

// sessionCapabilities returns the advertised capabilities.session, or nil.
func sessionCapabilities(initialization schema.InitializeResponse) *schema.SessionCapabilities {
	if initialization.Capabilities == nil {
		return nil
	}
	return initialization.Capabilities.Session
}

func advertisesArchive(initialization schema.InitializeResponse) bool {
	capabilities := sessionCapabilities(initialization)
	return capabilities != nil && capabilities.Archive != nil
}

// requireSessionTarget gates a session-scoped unstable method on its
// capability and requires a non-blank session ID.
func requireSessionTarget(advertised bool, method string, sessionID schema.SessionId) error {
	if !advertised {
		return fmt.Errorf("agent did not advertise %s support", method)
	}
	return acpvalidate.Identifier("session ID", string(sessionID))
}

// ArchiveSession sends session/archive, gated on capabilities.session.archive,
// with a non-blank session ID. Archiving an active session ends it as
// session/close would; archiving an archived session succeeds. An unknown
// session answers -32002 with SessionNotFoundErrorData, which
// acpv2.SessionNotFoundData decodes.
func ArchiveSession(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.ArchiveSessionRequest) (schema.ArchiveSessionResponse, error) {
	var result schema.ArchiveSessionResponse
	if err := requireSessionTarget(advertisesArchive(initialization), schema.SessionArchiveMethodName, request.SessionID); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.SessionArchiveMethodName, request, &result)
}

// UnarchiveSession sends session/unarchive, gated on
// capabilities.session.archive, with a non-blank session ID. It never resumes
// the session; unarchiving a session that is not archived succeeds. An unknown
// session answers -32002 with SessionNotFoundErrorData, which
// acpv2.SessionNotFoundData decodes.
func UnarchiveSession(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.UnarchiveSessionRequest) (schema.UnarchiveSessionResponse, error) {
	var result schema.UnarchiveSessionResponse
	if err := requireSessionTarget(advertisesArchive(initialization), schema.SessionUnarchiveMethodName, request.SessionID); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.SessionUnarchiveMethodName, request, &result)
}

// ListSessions sends session/list with the unstable request, whose archived
// filter the stable v2.Connection.ListSessions cannot carry. It is gated on
// capabilities.session, a cwd filter must be absolute, and a non-nil archived
// filter additionally needs capabilities.session.archive. The filter is an
// open enum: values other than exclude, include, and only are sent unchanged,
// and an agent that does not support one answers -32602. Keep the same cwd and
// archived values when following nextCursor.
func ListSessions(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.ListSessionsRequest) (schema.ListSessionsResponse, error) {
	var result schema.ListSessionsResponse
	capabilities := sessionCapabilities(initialization)
	if capabilities == nil {
		return result, fmt.Errorf("agent did not advertise the ACP v2 session method surface")
	}
	if request.Cwd != nil {
		if err := acpvalidate.AbsolutePath("session/list cwd", string(*request.Cwd)); err != nil {
			return result, err
		}
	}
	if request.Archived != nil && capabilities.Archive == nil {
		return result, fmt.Errorf("agent did not advertise session/archive support for the archived filter")
	}
	return result, connection.Call(ctx, schema.SessionListMethodName, request, &result)
}

// SessionStatus sends session/status, gated on capabilities.session.status,
// with a non-blank session ID. The method has no side effects on the agent.
//
// A status other than open must be treated as saved, so compare against
// schema.SessionStatusOpen rather than schema.SessionStatusSaved; unknown
// values are preserved. State is nil when the agent reports no foreground
// state, including when it sent a malformed one; that must not be read as
// idle, and an idle snapshot is not a new completion. A response without the
// required status (omitted or null) is an error, as in the reference SDK. An
// unknown session answers -32002 with SessionNotFoundErrorData, which
// acpv2.SessionNotFoundData decodes.
func SessionStatus(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.SessionStatusRequest) (schema.SessionStatusResponse, error) {
	var result schema.SessionStatusResponse
	capabilities := sessionCapabilities(initialization)
	if err := requireSessionTarget(capabilities != nil && capabilities.Status != nil, schema.SessionStatusMethodName, request.SessionID); err != nil {
		return result, err
	}
	return result, callRequiring(ctx, connection, schema.SessionStatusMethodName, request, "status", &result)
}

// SetSessionTitle sends session/set_title, gated on
// capabilities.session.title, with a non-blank session ID and title. The
// session need not be active. Display the returned title, which the agent may
// have normalized, rather than the one sent; a response without the required
// title (omitted or null) is an error, as in the reference SDK. An unknown
// session answers -32002 with SessionNotFoundErrorData, which
// acpv2.SessionNotFoundData decodes, and a title the agent rejects answers
// -32602.
func SetSessionTitle(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.SetSessionTitleRequest) (schema.SetSessionTitleResponse, error) {
	var result schema.SetSessionTitleResponse
	capabilities := sessionCapabilities(initialization)
	if err := requireSessionTarget(capabilities != nil && capabilities.Title != nil, schema.SessionSetTitleMethodName, request.SessionID); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("session title", request.Title); err != nil {
		return result, err
	}
	return result, callRequiring(ctx, connection, schema.SessionSetTitleMethodName, request, "title", &result)
}

// callRequiring sends method and decodes its result, failing when the
// response omits member or sends it as null, which the reference SDK cannot
// decode either.
func callRequiring(ctx context.Context, connection *acpv2.Connection, method string, request any, member string, result any) error {
	var raw json.RawMessage
	if err := connection.Call(ctx, method, request, &raw); err != nil {
		return err
	}
	if err := acpvalidate.RequiredMember(method, raw, member); err != nil {
		return err
	}
	return json.Unmarshal(raw, result)
}
