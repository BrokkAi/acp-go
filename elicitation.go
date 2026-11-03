package acp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BrokkAi/acp-go/internal/castparams"
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
				return nil, methodNotFound(method)
			}
			return next(ctx, method, raw)
		}
		if elicitation == nil {
			return nil, methodNotFound(method)
		}
		var request schema.CreateElicitationRequest
		if err := castparams.Unmarshal(raw, &request); err != nil {
			return nil, invalidParams(err, raw)
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
		if err := castparams.Unmarshal(raw, &notification); err != nil {
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

// invalidParams is the reference SDK's json_cast_params -32602 for params
// that do not deserialize: the error internal/rpcparams builds for the
// packages that import this one.
func invalidParams(err error, raw json.RawMessage) *RPCError {
	return &RPCError{Code: castparams.Code, Message: castparams.Message, Data: castparams.Data(err, raw)}
}

// methodNotFound is the reference SDK's answer to a request no handler
// serves: the error internal/rpcparams builds for the packages that import
// this one.
func methodNotFound(method string) *RPCError {
	data, _ := json.Marshal(method)
	return &RPCError{Code: -32601, Message: "Method not found", Data: data}
}
