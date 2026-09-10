package acp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BrokkAi/acp-go/schema"
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
	return schema.CreateElicitationResponse{Accept: &schema.ElicitationAcceptAction{Content: content}}
}

func DeclineElicitation() schema.CreateElicitationResponse {
	return schema.CreateElicitationResponse{Decline: &schema.CreateElicitationResponseDecline{}}
}

func CancelElicitation() schema.CreateElicitationResponse {
	return schema.CreateElicitationResponse{Cancel: &schema.CreateElicitationResponseCancel{}}
}

// ElicitationHandler serves typed elicitation/create requests.
type ElicitationHandler func(context.Context, schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error)

// HandleElicitation composes typed elicitation support with another
// agent-to-client request handler. Pass nil for next when elicitation is the
// only hosted method.
func HandleElicitation(next Handler, elicitation ElicitationHandler) Handler {
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		if method != schema.ElicitationCreateMethodName {
			if next == nil {
				return nil, &RPCError{Code: -32601, Message: "unsupported method: " + method}
			}
			return next(ctx, method, raw)
		}
		if elicitation == nil {
			return nil, &RPCError{Code: -32601, Message: "elicitation support is not configured"}
		}
		var request schema.CreateElicitationRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, &RPCError{Code: -32602, Message: err.Error()}
		}
		return elicitation(ctx, request)
	}
}

// ElicitationComplete adapts the agent's elicitation/complete notification to
// the generated notification type. Other notifications are ignored.
func ElicitationComplete(handler func(schema.CompleteElicitationNotification) error) Notifications {
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
// order. The first error terminates the connection, matching Connection semantics.
func CombineNotifications(callbacks ...Notifications) Notifications {
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
