package acp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// largeFrameData is longer than the 16 MiB HTTP POST body limit, the only
// size limit the reference SDK has, so a frame carrying it would fail any
// smaller line limit.
var largeFrameData = strings.Repeat("x", 17<<20)

func TestFrameOfAnySizeRoundTrips(t *testing.T) {
	p := newShutdownPeer(t, func(_ context.Context, _ string, params json.RawMessage) (any, error) {
		return params, nil
	})
	sent := make(chan error, 1)
	go func() {
		_, err := io.WriteString(p.input, `{"jsonrpc":"2.0","id":1,"method":"test/echo","params":{"data":"`+largeFrameData+`"}}`+"\r\n")
		if err == nil {
			// A frame after the large one is still read.
			_, err = io.WriteString(p.input, `{"jsonrpc":"2.0","id":2,"method":"test/echo","params":{"data":"small"}}`+"\n")
		}
		sent <- err
	}()
	got := map[int]string{}
	for range 2 {
		var response struct {
			ID     int
			Result struct{ Data string }
		}
		if err := json.Unmarshal([]byte(p.readLine()), &response); err != nil {
			t.Fatal(err)
		}
		got[response.ID] = response.Result.Data
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if got[1] != largeFrameData || got[2] != "small" {
		t.Fatalf("echoed data lengths %d and %q", len(got[1]), got[2])
	}
}

func TestShutdownDiscardsLargeFrame(t *testing.T) {
	p := newShutdownPeer(t, nil)
	p.queue(2)
	done := make(chan error, 1)
	go func() { done <- p.connection.Shutdown(context.Background()) }()
	waitUntil(t, "shutdown to begin", p.connection.shuttingDown.Load)
	// The drain cannot finish before the peer reads the queued frames, so
	// the large frame is read during it.
	if _, err := io.WriteString(p.input, `{"jsonrpc":"2.0","method":"test/n","params":"`+largeFrameData+`"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(p.readAll(), "\n"), strings.Join(notificationLines(2), "\n"); got != want {
		t.Fatalf("peer received:\n%s\nwant:\n%s", got, want)
	}
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
