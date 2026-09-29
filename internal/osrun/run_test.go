package osrun

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTailIsBoundedAndUTF8(t *testing.T) {
	tail := &Tail{Capacity: 4}
	_, _ = tail.Write([]byte("prefix€€"))
	text, truncated := tail.Text()
	if !truncated || !utf8.ValidString(text) || text != "€" {
		t.Fatalf("bad tail: %q %v", text, truncated)
	}
	empty := &Tail{Capacity: 0}
	_, _ = empty.Write([]byte("drop"))
	text, truncated = empty.Text()
	if text != "" || !truncated {
		t.Fatal("zero output limit ignored")
	}
}
func TestProcessTreeCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	args := []string{"sh", "-c", "sleep 30 & wait"}
	if runtime.GOOS == "windows" {
		args = []string{"cmd", "/c", "ping -n 30 127.0.0.1 | findstr x"}
	}
	if _, err := Run(ctx, "", nil, args...); err == nil {
		t.Fatal("cancelled shell succeeded")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("descendant held output pipes open")
	}
}
func TestKillAfterWaitLeavesPIDAlone(t *testing.T) {
	// Once reaped, the PID may belong to another process, so Kill must not use it.
	cmd := StartCommand(context.Background(), "", []string{os.Args[0], "-test.run=^$"}, nil)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := Kill(cmd); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Kill after Wait = %v, want os.ErrProcessDone", err)
	}
}
