package traceviewer_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go/traceviewer"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type feed struct {
	Events []traceviewer.Event `json:"events"`
}

func fetchFeed(t *testing.T, server *httptest.Server) feed {
	t.Helper()
	response, err := http.Get(server.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var payload feed
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// TestFileSourceFollowsLiveTranscript proves the viewer re-reads the transcript
// on every poll, so a session that is still writing shows new steps.
func TestFileSourceLiveUpdate(t *testing.T) {
	path := writeTranscript(t,
		`{"method":"initialize","id":1}`,
		`{"event":"session_end","phase":"session/prompt"}`,
	)
	server := httptest.NewServer(&traceviewer.Viewer{Source: traceviewer.FileSource{Path: path}})
	defer server.Close()

	page, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	buffer := make([]byte, 4096)
	n, _ := page.Body.Read(buffer)
	if !strings.Contains(string(buffer[:n]), "ACP trace") {
		t.Fatalf("viewer page = %s", buffer[:n])
	}

	first := fetchFeed(t, server)
	if len(first.Events) != 2 {
		t.Fatalf("events = %d", len(first.Events))
	}
	if first.Events[0].Label != "initialize" || first.Events[1].Label != "session_end" {
		t.Fatalf("labels = %q, %q", first.Events[0].Label, first.Events[1].Label)
	}
	if first.Events[0].Index != 1 || first.Events[1].Index != 2 {
		t.Fatalf("ordering = %d, %d", first.Events[0].Index, first.Events[1].Index)
	}

	appendLine(t, path, `{"method":"session/prompt","id":2}`)
	second := fetchFeed(t, server)
	if len(second.Events) != 3 || second.Events[2].Label != "session/prompt" {
		t.Fatalf("live update = %+v", second.Events)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func TestMemorySourceAndMalformedTranscript(t *testing.T) {
	memory := traceviewer.NewMemory()
	if err := memory.Push(map[string]any{"method": "initialize", "direction": "client → agent"}); err != nil {
		t.Fatal(err)
	}
	if err := memory.Push(json.RawMessage(`{"method":"session/prompt"}`)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(&traceviewer.Viewer{Source: memory})
	defer server.Close()
	events := fetchFeed(t, server).Events
	if len(events) != 2 || events[0].Direction != "client → agent" || events[1].Label != "session/prompt" {
		t.Fatalf("memory feed = %+v", events)
	}

	broken := writeTranscript(t, `{"method":"initialize"}`, `not json`)
	if _, err := (traceviewer.FileSource{Path: broken}).Events(); err == nil {
		t.Fatal("malformed transcript was accepted")
	}
}

func TestViewerRejectsUnknownRoutesAndMissingSource(t *testing.T) {
	server := httptest.NewServer(&traceviewer.Viewer{Source: traceviewer.NewMemory()})
	defer server.Close()
	response, err := http.Get(server.URL + "/missing")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", response.StatusCode)
	}

	broken := httptest.NewServer(&traceviewer.Viewer{})
	defer broken.Close()
	brokenResponse, err := http.Get(broken.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer brokenResponse.Body.Close()
	if brokenResponse.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d", brokenResponse.StatusCode)
	}
}
