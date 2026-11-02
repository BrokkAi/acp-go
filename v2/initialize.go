package v2

import (
	"context"
	"encoding/json"
	"fmt"
)

// InitializeExtended is InitializeWithInfo that also decodes the agent's
// initialize result into extended, a pointer to a type that extends the v2
// initialize response. Pass a *schema/v2/unstable.InitializeResponse to read
// the unstable capabilities the stable Initialization drops, which the
// github.com/BrokkAi/acp-go/v2/unstable and v2/mcp facades gate on. The
// result is validated and the connection state recorded exactly as by
// InitializeWithInfo. When it returns an error, extended may hold a partially
// decoded or unvalidated response and must not be used.
func (c *Connection) InitializeExtended(ctx context.Context, capabilities Capabilities, info ClientInfo, extended any) (Initialization, error) {
	if extended == nil {
		return Initialization{}, fmt.Errorf("extended initialize response target is required")
	}
	return c.initialize(ctx, capabilities, info, extended)
}

// decodeInitialize decodes one initialize result into the stable response
// and, when extended is non-nil, into extended as well.
func decodeInitialize(encoded json.RawMessage, result *Initialization, extended any) error {
	if err := json.Unmarshal(encoded, result); err != nil {
		return err
	}
	if extended != nil {
		if err := json.Unmarshal(encoded, extended); err != nil {
			return fmt.Errorf("decode extended initialize response: %w", err)
		}
	}
	return nil
}
