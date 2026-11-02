package acp

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/internal/rpcctl"
)

// TestDeadlineHonoredWhenOutputStalled: when the peer stops reading, a Notify
// or Call with a deadline still returns, with or without the internal guards
// that order frames.
func TestDeadlineHonoredWhenOutputStalled(t *testing.T) {
	guard := rpcctl.WithOutgoingGuard(func(string, json.RawMessage, bool) (json.RawMessage, error) { return nil, nil }).(ConnectOption)
	for name, options := range map[string][]ConnectOption{"unguarded": nil, "guarded": {guard}} {
		t.Run(name, func(t *testing.T) {
			inR, inW := io.Pipe()
			_, outW := io.Pipe()
			defer inW.Close()
			c := Connect(inR, outW, func(context.Context, string, json.RawMessage) (any, error) { return nil, nil }, nil, options...)
			defer c.Close()
			// Fill the writer (one frame blocked in Write) and the queue.
			for range 40 {
				go func() { _ = c.Notify(context.Background(), "x", map[string]int{"i": 1}) }()
			}
			time.Sleep(200 * time.Millisecond)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- c.Notify(ctx, "y", nil) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Notify with a 100ms deadline still blocked after 2s")
			}
			go func() { done <- c.Call(ctx, "z", nil, nil) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Call with an expired deadline still blocked after 2s")
			}
		})
	}
}
