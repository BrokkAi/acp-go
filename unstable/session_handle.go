package unstable

import (
	"context"

	"github.com/BrokkAi/acp-go/internal/unstabledispatch"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

// ArchiveHandler serves the agent side of session/archive and
// session/unarchive, and session/list with the archived filter, which the
// archive capability covers too. Serve it with Install, which announces
// sessionCapabilities.archive for it when the agent also advertises
// sessionCapabilities.list, as the protocol requires, and serves session/list
// from ListSessionsWithArchive; Advertise alone does not announce archive.
//
// ArchiveSession must end an active session as session/close would before
// archiving it, and both methods succeed when the session is already in the
// requested state. A session the agent does not know answers -32002; return
// acp.SessionNotFoundError.
//
// ListSessionsWithArchive serves session/list in place of the stable
// SessionLister, through the runtime override Install sets. It must honor
// exclude (also the meaning of an absent filter), include, and only, and
// answer -32602 for any other value it does not support rather than fall back
// to exclude. Every listed session must report archived; the override answers
// -32603 instead of sending a response that omits it.
type ArchiveHandler interface {
	ArchiveSession(context.Context, schema.ArchiveSessionRequest) (schema.ArchiveSessionResponse, error)
	UnarchiveSession(context.Context, schema.UnarchiveSessionRequest) (schema.UnarchiveSessionResponse, error)
	ListSessionsWithArchive(context.Context, schema.ListSessionsRequest) (schema.ListSessionsResponse, error)
}

// StatusHandler serves the agent side of session/status. Advertise announces
// sessionCapabilities.status for it. SessionStatus must not have side effects:
// it does not load, resume, create, close, or change the session. A session
// the agent does not know answers -32002 with the session ID as data; return
// acp.SessionNotFoundError. The response must carry a status; Handle sends
// it as returned, as the reference SDK does.
type StatusHandler interface {
	SessionStatus(context.Context, schema.SessionStatusRequest) (schema.SessionStatusResponse, error)
}

// TitleHandler serves the agent side of session/set_title. Advertise
// announces sessionCapabilities.title for it. The session need not be active.
// SetSessionTitle may normalize the title, answers -32602 for a title it
// rejects (including one empty after normalization), returns the stored
// title, and should not replace it with a generated title afterwards. A
// session the agent does not know answers -32002; return
// acp.SessionNotFoundError. Handle sends the response as returned, as the
// reference SDK does.
type TitleHandler interface {
	SetSessionTitle(context.Context, schema.SetSessionTitleRequest) (schema.SetSessionTitleResponse, error)
}

// advertiseSessionHandlers adds the status, title, and archive session
// capabilities for the handlers Advertise finds, keeping any value already
// set. archive is added only for Install, and needs the agent's own
// sessionCapabilities.list.
func advertiseSessionHandlers(handler any, archive bool, response *schema.InitializeResponse) {
	_, status := handler.(StatusHandler)
	_, title := handler.(TitleHandler)
	if _, ok := handler.(ArchiveHandler); !ok {
		archive = false
	}
	if (status || title) && response.AgentCapabilities == nil {
		response.AgentCapabilities = &schema.AgentCapabilities{}
	}
	if (status || title) && response.AgentCapabilities.SessionCapabilities == nil {
		response.AgentCapabilities.SessionCapabilities = &schema.SessionCapabilities{}
	}
	session := sessionCapabilities(*response)
	if status && session.Status == nil {
		session.Status = &schema.SessionStatusCapabilities{}
	}
	if title && session.Title == nil {
		session.Title = &schema.SessionTitleCapabilities{}
	}
	if archive && session != nil && session.List != nil && session.Archive == nil {
		session.Archive = &schema.SessionArchiveCapabilities{}
	}
}

func checkArchiveListResponse(response schema.ListSessionsResponse) error {
	for _, session := range response.Sessions {
		if session.Archived == nil {
			return unstabledispatch.InternalError("agent returned a session/list entry without its archive state")
		}
	}
	return nil
}
