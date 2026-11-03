// Package acp implements the ACP v1 stdio client protocol using only Go's
// standard library. Specification: https://agentclientprotocol.com/protocol/v1/overview
package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

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
//
// It follows the reference SDK's v1 client, whose lifecycle is unchecked: an
// initialize may be sent at any time, including a repeat or one while another
// is in progress. The response's protocolVersion is checked as the reference
// SDK checks it: a missing or invalid one fails with -32602 ("initialize.protocolVersion
// must be a valid ACP protocol version"), a v2 selection with -32600 ("required
// ACP protocol version 1 but peer negotiated 2; ..."), and any other version
// with -32600 ("unsupported ACP protocol version N; this endpoint only supports
// ACP protocol version 1"). A failure leaves the connection open.
//
// A Connection is version-neutral, so Call with initialize sends the params as
// given and does not check the response; InitializeWithInfo is the v1 client
// role, which the reference SDK applies to every initialize a v1 client sends.
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
	var raw json.RawMessage
	if err := c.Call(ctx, schema.InitializeMethodName, request, &raw); err != nil {
		return result, err
	}
	if err := checkV1Selection(raw); err != nil {
		return result, err
	}
	return result, json.Unmarshal(raw, &result)
}

// checkV1Selection checks the protocolVersion of a v1 initialize response as
// the reference SDK's v1 client does.
func checkV1Selection(raw json.RawMessage) error {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return invalidRequestData(-32602, "Invalid params", "initialize.protocolVersion must be a valid ACP protocol version")
	}
	text := strings.TrimSpace(string(object["protocolVersion"]))
	if text == "" || text[0] < '0' || text[0] > '9' {
		return invalidRequestData(-32602, "Invalid params", "initialize.protocolVersion must be a valid ACP protocol version")
	}
	version, err := strconv.ParseUint(text, 10, 16)
	if err != nil {
		return invalidRequestData(-32602, "Invalid params", "initialize.protocolVersion must be a valid ACP protocol version")
	}
	switch version {
	case uint64(Version):
		return nil
	case 2:
		return invalidRequestData(-32600, "Invalid request", "required ACP protocol version 1 but peer negotiated 2; use a matching implementation for the negotiated protocol version")
	default:
		return invalidRequestData(-32600, "Invalid request", fmt.Sprintf("unsupported ACP protocol version %d; this endpoint only supports ACP protocol version 1", version))
	}
}

// invalidRequestData is an error in the reference SDK's shape: the generic
// message, with detail as a JSON string in data.
func invalidRequestData(code int, message, detail string) *RPCError {
	data, _ := json.Marshal(detail)
	return &RPCError{Code: code, Message: message, Data: data}
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
