// Package v2 is an explicit, draft-quality Go client for Agent Client
// Protocol v2. It deliberately shares no wire types with the stable v1 API.
package v2

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/acpvalidate"
	"github.com/BrokkAi/acp-go/internal/negotiate"
	"github.com/BrokkAi/acp-go/internal/rpcctl"
	schemav1 "github.com/BrokkAi/acp-go/schema"
	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// Version is the ACP v2 wire version implemented by this package.
const Version = schema.ProtocolVersion(2)

type (
	Capabilities   = schema.ClientCapabilities
	Initialization = schema.InitializeResponse
	Session        = schema.NewSessionResponse
	SessionID      = schema.SessionId
	MessageID      = schema.MessageId
	Content        = schema.ContentBlock
	Update         = schema.UpdateSessionNotification
	ClientInfo     = schema.Implementation
)

// Connection adapts the transport-level acp.Connection to generated v2 types.
// Generic Call and Notify remain available for protocol extensions.
type Connection struct {
	*acp.Connection
	mu           sync.Mutex
	initialized  bool
	initializing bool
	// repeatable records whether a completed initialization may still be
	// repeated: from its completion until the first other request this
	// connection sends, as the reference SDK's initiator allows.
	repeatable bool
	// initialization is the response from the most recent successful
	// initialize. It is guarded by mu.
	initialization Initialization
}

// Connect starts a v2 connection. It always applies
// acp.WithProtocolVersion(Version), after any options given, so a request with
// a null id is answered with -32600 and id null, which v2 requires.
//
// The connection follows the reference SDK's v2 initiator lifecycle, whatever
// sends the traffic, including the embedded acp.Connection's Call:
//   - every initialize it sends is negotiated and recorded: its
//     protocolVersion is set to 2 and its params validated before it is
//     written, and its response is validated when it arrives, even when the
//     caller stopped waiting;
//   - until an initialize has completed, and while a repeat is in progress, it
//     refuses its own requests and notifications with -32600 ("ACP
//     initialization must complete before `<method>` can be used") without
//     writing them, except a $/cancel_request during an initialize; the agent's
//     requests get the same error without running onRequest, successful
//     responses either way become that error, and the agent's notifications
//     are dropped;
//   - an initialize request from the agent is negotiated like an agent would
//     and then refused with -32600, as clients never answer one.
func Connect(in io.ReadCloser, out io.WriteCloser, onRequest acp.Handler, onNotification acp.Notifications, options ...acp.ConnectOption) *Connection {
	c := &Connection{}
	options = append(options[:len(options):len(options)],
		acp.WithProtocolVersion(uint16(Version)),
		rpcctl.WithRequestGuard(c.incomingRequest).(acp.ConnectOption),
		rpcctl.WithNotificationGuard(func(method string) error { return c.allowed(method, true) }).(acp.ConnectOption),
		rpcctl.WithOutgoingGuard(c.outgoing).(acp.ConnectOption),
		rpcctl.WithOutgoingResponseGuard(c.outgoingResponse).(acp.ConnectOption),
		rpcctl.WithIncomingResponseGuard(c.incomingResponse).(acp.ConnectOption))
	c.Connection = acp.Connect(in, out, onRequest, onNotification, options...)
	return c
}

// allowed applies the reference SDK's ensure_initialized and, for a
// notification, ensure_notification_allowed.
func (c *Connection) allowed(method string, notification bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.initialized && !c.initializing:
		return nil
	case notification && c.initializing && method == schema.CancelRequestMethodName:
		return nil
	}
	return negotiate.InitializationRequired(method)
}

// outgoing vets a request or notification before it is written. An
// initialize request is prepared and begun as the reference SDK's
// outgoing_message does; any other admitted request closes the repeat
// window.
func (c *Connection) outgoing(method string, params json.RawMessage, notification bool) (json.RawMessage, error) {
	if !notification && negotiate.SuccessorEnclosesInitialize(method, params) {
		return nil, negotiate.InvalidPrewrappedInitialize()
	}
	if !notification && method == schema.InitializeMethodName {
		rewritten, err := negotiate.OutgoingV2Params(params)
		if err != nil {
			return nil, err
		}
		if err := c.beginInitialize(); err != nil {
			return nil, err
		}
		return rewritten, nil
	}
	if err := c.allowed(method, notification); err != nil {
		return nil, err
	}
	if !notification {
		c.mu.Lock()
		c.repeatable = false
		c.mu.Unlock()
	}
	return nil, nil
}

