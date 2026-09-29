package clienthost

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/schema"
)

func TestTerminalSymlinkWorkspace(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "real")
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := symlink(t, real, workspace); err != nil {
		t.Fatal(err)
	}
	if err := symlink(t, base, filepath.Join(real, "escape")); err != nil {
		t.Fatal(err)
	}
	h, err := Open(context.Background(), Config{Directory: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.SetSession("s")
	for _, cwd := range []string{"", workspace, filepath.Join(workspace, "sub"), real, filepath.Join(workspace, "escape")} {
		t.Run(cwd, func(t *testing.T) {
			request := schema.CreateTerminalRequest{SessionID: "s", Command: "pwd"}
			if cwd != "" {
				request.Cwd = &cwd
			}
			result, err := h.createTerminal(request)
			if cwd == filepath.Join(workspace, "escape") {
				if err == nil {
					t.Fatal("accepted escaping cwd")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			id := result.(schema.CreateTerminalResponse).TerminalID
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, err := h.waitForTerminal(ctx, schema.WaitForTerminalExitRequest{SessionID: "s", TerminalID: id}); err != nil {
				t.Fatal(err)
			}
			output, err := h.terminalOutput(schema.TerminalOutputRequest{SessionID: "s", TerminalID: id})
			if err != nil {
				t.Fatal(err)
			}
			expected := real
			if cwd == filepath.Join(workspace, "sub") {
				expected = filepath.Join(real, "sub")
			}
			if got := strings.TrimSpace(output.(schema.TerminalOutputResponse).Output); got != expected {
				t.Fatalf("cwd = %q, want %q", got, expected)
			}
		})
	}
	// File callbacks must still accept the configured spelling of the root.
	path := filepath.Join(workspace, "file.txt")
	if _, err := h.writeFile(schema.WriteTextFileRequest{Path: path, Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.readFile(schema.ReadTextFileRequest{Path: path}); err != nil {
		t.Fatal(err)
	}
}
