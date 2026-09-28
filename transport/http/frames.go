package acphttp

import (
	"encoding/json"
	"errors"
)

const (
	// HeaderConnectionID names the HTTP header carrying the ACP connection id.
	HeaderConnectionID = "Acp-Connection-Id"
	// HeaderSessionID names the HTTP header carrying a session id for
	// session-scoped methods.
	HeaderSessionID = "Acp-Session-Id"

	// DefaultPath is the endpoint the client targets and the server serves
	// when no path is configured.
	DefaultPath = "/acp"

	eventStreamMIME = "text/event-stream"
	jsonMIME        = "application/json"
	maxFrame        = 8 << 20
)

// frame is the JSON-RPC shape the transport needs for routing. It is
// deliberately shallow: the payload stays raw and unvalidated here, because the
// connection framing behind it owns protocol semantics.
type frame struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

func parseFrame(data []byte) (frame, error) {
	if len(data) > 0 && data[0] == '[' {
		return frame{}, errors.New("ACP HTTP transport does not accept batch bodies")
	}
	var parsed frame
	if err := json.Unmarshal(data, &parsed); err != nil {
		return frame{}, err
	}
	if parsed.Method == "" && len(parsed.ID) == 0 {
		return frame{}, errors.New("ACP HTTP frame must be a request, notification, or response")
	}
	if parsed.Method == "" && (parsed.Result == nil) == (parsed.Error == nil) {
		return frame{}, errors.New("ACP HTTP response needs exactly one of result or error")
	}
	return parsed, nil
}

func (f frame) isRequest() bool { return f.Method != "" && len(f.ID) > 0 }

func (f frame) isInitialize() bool { return f.isRequest() && f.Method == "initialize" }

// sessionID reads the sessionId a frame carries in its params.
func (f frame) sessionID() string {
	if len(f.Params) == 0 {
		return ""
	}
	var params struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(f.Params, &params) != nil {
		return ""
	}
	return params.SessionID
}

// requiresSessionHeader mirrors the Rust crate's list of methods that must
// carry the session header.
func requiresSessionHeader(method string) bool {
	switch method {
	case "session/prompt", "session/cancel", "session/close", "session/delete",
		"session/fork", "session/load", "session/resume",
		"session/set_config_option", "session/set_mode", "session/set_model":
		return true
	default:
		return false
	}
}
