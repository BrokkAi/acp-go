package v2

import (
	"encoding/json"
	"fmt"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// ElicitationClientCapabilities advertises support for form and URL modes.
func ElicitationClientCapabilities(form, url bool) *schema.ElicitationCapabilities {
	capabilities := schema.ElicitationCapabilities{}
	if form {
		capabilities.Form = &schema.ElicitationFormCapabilities{}
	}
	if url {
		capabilities.URL = &schema.ElicitationUrlCapabilities{}
	}
	return &capabilities
}

func AcceptElicitation(content map[string]schema.ElicitationContentValue) schema.CreateElicitationResponse {
	return schema.CreateElicitationResponse{Accept: &schema.ElicitationAcceptAction{
		Content: schema.Nullable[map[string]schema.ElicitationContentValue]{Set: true, Value: content},
	}}
}

func DeclineElicitation() schema.CreateElicitationResponse {
	return schema.CreateElicitationResponse{Decline: &schema.CreateElicitationResponseDecline{}}
}

func CancelElicitation() schema.CreateElicitationResponse {
	return schema.CreateElicitationResponse{Cancel: &schema.CreateElicitationResponseCancel{}}
}

// ElicitationComplete adapts the agent's elicitation/complete notification to
// the generated notification type. Other notifications are ignored.
func ElicitationComplete(handler func(schema.CompleteElicitationNotification) error) acp.Notifications {
	return func(method string, raw json.RawMessage) error {
		if method != schema.ElicitationCompleteMethodName {
			return nil
		}
		var notification schema.CompleteElicitationNotification
		if err := json.Unmarshal(raw, &notification); err != nil {
			return err
		}
		if notification.ElicitationID == "" {
			return fmt.Errorf("elicitation ID is required")
		}
		return handler(notification)
	}
}

// CombineNotifications fans an incoming notification out to typed adapters in
// wire order. The first error terminates the connection.
func CombineNotifications(callbacks ...acp.Notifications) acp.Notifications {
	return func(method string, raw json.RawMessage) error {
		for _, callback := range callbacks {
			if callback == nil {
				continue
			}
			if err := callback(method, raw); err != nil {
				return err
			}
		}
		return nil
	}
}