// incomingRequest vets a request from the agent, in wire order. An
// initialize is negotiated first, as the reference SDK's
// incoming_initialize_request does, and then refused: the agent never sends
// one. Other requests wait for initialization.
func (c *Connection) incomingRequest(method string, params json.RawMessage) error {
	if method == schema.InitializeMethodName {
		if _, err := negotiate.V2Params(params); err != nil {
			return err
		}
		return negotiate.InvalidInitializeDirection()
	}
	return c.allowed(method, false)
}

// outgoingResponse turns a successful response sent while no initialization
// is complete into the initialization-required error, as the reference SDK's
// outgoing_response_to does.
func (c *Connection) outgoingResponse(_ json.RawMessage, method string, result json.RawMessage, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	if err := c.allowed(method, false); err != nil {
		return nil, err
	}
	return result, nil
}

// incomingResponse checks each response against the lifecycle, as the
// reference SDK's incoming_response does. An initialize response completes or
// fails the initialization; any other successful response received while no
// initialization is complete becomes the initialization-required error.
func (c *Connection) incomingResponse(method string, acceptsLower bool, result json.RawMessage, err error) (json.RawMessage, error) {
	if method != schema.InitializeMethodName {
		if err != nil {
			return nil, err
		}
		if err := c.allowed(method, false); err != nil {
			return nil, err
		}
		return result, nil
	}
	if err != nil {
		c.completeInitialize(Initialization{}, false)
		return nil, err
	}
	outcome, err := decodeInitializeOutcome(result, acceptsLower)
	if err != nil || outcome.Initialized == nil {
		c.completeInitialize(Initialization{}, false)
		return result, err
	}
	c.completeInitialize(*outcome.Initialized, true)
	return result, nil
}

// beginInitialize starts a first or repeated initialization like the
// reference SDK's initiator: a repeat is allowed after the previous one
// completed and before any other request was sent.
func (c *Connection) beginInitialize() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.initializing:
		return negotiate.InvalidRequest("ACP initialization is already in progress on this connection")
	case c.initialized && !c.repeatable:
		return negotiate.InvalidRequest("ACP initialization cannot be repeated after any other request; reconnect to initialize again")
	default:
		c.initializing = true
		c.repeatable = false
		return nil
	}
}

// completeInitialize ends an initialization. A success replaces the recorded
// initialization; a failure keeps the previous one, if any, and the client may
// repeat again.
func (c *Connection) completeInitialize(initialization Initialization, success bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initializing = false
	if success {
		c.initialized = true
		c.initialization = initialization
	}
	c.repeatable = c.initialized
}

func (c *Connection) Initialize(ctx context.Context, capabilities Capabilities) (Initialization, error) {
	return c.InitializeWithInfo(ctx, capabilities, ClientInfo{Name: "acp-go-v2", Version: "0.0"})
}

// InitializeWithInfo negotiates ACP v2 with the calling application's
// identity. It succeeds only when the agent selects v2. When the agent selects
// a lower version, it returns an *acp.InitializationVersionMismatch error
// carrying the agent's validated response; use SendInitialize to receive that
// selection as an outcome instead. Either way a failed initialization leaves
// the connection open with its previous initialization, if any, in place, so
// the caller may retry, fall back, or close it.
//
// As in the reference SDK, a completed initialization may be repeated until
// the connection sends any other request; the latest successful one is
// recorded. A repeat after another request, or while one is in progress,
// fails locally with -32600 (Invalid request). If ctx ends first, the
// initialization stays in progress until the agent's response arrives, which
// then completes or fails it.
func (c *Connection) InitializeWithInfo(ctx context.Context, capabilities Capabilities, info ClientInfo) (Initialization, error) {
	return c.initialize(ctx, capabilities, info, nil)
}

