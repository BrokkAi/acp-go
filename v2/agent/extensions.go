package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// serveExtension answers a request the runtime does not own. It runs behind
// the initialization gate, like every non-initialize method.
func (s *server) serveExtension(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	if s.extensions == nil {
		return nil, methodNotFound(method)
	}
	return s.extensions(ctx, method, raw)
}

// extendInitialize applies Runtime.InitializeResult. It returns the value to
// send and the stable view of that exact value, which the runtime validates
// and records instead of the agent's original response.
func (s *server) extendInitialize(response schema.InitializeResponse) (any, schema.InitializeResponse, error) {
	if s.initializeResult == nil {
		return response, response, nil
	}
	extended, err := s.initializeResult(response)
	if err != nil {
		return nil, response, err
	}
	encoded, err := json.Marshal(extended)
	if err != nil {
		return nil, response, fmt.Errorf("encode extended initialize response: %w", err)
	}
	var view schema.InitializeResponse
	if err := json.Unmarshal(encoded, &view); err != nil {
		return nil, response, fmt.Errorf("extended initialize response is not a v2 initialize response: %w", err)
	}
	return json.RawMessage(encoded), view, nil
}

// checkOverrides validates Runtime.Overrides and copies it, so that later
// changes to the caller's map do not race with the connection.
func checkOverrides(overrides map[string]acp.Handler) (map[string]acp.Handler, error) {
	if len(overrides) == 0 {
		return nil, nil
	}
	copied := make(map[string]acp.Handler, len(overrides))
	for method, handler := range overrides {
		switch method {
		case schema.InitializeMethodName, schema.SessionPromptMethodName:
			return nil, fmt.Errorf("runtime override for %s is not allowed: the runtime serves it itself", method)
		}
		if schema.Methods[method].Notification {
			// Overrides answer requests; a notification never reaches them.
			return nil, fmt.Errorf("runtime override for %s is not allowed: it is a notification, not a request", method)
		}
		if handler == nil {
			return nil, fmt.Errorf("runtime override for %s is nil", method)
		}
		copied[method] = handler
	}
	return copied, nil
}
