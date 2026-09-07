package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func TestWorkspaceCannotEscapeAndCancelledPermission(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(outside, "private")
	writeTestFile(t, path, "secret")
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	h, err := newHost(context.Background(), dir, io.Discard, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	h.session = "s"
	for _, path := range []string{path, filepath.Join(dir, "link", "private"), "relative"} {
		params, _ := json.Marshal(map[string]string{"sessionId": "s", "path": path, "content": "overwrite"})
		for _, method := range []string{"fs/read_text_file", "fs/write_text_file"} {
			if _, err := h.request(context.Background(), method, params); err == nil {
				t.Fatalf("escaped root using %s %s", method, path)
			}
		}
	}
	h.cancel()
	params := json.RawMessage(`{"sessionId":"s","options":[{"kind":"allow_once","optionId":"yes"}]}`)
	result, err := h.request(context.Background(), "session/request_permission", params)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(result)
	if !strings.Contains(string(b), "cancelled") {
		t.Fatal("permission approved after cancellation")
	}
}

func TestPermissionsRequireExplicitOptIn(t *testing.T) {
	h, err := newHost(context.Background(), t.TempDir(), io.Discard, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	h.session = "s"
	raw := json.RawMessage(`{"sessionId":"s","options":[{"kind":"allow_once","optionId":"yes"}]}`)
	for _, allow := range []bool{false, true} {
		h.autoApprove = allow
		result, err := h.request(context.Background(), "session/request_permission", raw)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(result)
		if strings.Contains(string(data), "selected") != allow {
			t.Fatalf("permission opt-in ignored: %s", data)
		}
	}
}
