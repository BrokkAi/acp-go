package agent

import (
	"context"
	"encoding/json"
	"fmt"

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
