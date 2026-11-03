package proxyrouter

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/acptest"
)

// largeFrameData is longer than the 8 MiB line limit acp-go once had; the
// reference SDK has none. The stdio connection test covers a frame over 16 MiB.
var largeFrameData = strings.Repeat("x", 9<<20)

// TestProxyRouterReadsFramesOfAnySize sends a large frame the router rejects,
// a large proxy/initialize it routes, and a large frame after it. The proxy
// echoes every line it reads, the replayed first frame included.
func TestProxyRouterReadsFramesOfAnySize(t *testing.T) {
	echo := acptest.ProxyFunc(func(_ context.Context, in io.ReadCloser, out io.WriteCloser) error {
		defer in.Close()
		reader := bufio.NewReader(in)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return err
			}
			if _, err := out.Write(line); err != nil {
				return err
			}
		}
	})
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Minute)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() { done <- New().WithV2(echo).Serve(context.Background(), local, local) }()
	t.Cleanup(func() { _ = peer.Close(); <-done })
	rw := bufio.NewReadWriter(bufio.NewReader(peer), bufio.NewWriter(peer))

	meta := `"_meta":{"blob":"` + largeFrameData + `"}`
	rejected := exchange(t, rw, `{"jsonrpc":"2.0","id":1,"method":"proxy/initialize","params":{"protocolVersion":1,`+meta+`}}`)
	if want := `{"jsonrpc":"2.0","id":1,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":1,"supported":[2]}}}`; rejected != want {
		t.Fatalf("rejection = %.200s, want %s", rejected, want)
	}
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":2,"method":"proxy/initialize","params":{"protocolVersion":2,"info":{"name":"c","version":"1"},` + meta + `}}`,
		`{"jsonrpc":"2.0","method":"test/n","params":{` + meta + `}}`,
	} {
		if got := exchange(t, rw, frame); got != frame {
			t.Fatalf("proxy echoed %d bytes, want the %d byte frame", len(got), len(frame))
		}
	}
}
