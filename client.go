package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"
)

const Version = 1

type Capabilities struct {
	FS struct {
		Read  bool `json:"readTextFile"`
		Write bool `json:"writeTextFile"`
	} `json:"fs"`
	Terminal bool `json:"terminal"`
}
type Initialization struct {
	ProtocolVersion   int             `json:"protocolVersion"`
	AgentCapabilities json.RawMessage `json:"agentCapabilities"`
	AuthMethods       []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"authMethods"`
}
type Session struct {
	ID            string         `json:"sessionId"`
	ConfigOptions []ConfigOption `json:"configOptions,omitempty"`
}
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type Update struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		Kind       string          `json:"sessionUpdate"`
		Content    json.RawMessage `json:"content"`
		Title      string          `json:"title"`
		MessageID  string          `json:"messageId"`
		ToolCallID string          `json:"toolCallId"`
		Status     string          `json:"status"`
		RawOutput  json.RawMessage `json:"rawOutput"`
		Meta       struct {
			TerminalOutput *struct {
				Data string `json:"data"`
			} `json:"terminal_output_delta"`
		} `json:"_meta"`
	} `json:"update"`
}

// ClientInfo identifies the application to the agent.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (c *Connection) Initialize(ctx context.Context, caps Capabilities) (Initialization, error) {
	return c.InitializeWithInfo(ctx, caps, ClientInfo{Name: "acp-go", Version: "0.1.0"})
}

// InitializeWithInfo negotiates ACP v1 with the calling application's identity.
func (c *Connection) InitializeWithInfo(ctx context.Context, caps Capabilities, info ClientInfo) (Initialization, error) {
	var result Initialization
	err := c.Call(ctx, "initialize", map[string]any{"protocolVersion": Version, "clientCapabilities": caps, "clientInfo": info}, &result)
	if err == nil && result.ProtocolVersion != Version {
		_ = c.Close()
		err = fmt.Errorf("agent selected unsupported ACP version %d", result.ProtocolVersion)
	}
	return result, err
}
func (c *Connection) Authenticate(ctx context.Context, init Initialization, method string) error {
	for _, m := range init.AuthMethods {
		if m.ID == method {
			if m.Type != "" && m.Type != "agent" {
				return fmt.Errorf("authentication %s must be completed outside this unattended client", method)
			}
			return c.Call(ctx, "authenticate", map[string]string{"methodId": method}, nil)
		}
	}
	return fmt.Errorf("agent did not advertise authentication method %q", method)
}
func (c *Connection) NewSession(ctx context.Context, directory string) (Session, error) {
	var s Session
	if !filepath.IsAbs(directory) {
		return s, fmt.Errorf("ACP working directory must be absolute")
	}
	err := c.Call(ctx, "session/new", map[string]any{"cwd": directory, "mcpServers": []any{}}, &s)
	if err == nil && s.ID == "" {
		err = fmt.Errorf("agent returned an empty session ID")
	}
	return s, err
}
func (c *Connection) SetMode(ctx context.Context, s *Session, mode string) error {
	if option := s.selector("mode"); option != nil {
		return c.setSelection(ctx, s, *option, mode)
	}
	return c.Call(ctx, "session/set_mode", map[string]string{"sessionId": s.ID, "modeId": mode}, nil)
}
func (c *Connection) Prompt(ctx context.Context, s Session, text string) (string, error) {
	var result struct {
		StopReason string `json:"stopReason"`
	}
	err := c.Call(ctx, "session/prompt", map[string]any{"sessionId": s.ID, "prompt": []Content{{Type: "text", Text: text}}}, &result)
	if ctx.Err() != nil {
		cancelCtx, stop := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_ = c.Notify(cancelCtx, "session/cancel", map[string]string{"sessionId": s.ID})
		stop()
	}
	return result.StopReason, err
}
