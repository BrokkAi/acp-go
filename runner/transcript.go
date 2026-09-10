package runner

import (
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
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
	update := event.Update
	switch {
	case update.AgentMessageChunk != nil || update.AgentThoughtChunk != nil:
		chunk := update.AgentMessageChunk
		source := "Agent"
		if chunk == nil {
			chunk = update.AgentThoughtChunk
			source = "Thinking"
		}
		if chunk.Content.Text != nil {
			stream(h.log, source, string(event.SessionID), chunk.Content.Text.Text)
		}
	case update.ToolCall != nil:
		tool := update.ToolCall
		h.log.Info("Tool", "title", tool.Title)
		h.showTool(tool.ToolCallID, tool.Title, tool.Status, tool.Content, tool.RawOutput, tool.Meta)
	case update.ToolCallUpdate != nil:
		tool := update.ToolCallUpdate
		title := string(tool.ToolCallID)
		if tool.Title != nil {
			title = *tool.Title
		}
		h.showTool(tool.ToolCallID, title, tool.Status, tool.Content, tool.RawOutput, tool.Meta)
	}
	return nil
}

func (h *workspaceHost) showTool(id schema.ToolCallId, title string, status *schema.ToolCallStatus, content []schema.ToolCallContent, rawOutput json.RawMessage, meta schema.Meta) {
	tool := h.toolOutput[string(id)]
	if tool == nil {
		tool = &toolTranscript{title: string(id)}
		h.toolOutput[string(id)] = tool
	}
	if title != "" {
		tool.title = title
	}
	if delta := terminalOutputDelta(meta); delta != "" {
		tool.deltas = true
		stream(h.log, "Tool output", string(id), delta)
	} else if !tool.deltas {
		text := toolText(content, rawOutput)
		if tool.length > 0 && len(text) >= tool.length && sha256.Sum256([]byte(text[:tool.length])) == tool.digest {
			stream(h.log, "Tool output", string(id), text[tool.length:])
		} else {
			stream(h.log, "Tool output", string(id), text)
		}
		if text != "" {
			tool.length = len(text)
			tool.digest = sha256.Sum256([]byte(text))
		}
	}
	switch {
	case status == nil:
	case *status == schema.ToolCallStatusCompleted:
		h.log.Info("Tool completed", "title", tool.title)
	case *status == schema.ToolCallStatusFailed:
		h.log.Error("Tool failed", "title", tool.title)
	}
}

func terminalOutputDelta(meta schema.Meta) string {
	value, ok := meta["terminal_output_delta"]
	if !ok {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	var delta struct {
		Data string `json:"data"`
	}
	if json.Unmarshal(encoded, &delta) != nil {
		return ""
	}
	return delta.Data
}

func toolText(content []schema.ToolCallContent, raw json.RawMessage) string {
	var text strings.Builder
	for _, block := range content {
		switch {
		case block.Content != nil && block.Content.Content.Text != nil:
			text.WriteString(block.Content.Content.Text.Text)
		case block.Diff != nil:
			text.WriteString("File: " + block.Diff.Path + "\n" + block.Diff.NewText + "\n")
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