func (c *Connection) initialize(ctx context.Context, capabilities Capabilities, info ClientInfo, extended any) (Initialization, error) {
	outcome, err := c.sendInitialize(ctx, capabilities, info, extended)
	if err != nil {
		return Initialization{}, err
	}
	if outcome.LowerVersion != nil {
		return Initialization{}, outcome.LowerVersion
	}
	return *outcome.Initialized, nil
}

// InitializeOutcome is how the agent answered SendInitialize, like the
// reference SDK's v2::InitializeOutcome. Exactly one field is set.
type InitializeOutcome struct {
	// Initialized is the agent's response when it selected v2. The
	// connection is ready for v2 traffic.
	Initialized *Initialization
	// LowerVersion is set when the agent selected a lower protocol version,
	// as a v1-only agent legitimately does. That is a successful reply, not an
	// error, but this connection cannot carry the selected version's traffic
	// and never converts it: it stays open with its previous initialization,
	// if any, in place. Fall back to an implementation of the selected version
	// if that is acceptable, for example over a new connection, or close this
	// one.
	LowerVersion *acp.InitializationVersionMismatch
}

// SendInitialize sends an initialize request for ACP v2 and reports how the
// agent negotiated it, like the reference SDK's send_initialize. A response
// selecting v2 is validated as a v2 response; a response selecting v1 is
// validated as a v1 response and reported as InitializeOutcome.LowerVersion.
// Errors returned by the agent, including -32022 when it supports no version
// at or below v2, malformed responses, responses selecting any other version,
// and transport failures are returned as errors, with the reference SDK's
// codes and data: a missing or invalid protocolVersion is -32602, a version
// other than 1 or 2 is -32600, and a v2 response whose info does not parse is
// -32700. Every outcome other than a v2 selection leaves the connection open
// with its previous initialization, if any, in place.
func (c *Connection) SendInitialize(ctx context.Context, capabilities Capabilities, info ClientInfo) (InitializeOutcome, error) {
	return c.sendInitialize(ctx, capabilities, info, nil)
}

func (c *Connection) sendInitialize(ctx context.Context, capabilities Capabilities, info ClientInfo, extended any) (InitializeOutcome, error) {
	if info.Name == "" || info.Version == "" {
		return InitializeOutcome{}, fmt.Errorf("client name and version are required")
	}
	// The reference SDK serializes the request in its struct order.
	request := struct {
		ProtocolVersion schema.ProtocolVersion `json:"protocolVersion"`
		Info            ClientInfo             `json:"info"`
		Capabilities    *Capabilities          `json:"capabilities,omitempty"`
	}{Version, info, &capabilities}
	var raw json.RawMessage
	if err := c.Connection.Call(rpcctl.WithAcceptsLowerVersion(ctx), schema.InitializeMethodName, request, &raw); err != nil {
		return InitializeOutcome{}, err
	}
	outcome, err := decodeInitializeOutcome(raw, true)
	if err != nil || extended == nil || outcome.Initialized == nil {
		return outcome, err
	}
	return outcome, decodeExtended(raw, extended)
}

// decodeInitializeOutcome types an initialize result by the protocol version
// it selects, as the reference SDK's incoming_initialize_response does.
// acceptsLower marks a SendInitialize request, which receives a v1 selection
// as an outcome; any other request fails with -32600.
func decodeInitializeOutcome(raw json.RawMessage, acceptsLower bool) (InitializeOutcome, error) {
	version, err := negotiate.SelectedVersion(raw)
	if err != nil {
		return InitializeOutcome{}, err
	}
	switch version {
	case uint16(Version):
		if err := negotiate.CheckV2Response(raw); err != nil {
			return InitializeOutcome{}, err
		}
		var result Initialization
		if err := negotiate.DecodeResponse(raw, &result); err != nil {
			return InitializeOutcome{}, err
		}
		return InitializeOutcome{Initialized: &result}, nil
	case uint16(acp.Version):
		if !acceptsLower {
			return InitializeOutcome{}, negotiate.RequiredVersion(uint16(Version), version)
		}
		var result schemav1.InitializeResponse
		if err := negotiate.DecodeResponse(raw, &result); err != nil {
			return InitializeOutcome{}, err
		}
		return InitializeOutcome{LowerVersion: &acp.InitializationVersionMismatch{
			Requested: schemav1.ProtocolVersion(Version),
			Response:  acp.SelectedInitializeResponse{V1: &result},
		}}, nil
	default:
		return InitializeOutcome{}, negotiate.UnsupportedSelectedVersion(version, uint16(Version))
	}
}

