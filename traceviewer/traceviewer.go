/*
Package traceviewer renders ACP trace events as an ordered sequence diagram in a
browser. It is an explicit developer tool, not a transport: the SDK keeps stdio
as its default, and this package only reads JSONL trace files (the ones
`clienthost` writes) or events pushed in memory.

The viewer serves two routes over ordinary `net/http`:

  - `GET /` returns an embedded page that polls the feed and renders each event
    as one ordered step.
  - `GET /events` returns the current events as JSON, re-read from the source on
    every request, so a file being written by a live session updates in place.

No external assets or modules are used; the page is embedded in the binary.
*/
package traceviewer

import (
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed viewer.html
var assets embed.FS

// maxEvents bounds one read so a corrupted trace cannot exhaust memory.
const maxEvents = 100_000

// Source supplies the current trace events. Events returns whole JSONL records;
// implementations must be safe for concurrent use.
type Source interface {
	Events() ([]json.RawMessage, error)
}

// FileSource re-reads one JSONL trace file on every request, which picks up a
// live session's appended records.
type FileSource struct {
	Path string
}

func (s FileSource) Events() ([]json.RawMessage, error) {
	file, err := os.Open(s.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readEvents(file)
}

// Memory is a source for events pushed by the host, mirroring the reference
// trace viewer's in-memory mode.
type Memory struct {
	mu     sync.Mutex
	events []json.RawMessage
}

// NewMemory returns an empty in-memory source.
func NewMemory() *Memory { return &Memory{} }

// Push appends one event, encoding plain values as JSON.
func (m *Memory) Push(value any) error {
	var raw json.RawMessage
	switch typed := value.(type) {
	case json.RawMessage:
		raw = append(json.RawMessage(nil), typed...)
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		raw = encoded
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, raw)
	return nil
}

func (m *Memory) Events() ([]json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]json.RawMessage(nil), m.events...), nil
}

// Viewer serves the embedded page and the event feed for one source.
type Viewer struct {
	Source Source
}

// Event is one step in the rendered sequence.
type Event struct {
	Index     int             `json:"index"`
	Label     string          `json:"label"`
	Direction string          `json:"direction,omitempty"`
	Raw       json.RawMessage `json:"raw"`
}

func (v *Viewer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "":
		v.servePage(w)
	case "/events":
		v.serveEvents(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (v *Viewer) servePage(w http.ResponseWriter) {
	page, err := assets.ReadFile("viewer.html")
	if err != nil {
		http.Error(w, "ACP trace viewer page is unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func (v *Viewer) serveEvents(w http.ResponseWriter, r *http.Request) {
	if v.Source == nil {
		http.Error(w, "ACP trace viewer has no event source", http.StatusInternalServerError)
		return
	}
	raw, err := v.Source.Events()
	if err != nil {
		http.Error(w, "read ACP trace events: "+err.Error(), http.StatusInternalServerError)
		return
	}
	events := make([]Event, 0, len(raw))
	for i, record := range raw {
		label, direction := describe(record)
		events = append(events, Event{Index: i + 1, Label: label, Direction: direction, Raw: record})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": events})
}

// Run serves the viewer until the context is cancelled.
func (v *Viewer) Run(ctx context.Context, address string) error {
	if strings.TrimSpace(address) == "" {
		return errors.New("ACP trace viewer address is required")
	}
	server := &http.Server{Addr: address, Handler: v, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return ctx.Err()
	}
}

// Open launches the platform browser for url. It is best-effort: a missing
// browser opener returns the underlying error.
func Open(url string) error {
	var command string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
	case "windows":
		command = "rundll32"
	default:
		command = "xdg-open"
	}
	args := []string{url}
	if command == "rundll32" {
		args = []string{"url.dll,FileProtocolHandler", url}
	}
	return exec.Command(command, args...).Start()
}

// describe derives a readable step label and direction from one record. Raw
// JSON-RPC frames use their method; transcript records use their event name.
func describe(record json.RawMessage) (string, string) {
	var parsed struct {
		Method    string `json:"method"`
		Event     string `json:"event"`
		Direction string `json:"direction"`
		ID        any    `json:"id"`
	}
	if json.Unmarshal(record, &parsed) != nil {
		return "frame", ""
	}
	label := parsed.Method
	if label == "" {
		label = parsed.Event
	}
	if label == "" {
		label = "frame"
	}
	return label, parsed.Direction
}

func readEvents(reader io.Reader) ([]json.RawMessage, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	events := []json.RawMessage{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if len(events) >= maxEvents {
			return nil, fmt.Errorf("ACP trace exceeds %d events", maxEvents)
		}
		if !json.Valid([]byte(line)) {
			return nil, fmt.Errorf("ACP trace line %d is not valid JSON", len(events)+1)
		}
		events = append(events, json.RawMessage(line))
	}
	return events, scanner.Err()
}
