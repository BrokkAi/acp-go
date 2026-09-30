// Package v2 is an explicit, draft-quality Go client for Agent Client
// Protocol v2. It deliberately shares no wire types with the stable v1 API.
package v2

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/acpvalidate"
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
	// initialization is the response from the most recent successful
	// Initialize call. It is guarded by mu.
	initialization Initialization
}

func Connect(in io.ReadCloser, out io.WriteCloser, onRequest acp.Handler, onNotification acp.Notifications) *Connection {
	return &Connection{Connection: acp.Connect(in, out, onRequest, onNotification)}
}

func (c *Connection) beginInitialize() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.initialized:
		return fmt.Errorf("ACP connections may only be initialized once; reconnect to initialize again")
	case c.initializing:
		return fmt.Errorf("ACP initialization is already in progress on this connection")
	default:
		c.initializing = true
		return nil
	}
}

func (c *Connection) completeInitialize(initialization Initialization, success bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initializing = false
	c.initialized = success
	if success {
		c.initialization = initialization
	}
}

func (c *Connection) Initialize(ctx context.Context, capabilities Capabilities) (Initialization, error) {
	return c.InitializeWithInfo(ctx, capabilities, ClientInfo{Name: "acp-go-v2", Version: "0.0"})
}

func (c *Connection) InitializeWithInfo(ctx context.Context, capabilities Capabilities, info ClientInfo) (Initialization, error) {
	var result Initialization
	if info.Name == "" || info.Version == "" {
		return result, fmt.Errorf("client name and version are required")
	}
	if err := c.beginInitialize(); err != nil {
		return result, err
	}
	request := schema.InitializeRequest{
		Capabilities:    &capabilities,
		Info:            info,
		ProtocolVersion: Version,
	}
	if err := c.Call(ctx, schema.InitializeMethodName, request, &result); err != nil {
		return result, err
	}
	if result.ProtocolVersion != Version {
		_ = c.Close()
		err := fmt.Errorf("agent selected unsupported ACP version %d", result.ProtocolVersion)
		c.completeInitialize(Initialization{}, false)
		return result, err
	}
	if result.Info.Name == "" || result.Info.Version == "" {
		c.completeInitialize(Initialization{}, false)
		return result, fmt.Errorf("agent initialize response info requires name and version")
	}
	c.completeInitialize(result, true)
	return result, nil
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
	if err := c.Connection.Call(ctx, schema.SessionPromptMethodName, schema.PromptRequest{
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
