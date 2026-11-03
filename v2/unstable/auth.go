package unstable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/initresult"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	"github.com/BrokkAi/acp-go/v2/agent"
)

// AuthStatusHandler serves the agent side of auth/status. Advertise (and
// Install) announces capabilities.auth.status for it.
//
// AuthStatus is a pure query: it must not change any agent state, and it must
// not answer auth_required (-32000), since clients call it to learn whether
// authentication is required. It should answer quickly, from local
// information where it can; a check that would block should answer
// schema.AuthStateUnknown and push the result later with
// NotifyAuthStatusUpdate. Handle sends the response as returned, as the
// reference SDK does. accounts lists the credentials the agent itself holds,
// for display only, and must not include secrets.
type AuthStatusHandler interface {
	AuthStatus(context.Context, schema.AuthStatusRequest) (schema.AuthStatusResponse, error)
}

// AuthStatus sends auth/status, gated on capabilities.auth.status. It fails
// before writing when the agent did not advertise the capability, as the
// auth-status RFD requires clients to check it first.
//
// Act on State only, and treat a value other than the four known states as
// schema.AuthStateUnknown; the value itself is preserved. Accounts is display
// information: do not use it to decide whether to authenticate, as a stable
// account identifier, or in logs or telemetry. As in the reference SDK, a
// response without a state (omitted or null) is an error, while a malformed
// accounts value reads as no accounts and a malformed entry is skipped.
func AuthStatus(ctx context.Context, connection *acpv2.Connection, initialization schema.InitializeResponse, request schema.AuthStatusRequest) (schema.AuthStatusResponse, error) {
	var result schema.AuthStatusResponse
	if !advertisesAuthStatus(initialization) {
		return result, fmt.Errorf("agent did not advertise %s support", schema.AuthStatusMethodName)
	}
	return result, connection.Call(ctx, schema.AuthStatusMethodName, request, &result)
}

// AuthStatusUpdates adapts the agent's auth/status_update notification to
// handler and ignores every other notification. Compose it with the
// connection's other notification handlers:
//
//	notifications := acpv2.CombineNotifications(acpv2.SessionUpdates(onUpdate), unstable.AuthStatusUpdates(onAuthStatus))
//
// Each update is a full snapshot that replaces the previous auth status; the
// decoding rules are those of AuthStatus. A notification that does not decode
// returns its error, which goes to the connection's error handler.
func AuthStatusUpdates(handler func(schema.AuthStatusUpdateNotification) error) acp.Notifications {
	return func(method string, raw json.RawMessage) error {
		if method != schema.AuthStatusUpdateMethodName {
			return nil
		}
		var notification schema.AuthStatusUpdateNotification
		if err := json.Unmarshal(raw, &notification); err != nil {
			return fmt.Errorf("decode %s: %w", method, err)
		}
		return handler(notification)
	}
}

// NotifyAuthStatusUpdate sends auth/status_update to the client, which needs
// no client capability. Only an agent that advertises capabilities.auth.status
// may send it, at any time after initialize: when ctx is the context of a
// request the agent runtime dispatched, such as the auth/login or auth/logout
// that changed the status, NotifyAuthStatusUpdate fails before writing when
// that connection's initialize response did not announce the capability.
// Outside such a request it cannot check and sends. To push a status later,
// after answering auth/status with schema.AuthStateUnknown, for example, keep
// the check by sending with context.WithoutCancel of the request's context,
// which carries the connection's initialize response past the request.
//
// Send a full snapshot, the same fields AuthStatus answers, and only when it
// differs from the last one sent on the connection.
func NotifyAuthStatusUpdate(ctx context.Context, client agent.Client, notification schema.AuthStatusUpdateNotification) error {
	if client == nil {
		return errors.New("auth/status_update requires a client")
	}
	if raw := initresult.From(ctx); raw != nil {
		var sent schema.InitializeResponse
		if err := json.Unmarshal(raw, &sent); err != nil || !advertisesAuthStatus(sent) {
			return fmt.Errorf("agent did not advertise %s support", schema.AuthStatusMethodName)
		}
	}
	return client.Notify(ctx, schema.AuthStatusUpdateMethodName, notification)
}

func advertisesAuthStatus(initialization schema.InitializeResponse) bool {
	capabilities := initialization.Capabilities
	return capabilities != nil && capabilities.Auth != nil && capabilities.Auth.Status != nil
}

// advertiseAuthStatus adds capabilities.auth.status for an AuthStatusHandler,
// keeping any value already set.
func advertiseAuthStatus(handler any, response *schema.InitializeResponse) {
	if _, ok := handler.(AuthStatusHandler); !ok {
		return
	}
	if response.Capabilities == nil {
		response.Capabilities = &schema.AgentCapabilities{}
	}
	if response.Capabilities.Auth == nil {
		response.Capabilities.Auth = &schema.AgentAuthCapabilities{}
	}
	if response.Capabilities.Auth.Status == nil {
		response.Capabilities.Auth.Status = &schema.AuthStatusCapabilities{}
	}
}
