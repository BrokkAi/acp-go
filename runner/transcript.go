package runner

import (
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/BrokkAi/acp-go"
)

// Stream records stay structured with --json; the console joins fragments into
// continuous text instead of printing a timestamp and escaped string per token.
func stream(log *slog.Logger, source, id, text string) {
	if text != "" {
		log.Info("agent transcript", "source", source, "stream_id", id, "text", text)
	}
}

type transcriptWriter struct {
	mu         sync.Mutex
	log        *slog.Logger
	source, id string
	pending    []byte
	record     func(any) error
}

func (w *transcriptWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(data)
	w.pending = append(w.pending, data...)
	end := 0
	for end < len(w.pending) && utf8.FullRune(w.pending[end:]) {
		_, size := utf8.DecodeRune(w.pending[end:])
		end += size
	}
	text := strings.ToValidUTF8(string(w.pending[:end]), "�")
	stream(w.log, w.source, w.id, text)
	w.pending = append(w.pending[:0], w.pending[end:]...)
	if text != "" && w.record != nil {
		if err := w.record(map[string]string{"event": "process_output", "source": w.source, "stream_id": w.id, "text": text}); err != nil {
			return n, err
		}
	}
	return n, nil
}

type toolTranscript struct {
	title  string
	length int
	digest [sha256.Size]byte
	deltas bool
}

func (h *workspaceHost) showUpdate(event acp.Update) error {
	u := event.Update
	switch u.Kind {
	case "agent_message_chunk", "agent_thought_chunk":
		var content acp.Content
		if err := json.Unmarshal(u.Content, &content); err != nil {
			return err
		}
		if content.Type == "text" {
			source := "Agent"
			if u.Kind == "agent_thought_chunk" {
				source = "Thinking"
			}
			stream(h.log, source, event.SessionID, content.Text)
		}
	case "tool_call", "tool_call_update":
		tool := h.toolOutput[u.ToolCallID]
		if tool == nil {
			tool = &toolTranscript{title: u.ToolCallID}
			h.toolOutput[u.ToolCallID] = tool
		}
		if u.Title != "" {
			tool.title = u.Title
		}
		if u.Kind == "tool_call" {
			h.log.Info("Tool", "title", tool.title)
		}
		if u.Meta.TerminalOutput != nil {
			tool.deltas = true
			stream(h.log, "Tool output", u.ToolCallID, u.Meta.TerminalOutput.Data)
		} else if !tool.deltas {
			text := toolText(u.Content, u.RawOutput)
			if tool.length > 0 && len(text) >= tool.length && sha256.Sum256([]byte(text[:tool.length])) == tool.digest {
				stream(h.log, "Tool output", u.ToolCallID, text[tool.length:])
			} else {
				stream(h.log, "Tool output", u.ToolCallID, text)
			}
			if text != "" {
				tool.length = len(text)
				tool.digest = sha256.Sum256([]byte(text))
			}
		}
		if u.Status == "completed" || u.Status == "failed" {
			if u.Status == "failed" {
				h.log.Error("Tool failed", "title", tool.title)
			} else {
				h.log.Info("Tool completed", "title", tool.title)
			}
		}
	}
	return nil
}

func toolText(content, raw json.RawMessage) string {
	var blocks []struct {
		Type    string      `json:"type"`
		Content acp.Content `json:"content"`
		Path    string      `json:"path"`
		OldText string      `json:"oldText"`
		NewText string      `json:"newText"`
	}
	var text strings.Builder
	if json.Unmarshal(content, &blocks) == nil {
		for _, block := range blocks {
			if block.Type == "content" && block.Content.Type == "text" {
				text.WriteString(block.Content.Text)
			} else if block.Type == "diff" {
				text.WriteString("File: " + block.Path + "\n" + block.NewText + "\n")
			}
		}
	}
	if text.Len() > 0 {
		return text.String()
	}
	var output struct {
		Text string `json:"formatted_output"`
	}
	if json.Unmarshal(raw, &output) == nil && output.Text != "" {
		return output.Text
	}
	var plain string
	if json.Unmarshal(raw, &plain) == nil {
		return plain
	}
	if len(raw) > 0 && string(raw) != "null" {
		return string(raw) + "\n"
	}
	return ""
}
