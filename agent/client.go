package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
)

type clientConnection struct {
	connectionReady chan struct{}
	connectionValue *acp.Connection

	mu           sync.RWMutex
	initialized  bool
	capabilities *schema.ClientCapabilities
}

var _ Client = (*clientConnection)(nil)

func (c *clientConnection) attach(connection *acp.Connection) {
	c.connectionValue = connection
	close(c.connectionReady)
}

func (c *clientConnection) connection() *acp.Connection {
	<-c.connectionReady
	return c.connectionValue
}

func (c *clientConnection) initialize(capabilities *schema.ClientCapabilities) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initialized = true
	if capabilities != nil {
		copied := *capabilities
		capabilities = &copied
	}
	c.capabilities = capabilities
}

func (c *clientConnection) ready() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.initialized {
		return &acp.RPCError{Code: -32600, Message: "client has not initialized the agent"}
	}
	return nil
}

func (c *clientConnection) clientCapabilities() schema.ClientCapabilities {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.capabilities == nil {
		return schema.ClientCapabilities{}
	}
	return *c.capabilities
}

func (c *clientConnection) Call(ctx context.Context, method string, params, result any) error {
	return c.connection().Call(ctx, method, params, result)
}

func (c *clientConnection) Notify(ctx context.Context, method string, params any) error {
	return c.connection().Notify(ctx, method, params)
}

func (c *clientConnection) ReadTextFile(ctx context.Context, request schema.ReadTextFileRequest) (schema.ReadTextFileResponse, error) {
	var result schema.ReadTextFileResponse
	capabilities := c.clientCapabilities()
	supported := capabilities.Fs != nil && capabilities.Fs.ReadTextFile != nil && *capabilities.Fs.ReadTextFile
	if err := c.requireHostMethod(supported, schema.FsReadTextFileMethodName); err != nil {
		return result, err
	}
	return result, c.connection().Call(ctx, schema.FsReadTextFileMethodName, request, &result)
}

func (c *clientConnection) WriteTextFile(ctx context.Context, request schema.WriteTextFileRequest) (schema.WriteTextFileResponse, error) {
	var result schema.WriteTextFileResponse
	capabilities := c.clientCapabilities()
	supported := capabilities.Fs != nil && capabilities.Fs.WriteTextFile != nil && *capabilities.Fs.WriteTextFile
	if err := c.requireHostMethod(supported, schema.FsWriteTextFileMethodName); err != nil {
		return result, err
	}
	return result, c.connection().Call(ctx, schema.FsWriteTextFileMethodName, request, &result)
}

func (c *clientConnection) RequestPermission(ctx context.Context, request schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
	var result schema.RequestPermissionResponse
	if err := c.ready(); err != nil {
		return result, err
	}
	return result, c.connection().Call(ctx, schema.SessionRequestPermissionMethodName, request, &result)
}

func (c *clientConnection) CreateElicitation(ctx context.Context, request schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error) {
	var result schema.CreateElicitationResponse
	capabilities := c.clientCapabilities()
	var supported bool
	switch {
	case request.Form != nil:
		supported = capabilities.Elicitation != nil && capabilities.Elicitation.Form != nil
	case request.URL != nil:
		supported = capabilities.Elicitation != nil && capabilities.Elicitation.URL != nil
	default:
		return result, fmt.Errorf("elicitation request has no recognized mode")
	}
	if err := c.requireHostMethod(supported, schema.ElicitationCreateMethodName); err != nil {
		return result, err
	}
	return result, c.connection().Call(ctx, schema.ElicitationCreateMethodName, request, &result)
}

func (c *clientConnection) CompleteElicitation(ctx context.Context, elicitationID schema.ElicitationId) error {
	capabilities := c.clientCapabilities()
	supported := capabilities.Elicitation != nil && capabilities.Elicitation.URL != nil
	if err := c.requireHostMethod(supported, schema.ElicitationCompleteMethodName); err != nil {
		return err
	}
	return c.connection().Notify(ctx, schema.ElicitationCompleteMethodName, schema.CompleteElicitationNotification{
		ElicitationID: elicitationID,
	})
}

func (c *clientConnection) CreateTerminal(ctx context.Context, request schema.CreateTerminalRequest) (schema.CreateTerminalResponse, error) {
	var result schema.CreateTerminalResponse
	capabilities := c.clientCapabilities()
	supported := capabilities.Terminal != nil && *capabilities.Terminal
	if err := c.requireHostMethod(supported, schema.TerminalCreateMethodName); err != nil {
		return result, err
	}
	return result, c.connection().Call(ctx, schema.TerminalCreateMethodName, request, &result)
}

func (c *clientConnection) TerminalOutput(ctx context.Context, request schema.TerminalOutputRequest) (schema.TerminalOutputResponse, error) {
	var result schema.TerminalOutputResponse
	capabilities := c.clientCapabilities()
	supported := capabilities.Terminal != nil && *capabilities.Terminal
	if err := c.requireHostMethod(supported, schema.TerminalOutputMethodName); err != nil {
		return result, err
	}
	return result, c.connection().Call(ctx, schema.TerminalOutputMethodName, request, &result)
}

func (c *clientConnection) WaitForTerminalExit(ctx context.Context, request schema.WaitForTerminalExitRequest) (schema.WaitForTerminalExitResponse, error) {
	var result schema.WaitForTerminalExitResponse
	capabilities := c.clientCapabilities()
	supported := capabilities.Terminal != nil && *capabilities.Terminal
	if err := c.requireHostMethod(supported, schema.TerminalWaitForExitMethodName); err != nil {
		return result, err
	}
	return result, c.connection().Call(ctx, schema.TerminalWaitForExitMethodName, request, &result)
}

func (c *clientConnection) KillTerminal(ctx context.Context, request schema.KillTerminalRequest) (schema.KillTerminalResponse, error) {
	var result schema.KillTerminalResponse
	capabilities := c.clientCapabilities()
	supported := capabilities.Terminal != nil && *capabilities.Terminal
	if err := c.requireHostMethod(supported, schema.TerminalKillMethodName); err != nil {
		return result, err
	}
	return result, c.connection().Call(ctx, schema.TerminalKillMethodName, request, &result)
}

func (c *clientConnection) ReleaseTerminal(ctx context.Context, request schema.ReleaseTerminalRequest) (schema.ReleaseTerminalResponse, error) {
	var result schema.ReleaseTerminalResponse
	capabilities := c.clientCapabilities()
	supported := capabilities.Terminal != nil && *capabilities.Terminal
	if err := c.requireHostMethod(supported, schema.TerminalReleaseMethodName); err != nil {
		return result, err
	}
	return result, c.connection().Call(ctx, schema.TerminalReleaseMethodName, request, &result)
}

func (c *clientConnection) requireHostMethod(supported bool, method string) error {
	if err := c.ready(); err != nil {
		return err
	}
	if !supported {
		return &acp.RPCError{Code: -32601, Message: "client did not advertise " + method + " support"}
	}
	return nil
}
