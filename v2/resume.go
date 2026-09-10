package v2

import (
	"context"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// ResumeSessionFromStart resumes a session and requests replay from the very
// beginning. Update and interactive handlers must already be installed on the
// Connection: replay updates arrive before the resume response, and this
// method returns only after those notifications have been handled in wire
// order.
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
