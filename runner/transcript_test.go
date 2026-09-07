package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestTranscriptStreamsMessagesAndDeduplicatesToolSnapshots(t *testing.T) {
	var output, saved bytes.Buffer
	h, err := newHost(context.Background(), t.TempDir(), &saved, slog.New(slog.NewJSONHandler(&output, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	h.session = "s"
	updates := []string{
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Checking "}}`,
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"the build.\n"}}`,
		`{"sessionUpdate":"agent_thought_chunk","content":{"type":"text","text":"Inspecting configuration.\n"}}`,
		`{"sessionUpdate":"tool_call","toolCallId":"build","title":"go test ./...","status":"in_progress"}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"build","rawOutput":{"formatted_output":"compiling\n"}}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"build","rawOutput":{"formatted_output":"compiling\nfailed: missing credential\n"},"status":"failed"}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"build","rawOutput":{"formatted_output":"compiling\nfailed: missing credential\n"}}`,
		`{"sessionUpdate":"tool_call","toolCallId":"streamed","title":"probe"}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"streamed","_meta":{"terminal_output_delta":{"data":"live probe output\n"}}}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"streamed","rawOutput":{"formatted_output":"live probe output\n"},"status":"completed"}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"read","content":[{"type":"content","content":{"type":"text","text":"file contents\n"}}]}`,
	}
	for _, update := range updates {
		if err := h.notification("session/update", json.RawMessage(`{"sessionId":"s","update":`+update+`}`)); err != nil {
			t.Fatal(err)
		}
	}
	var messages, thoughts, tools strings.Builder
	failed := false
	decoder := json.NewDecoder(&output)
	for {
		var entry map[string]any
		if err := decoder.Decode(&entry); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if entry["msg"] == "Tool failed" {
			failed = true
		}
		text, _ := entry["text"].(string)
		switch entry["source"] {
		case "Agent":
			messages.WriteString(text)
		case "Thinking":
			thoughts.WriteString(text)
		case "Tool output":
			tools.WriteString(text)
		}
	}
	if messages.String() != "Checking the build.\n" || thoughts.String() != "Inspecting configuration.\n" {
		t.Fatal("agent text did not stream")
	}
	if tools.String() != "compiling\nfailed: missing credential\nlive probe output\nfile contents\n" || !failed {
		t.Fatalf("tool output lost or duplicated: %q, failed=%v", tools.String(), failed)
	}
	if bytes.Count(saved.Bytes(), []byte(`"sessionId":"s"`)) != len(updates) {
		t.Fatal("console output replaced the durable full transcript")
	}
	answer, _ := h.answer.Text()
	if answer != messages.String() {
		t.Fatal("thoughts or tool output contaminated the release receipt")
	}
}

func TestTranscriptWriterPreservesSplitUTF8(t *testing.T) {
	var output bytes.Buffer
	w := &transcriptWriter{log: slog.New(slog.NewJSONHandler(&output, nil)), source: "Agent stderr"}
	for _, b := range []byte("error: café\n") {
		_, _ = w.Write([]byte{b})
	}
	var text strings.Builder
	d := json.NewDecoder(&output)
	for {
		var entry struct{ Text string }
		if err := d.Decode(&entry); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		text.WriteString(entry.Text)
	}
	if text.String() != "error: café\n" {
		t.Fatalf("damaged UTF-8 output: %q", text.String())
	}
}
