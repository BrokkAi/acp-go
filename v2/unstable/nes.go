/*
Package unstable is the draft-v2 counterpart to
github.com/BrokkAi/acp-go/unstable: narrow, opt-in typed facades for ACP's
optional methods, starting with Next Edit Suggestions. Every method stays
behind this import; nothing is reachable from the stable v2 facade.

Capability gating reads the unstable draft-v2 initialize response, which is the
only place the optional capability fields are published:

	var initialization unstable.InitializeResponse
	json.Unmarshal(encodedInitializeResult, &initialization)
	session, err := unstable.StartNes(ctx, connection, initialization, request)
*/
package unstable

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

func requireNes(initialization schema.InitializeResponse) error {
	if initialization.Capabilities == nil || initialization.Capabilities.Nes == nil {
		return fmt.Errorf("agent did not advertise next edit suggestion support")
	}
	return nil
}

// StartNes opens a next-edit-suggestion session and returns its identifier.
func StartNes(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.StartNesRequest) (schema.StartNesResponse, error) {
	var result schema.StartNesResponse
	if err := requireNes(initialization); err != nil {
		return result, err
	}
	if err := connection.Call(ctx, schema.NesStartMethodName, request, &result); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("NES session ID", string(result.SessionID)); err != nil {
		return result, err
	}
	return result, nil
}

// SuggestNes asks for suggestions at one position in a document.
func SuggestNes(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.SuggestNesRequest) (schema.SuggestNesResponse, error) {
	var result schema.SuggestNesResponse
	if err := requireNes(initialization); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("NES session ID", string(request.SessionID)); err != nil {
		return result, err
	}
	if err := acpvalidate.URI("NES document URI", request.URI); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.NesSuggestMethodName, request, &result)
}

// AcceptNes reports that the user accepted one suggestion.
func AcceptNes(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, notification schema.AcceptNesNotification) error {
	if err := requireNes(initialization); err != nil {
		return err
	}
	if err := acpvalidate.Identifier("NES session ID", string(notification.SessionID)); err != nil {
		return err
	}
	if err := acpvalidate.Identifier("NES suggestion ID", string(notification.SuggestionID)); err != nil {
		return err
	}
	return connection.Notify(ctx, schema.NesAcceptMethodName, notification)
}

// RejectNes reports that the user dismissed one suggestion.
func RejectNes(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, notification schema.RejectNesNotification) error {
	if err := requireNes(initialization); err != nil {
		return err
	}
	if err := acpvalidate.Identifier("NES session ID", string(notification.SessionID)); err != nil {
		return err
	}
	if err := acpvalidate.Identifier("NES suggestion ID", string(notification.SuggestionID)); err != nil {
		return err
	}
	return connection.Notify(ctx, schema.NesRejectMethodName, notification)
}

// CloseNes ends a next-edit-suggestion session.
func CloseNes(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.CloseNesRequest) (schema.CloseNesResponse, error) {
	var result schema.CloseNesResponse
	if err := requireNes(initialization); err != nil {
		return result, err
	}
	if err := acpvalidate.Identifier("NES session ID", string(request.SessionID)); err != nil {
		return result, err
	}
	return result, connection.Call(ctx, schema.NesCloseMethodName, request, &result)
}

// NesHandler serves the agent side of the NES methods. Accept and Reject are
// notifications; the rest are requests.
type NesHandler interface {
	StartNes(context.Context, schema.StartNesRequest) (schema.StartNesResponse, error)
	SuggestNes(context.Context, schema.SuggestNesRequest) (schema.SuggestNesResponse, error)
	AcceptNes(context.Context, schema.AcceptNesNotification) error
	RejectNes(context.Context, schema.RejectNesNotification) error
	CloseNes(context.Context, schema.CloseNesRequest) (schema.CloseNesResponse, error)
}

// Handle composes the agent-side NES request dispatch with another handler.
// Pass nil for next when NES is the only hosted request surface.
func Handle(next acp.Handler, handler any) acp.Handler {
	nes, _ := handler.(NesHandler)
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case schema.NesStartMethodName:
			if nes == nil {
				return nil, methodNotFound(method)
			}
			request, err := decode[schema.StartNesRequest](raw)
			if err != nil {
				return nil, err
			}
			return nes.StartNes(ctx, request)
		case schema.NesSuggestMethodName:
			if nes == nil {
				return nil, methodNotFound(method)
			}
			request, err := decode[schema.SuggestNesRequest](raw)
			if err != nil {
				return nil, err
			}
			return nes.SuggestNes(ctx, request)
		case schema.NesCloseMethodName:
			if nes == nil {
				return nil, methodNotFound(method)
			}
			request, err := decode[schema.CloseNesRequest](raw)
			if err != nil {
				return nil, err
			}
			return nes.CloseNes(ctx, request)
		default:
			if next == nil {
				return nil, methodNotFound(method)
			}
			return next(ctx, method, raw)
		}
	}
}

// HandleNesNotifications composes the agent-side NES notification handling with
// another notification handler.
func HandleNesNotifications(next acp.Notifications, handler any) acp.Notifications {
	nes, _ := handler.(NesHandler)
	return func(method string, raw json.RawMessage) error {
		switch method {
		case schema.NesAcceptMethodName:
			if nes == nil {
				return nil
			}
			notification, err := decode[schema.AcceptNesNotification](raw)
			if err != nil {
				return err
			}
			return nes.AcceptNes(context.Background(), notification)
		case schema.NesRejectMethodName:
			if nes == nil {
				return nil
			}
			notification, err := decode[schema.RejectNesNotification](raw)
			if err != nil {
				return err
			}
			return nes.RejectNes(context.Background(), notification)
		default:
			if next == nil {
				return nil
			}
			return next(method, raw)
		}
	}
}

func decode[T any](raw json.RawMessage) (T, error) {
	var request T
	if err := json.Unmarshal(raw, &request); err != nil {
		return request, &acp.RPCError{Code: -32602, Message: err.Error()}
	}
	return request, nil
}

func methodNotFound(method string) error {
	return &acp.RPCError{Code: -32601, Message: "unsupported method: " + method}
}
