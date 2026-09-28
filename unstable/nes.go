package unstable

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	schema "github.com/BrokkAi/acp-go/schema/unstable"
)

func requireNes(initialization schema.InitializeResponse) error {
	if initialization.AgentCapabilities == nil || initialization.AgentCapabilities.Nes == nil {
		return fmt.Errorf("agent did not advertise next edit suggestion support")
	}
	return nil
}

// StartNes opens a next-edit-suggestion session and returns its identifier.
func StartNes(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, request schema.StartNesRequest) (schema.StartNesResponse, error) {
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
func SuggestNes(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, request schema.SuggestNesRequest) (schema.SuggestNesResponse, error) {
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
func AcceptNes(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, notification schema.AcceptNesNotification) error {
	if err := requireNes(initialization); err != nil {
		return err
	}
	if err := acpvalidate.Identifier("NES session ID", string(notification.SessionID)); err != nil {
		return err
	}
	if err := acpvalidate.Identifier("NES suggestion ID", string(notification.ID)); err != nil {
		return err
	}
	return connection.Notify(ctx, schema.NesAcceptMethodName, notification)
}

// RejectNes reports that the user dismissed one suggestion.
func RejectNes(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, notification schema.RejectNesNotification) error {
	if err := requireNes(initialization); err != nil {
		return err
	}
	if err := acpvalidate.Identifier("NES session ID", string(notification.SessionID)); err != nil {
		return err
	}
	if err := acpvalidate.Identifier("NES suggestion ID", string(notification.ID)); err != nil {
		return err
	}
	return connection.Notify(ctx, schema.NesRejectMethodName, notification)
}

// CloseNes ends a next-edit-suggestion session.
func CloseNes(ctx context.Context, connection *acp.Connection, initialization schema.InitializeResponse, request schema.CloseNesRequest) (schema.CloseNesResponse, error) {
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

// HandleNesNotifications composes the agent-side NES notification handling with
// another notification handler. Pass nil for next when NES is the only hosted
// notification surface.
func HandleNesNotifications(next acp.Notifications, handler any) acp.Notifications {
	nes, _ := handler.(NesHandler)
	return func(method string, raw json.RawMessage) error {
		switch method {
		case schema.NesAcceptMethodName, schema.NesRejectMethodName:
			if nes == nil {
				return nil
			}
		default:
			if next == nil {
				return nil
			}
			return next(method, raw)
		}
		if method == schema.NesAcceptMethodName {
			notification, err := decode[schema.AcceptNesNotification](raw)
			if err != nil {
				return err
			}
			return nes.AcceptNes(context.Background(), notification)
		}
		notification, err := decode[schema.RejectNesNotification](raw)
		if err != nil {
			return err
		}
		return nes.RejectNes(context.Background(), notification)
	}
}