func (c *Connection) AuthLogin(ctx context.Context, initialization Initialization, method string) error {
	advertised, err := findAgentAuthMethod(initialization, method)
	if err != nil {
		return err
	}
	var result schema.LoginAuthResponse
	return c.Call(ctx, schema.AuthLoginMethodName, schema.LoginAuthRequest{MethodID: advertised}, &result)
}

func (c *Connection) AuthLogout(ctx context.Context, initialization Initialization) error {
	if len(initialization.AuthMethods) == 0 {
		return fmt.Errorf("agent did not advertise authentication methods")
	}
	var result schema.LogoutAuthResponse
	return c.Call(ctx, schema.AuthLogoutMethodName, schema.LogoutAuthRequest{}, &result)
}

func findAgentAuthMethod(initialization Initialization, method string) (schema.AuthMethodId, error) {
	if method == "" {
		return "", fmt.Errorf("authentication method is required")
	}
	if len(initialization.AuthMethods) == 0 {
		return "", fmt.Errorf("agent did not advertise authentication methods")
	}
	wanted := schema.AuthMethodId(method)
	for i := range initialization.AuthMethods {
		candidate := &initialization.AuthMethods[i]
		if candidate.Agent != nil && candidate.Agent.MethodID == wanted {
			return wanted, nil
		}
		if candidate.Terminal != nil && candidate.Terminal.MethodID == wanted {
			return "", fmt.Errorf("authentication %s is terminal-based and must be completed outside this connection", method)
		}
	}
	return "", fmt.Errorf("agent did not advertise authentication method %q", method)
}

// NewSessionOptions carries optional fields for session/new.
type NewSessionOptions struct {
	AdditionalDirectories []string
}

// NewSession starts a session using the initialization response from the most
// recent successful Initialize call. Use NewSessionWithOptions when the
// initialization is held elsewhere.
func (c *Connection) NewSession(ctx context.Context, directory string) (Session, error) {
	initialization, ok := c.initializationSnapshot()
	if !ok {
		return Session{}, fmt.Errorf("connection must be initialized before starting a session")
	}
	return c.NewSessionWithOptions(ctx, initialization, directory, NewSessionOptions{})
}

// initializationSnapshot returns the most recent successful initialization.
// ok is false until Initialize has completed successfully.
func (c *Connection) initializationSnapshot() (Initialization, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initialization, c.initialized
}

func (c *Connection) NewSessionWithOptions(ctx context.Context, initialization Initialization, directory string, options NewSessionOptions) (Session, error) {
	var session Session
	if err := requireSessionCapabilities(initialization); err != nil {
		return session, err
	}
	if err := validateAbsolutePath(directory); err != nil {
		return session, err
	}
	if err := validateAdditionalDirectories(initialization, options.AdditionalDirectories); err != nil {
		return session, err
	}
	request := schema.NewSessionRequest{
		Cwd:                   schema.AbsolutePath(directory),
		AdditionalDirectories: absolutePaths(options.AdditionalDirectories),
	}
	if err := c.Call(ctx, schema.SessionNewMethodName, request, &session); err != nil {
		return session, err
	}
	if session.SessionID == "" {
		return session, fmt.Errorf("agent returned an empty session ID")
	}
	return session, nil
}

