package osrun

import (
	"context"
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
	if _, err := Run(ctx, "", nil, "sh", "-c", "sleep 30 & wait"); err == nil {
		t.Fatal("cancelled shell succeeded")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("descendant held output pipes open")
	}
}
