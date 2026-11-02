package agent

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	schema "github.com/BrokkAi/acp-go/schema/v2"
)

// The draft-v2 runtime serves protocol v2, which forbids null request ids:
// such a request gets the reference SDK's -32600 with id null, before the
// initialization gate, and the connection keeps serving.
func TestRuntimeRejectsNullRequestIDs(t *testing.T) {
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- New(&echoAgent{cancelled: make(chan schema.CancelSessionNotification, 1)}).Serve(context.Background(), peer, peer)
	}()
	t.Cleanup(func() {
		_ = local.Close()
		select {
		case <-serveDone:
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	reader := bufio.NewReader(local)
	for _, want := range []string{
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"ACP protocol v2 request ids must be a string or an integer, not null"}}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"ACP protocol v2 request ids must be a string or an integer, not null"}}`,
	} {
		if _, err := io.WriteString(local, `{"jsonrpc":"2.0","id":null,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"c","version":"1"}}}`+"\n"); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(line); got != want {
			t.Fatalf("answer = %s, want %s", got, want)
		}
	}
}