func (c *Connection) ResumeSession(ctx context.Context, initialization Initialization, request schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	var result schema.ResumeSessionResponse
	if err := requireSessionCapabilities(initialization); err != nil {
		return result, err
	}
	if err := validateAbsolutePath(string(request.Cwd)); err != nil {
		return result, err
	}
	if request.SessionID == "" {
		return result, fmt.Errorf("session ID is required")
	}
	if len(request.MCPServers) > 0 {
		return result, fmt.Errorf("MCP servers require the explicit github.com/BrokkAi/acp-go/v2/mcp package")
	}
	if err := validateAdditionalDirectories(initialization, absoluteStrings(request.AdditionalDirectories)); err != nil {
		return result, err
	}
	return result, c.Call(ctx, schema.SessionResumeMethodName, request, &result)
}

func (c *Connection) Prompt(ctx context.Context, initialization Initialization, session Session, prompt string) (MessageID, error) {
	return c.PromptContent(ctx, initialization, session, []Content{NewTextContent(prompt)})
}

// PromptContent submits a prompt and returns once the agent inserts the user
// message into the conversation. The returned ID identifies that message; the
// matching user_message session update carries the same ID and may arrive
// before or after this call returns. In ACP v2, foreground completion is
// reported later through idle state updates.
func (c *Connection) PromptContent(ctx context.Context, initialization Initialization, session Session, prompt []Content) (MessageID, error) {
	if err := requireSessionCapabilities(initialization); err != nil {
		return "", err
	}
	if session.SessionID == "" {
		return "", fmt.Errorf("session ID is required")
	}
	if err := validatePromptContent(initialization, prompt); err != nil {
		return "", err
	}
	var result schema.PromptResponse
	if err := c.Call(ctx, schema.SessionPromptMethodName, schema.PromptRequest{
		SessionID: session.SessionID,
		Prompt:    prompt,
	}, &result); err != nil {
		return "", err
	}
	if result.MessageID == "" {
		return "", fmt.Errorf("agent accepted the prompt without the required user message ID")
	}
	return result.MessageID, nil
}

func (c *Connection) CancelSession(ctx context.Context, sessionID SessionID) error {
	if sessionID == "" {
		return fmt.Errorf("session ID is required")
	}
	return c.Notify(ctx, schema.SessionCancelMethodName, schema.CancelSessionNotification{SessionID: sessionID})
}

func (c *Connection) CloseSession(ctx context.Context, initialization Initialization, sessionID SessionID) error {
	if err := requireSessionCapabilities(initialization); err != nil {
		return err
	}
	if sessionID == "" {
		return fmt.Errorf("session ID is required")
	}
	var result schema.CloseSessionResponse
	return c.Call(ctx, schema.SessionCloseMethodName, schema.CloseSessionRequest{SessionID: sessionID}, &result)
}

func (c *Connection) ListSessions(ctx context.Context, initialization Initialization, request schema.ListSessionsRequest) (schema.ListSessionsResponse, error) {
	var result schema.ListSessionsResponse
	if err := requireSessionCapabilities(initialization); err != nil {
		return result, err
	}
	if request.Cwd != nil {
		if err := validateAbsolutePath(string(*request.Cwd)); err != nil {
			return result, err
		}
	}
	return result, c.Call(ctx, schema.SessionListMethodName, request, &result)
}

func (c *Connection) DeleteSession(ctx context.Context, initialization Initialization, sessionID SessionID) error {
	var capabilities *schema.SessionCapabilities
	if initialization.Capabilities != nil {
		capabilities = initialization.Capabilities.Session
	}
	if capabilities == nil || capabilities.Delete == nil {
		return fmt.Errorf("agent did not advertise session/delete support")
	}
	if sessionID == "" {
		return fmt.Errorf("session ID is required")
	}
	var result schema.DeleteSessionResponse
	return c.Call(ctx, schema.SessionDeleteMethodName, schema.DeleteSessionRequest{SessionID: sessionID}, &result)
}

func requireSessionCapabilities(initialization Initialization) error {
	if initialization.Capabilities == nil || initialization.Capabilities.Session == nil {
		return fmt.Errorf("agent did not advertise the ACP v2 session method surface")
	}
	return nil
}

