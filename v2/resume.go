package v2

import (
	"context"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// ResumeSessionFromStart resumes a session and requests replay from the very
// beginning. Update and interactive handlers must already be installed on the
// Connection: replay updates arrive before the resume response, and this
// method returns only after those notifications have been handled in wire
// order. Since schema crate 1.18 the agent ends the replayed history with a
// replay_complete update, also when the history is empty, so an update
// handler can tell replayed history from live updates; a resume without a
// replay gets none.
func (c *Connection) ResumeSessionFromStart(ctx context.Context, initialization Initialization, sessionID SessionID, directory string, additionalDirectories []string) (schema.ResumeSessionResponse, error) {
	return c.ResumeSession(ctx, initialization, schema.ResumeSessionRequest{
		SessionID:             sessionID,
		Cwd:                   schema.AbsolutePath(directory),
		AdditionalDirectories: absolutePaths(additionalDirectories),
		ReplayFrom: &schema.ReplayFrom{
			Start: &schema.ReplayFromStart{},
		},
	})
}
