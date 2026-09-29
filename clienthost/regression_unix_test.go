//go:build unix

package clienthost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/schema"
)

func TestReadFIFOIsRejectedWithoutBlocking(t *testing.T) {
	for _, heldOpen := range []bool{false, true} {
		name := "no-writer"
		if heldOpen {
			name = "held-open"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "input.fifo")
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			if heldOpen {
				f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
			}
			h, err := Open(context.Background(), Config{Directory: directory})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			h.SetSession("s")
			raw, _ := json.Marshal(schema.ReadTextFileRequest{SessionID: "s", Path: path})
			done := make(chan error, 1)
			go func() { _, err := h.Request(context.Background(), schema.FsReadTextFileMethodName, raw); done <- err }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "regular file") {
					t.Fatalf("expected regular-file error, got %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("FIFO read blocked")
			}
		})
	}
}
