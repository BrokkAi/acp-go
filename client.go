// Package acp implements the ACP v1 stdio client protocol using only Go's
// standard library. Specification: https://agentclientprotocol.com/protocol/v1/overview
package acp

import (
	"context"
	"fmt"

	"github.com/BrokkAi/acp-go/schema"
)

// Version is the ACP protocol version implemented by this module.
const Version = schema.ProtocolVersion(1)

// The exported names below are intentional aliases into the generated schema
// package. Client code can use the small acp facade without losing access to
// fields introduced by future pinned schema releases.
type (
	Capabilities   = schema.ClientCapabilities
	Initialization = schema.InitializeResponse
	Session        = schema.NewSessionResponse
	SessionID      = schema.SessionId
	Content        = schema.ContentBlock
	Update         = schema.SessionNotification
	ClientInfo     = schema.Implementation
)

func (c *Connection) Initialize(ctx context.Context, caps Capabilities) (Initialization, error) {
	return c.InitializeWithInfo(ctx, caps, ClientInfo{Name: "acp-go", Version: "0.1.0"})
}

// InitializeWithInfo negotiates ACP v1 with the calling application's identity.
func (c *Connection) InitializeWithInfo(ctx context.Context, caps Capabilities, info ClientInfo) (Initialization, error) {
	var result Initialization
	if info.Name == "" || info.Version == "" {
		return result, fmt.Errorf("client name and version are required")
	}
	request := schema.InitializeRequest{
		ClientCapabilities: &caps,
		ClientInfo:         &info,
		ProtocolVersion:    Version,
	}
	err := c.Call(ctx, schema.InitializeMethodName, request, &result)
	if err == nil && result.ProtocolVersion != Version {
		_ = c.Close()
		err = fmt.Errorf("agent selected unsupported ACP version %d", result.ProtocolVersion)
	}
	return result, err
}

func (c *Connection) Authenticate(ctx context.Context, init Initialization, method string) error {
	if method == "" {
		return fmt.Errorf("authentication method is required")
	}
	for i := range init.AuthMethods {
		advertised := &init.AuthMethods[i]
		id := schema.AuthMethodId(method)
		if advertised.Agent != nil && advertised.Agent.ID == id {
			var result schema.AuthenticateResponse
			return c.Call(ctx, schema.AuthenticateMethodName, schema.AuthenticateRequest{MethodID: id}, &result)
		}
		if advertised.Terminal != nil && advertised.Terminal.ID == id {
			return fmt.Errorf("authentication %s is terminal-based and must be completed outside this connection", method)
		}
	}
	return fmt.Errorf("agent did not advertise authentication method %q", method)
}

func (c *Connection) NewSession(ctx context.Context, directory string) (Session, error) {
	return c.NewSessionWithOptions(ctx, Initialization{}, directory, NewSessionOptions{})
}

func (c *Connection) Prompt(ctx context.Context, session Session, text string) (schema.StopReason, error) {
	return c.PromptContent(ctx, Initialization{}, session, []Content{NewTextContent(text)})
}