func validateAbsolutePath(path string) error {
	return acpvalidate.AbsolutePath("ACP path", path)
}

func validateAdditionalDirectories(initialization Initialization, directories []string) error {
	if len(directories) == 0 {
		return nil
	}
	var capabilities *schema.SessionCapabilities
	if initialization.Capabilities != nil {
		capabilities = initialization.Capabilities.Session
	}
	if capabilities == nil || capabilities.AdditionalDirectories == nil {
		return fmt.Errorf("agent did not advertise additionalDirectories support")
	}
	for _, directory := range directories {
		if err := validateAbsolutePath(directory); err != nil {
			return err
		}
	}
	return nil
}

func validatePromptContent(initialization Initialization, prompt []Content) error {
	var capabilities *schema.PromptCapabilities
	if initialization.Capabilities != nil && initialization.Capabilities.Session != nil {
		capabilities = initialization.Capabilities.Session.Prompt
	}
	for i, block := range prompt {
		switch {
		case block.Text != nil:
		case block.ResourceLink != nil:
			if err := acpvalidate.URI("resource link URI", block.ResourceLink.URI); err != nil {
				return fmt.Errorf("prompt block %d: %w", i, err)
			}
		case block.Image != nil:
			if capabilities == nil || capabilities.Image == nil {
				return fmt.Errorf("agent did not advertise image prompt support (block %d)", i)
			}
			if err := acpvalidate.MediaType("image content media type", string(block.Image.MimeType)); err != nil {
				return fmt.Errorf("prompt block %d: %w", i, err)
			}
			if block.Image.URI.Set && !block.Image.URI.Null {
				if err := acpvalidate.URI("image content URI", block.Image.URI.Value); err != nil {
					return fmt.Errorf("prompt block %d: %w", i, err)
				}
			}
		case block.Audio != nil:
			if capabilities == nil || capabilities.Audio == nil {
				return fmt.Errorf("agent did not advertise audio prompt support (block %d)", i)
			}
			if err := acpvalidate.MediaType("audio content media type", string(block.Audio.MimeType)); err != nil {
				return fmt.Errorf("prompt block %d: %w", i, err)
			}
		case block.Resource != nil:
			if capabilities == nil || capabilities.EmbeddedContext == nil {
				return fmt.Errorf("agent did not advertise embedded context prompt support (block %d)", i)
			}
			if err := validateEmbeddedResource(i, block.Resource); err != nil {
				return err
			}
		default:
			return fmt.Errorf("prompt block %d is not enabled by the negotiated capabilities", i)
		}
	}
	return nil
}

// validateEmbeddedResource checks the URI, and any declared media type, of a
// draft-v2 embedded text or blob resource.
func validateEmbeddedResource(index int, resource *schema.EmbeddedResource) error {
	var uri string
	var mimeType *schema.MediaType
	switch contents := resource.Resource; {
	case contents.TextResourceContents != nil:
		uri, mimeType = contents.TextResourceContents.URI, contents.TextResourceContents.MimeType
	case contents.BlobResourceContents != nil:
		uri, mimeType = contents.BlobResourceContents.URI, contents.BlobResourceContents.MimeType
	default:
		return fmt.Errorf("prompt block %d: embedded resource requires a text or blob resource", index)
	}
	if err := acpvalidate.URI("embedded resource URI", uri); err != nil {
		return fmt.Errorf("prompt block %d: %w", index, err)
	}
	if mimeType != nil {
		if err := acpvalidate.MediaType("embedded resource media type", string(*mimeType)); err != nil {
			return fmt.Errorf("prompt block %d: %w", index, err)
		}
	}
	return nil
}

func absolutePaths(paths []string) []schema.AbsolutePath {
	if paths == nil {
		return nil
	}
	result := make([]schema.AbsolutePath, len(paths))
	for i, path := range paths {
		result[i] = schema.AbsolutePath(path)
	}
	return result
}

func absoluteStrings(paths []schema.AbsolutePath) []string {
	if paths == nil {
		return nil
	}
	result := make([]string, len(paths))
	for i, path := range paths {
		result[i] = string(path)
	}
	return result
}
