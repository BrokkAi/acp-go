package v2

import (
	"encoding/json"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// SessionUpdates adapts v2 session/update notifications to the generated
// discriminated-union shape.
func SessionUpdates(handle func(Update) error) acp.Notifications {
	return func(method string, raw json.RawMessage) error {
		if method != schema.SessionUpdateMethodName {
			return nil
		}
		var update Update
		if err := json.Unmarshal(raw, &update); err != nil {
			return err
		}
		return handle(update)
	}
}
