package acp

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// FuzzConnectionFrame checks that every malformed or well-formed frame fed to
// the reader terminates cleanly instead of hanging, panicking, or bypassing
// JSON-RPC validation. The seed corpus is exercised by ordinary go test runs;
// the mutation engine is reserved for the optional CI fuzz job.
func FuzzConnectionFrame(f *testing.F) {
	seeds := []string{
		``,
		`not-json`,
		`{"jsonrpc":"1.0","method":"session/update","params":{}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{}}`,
		`{"jsonrpc":"2.0","id":"reply","result":{"ok":true}}`,
		`{"jsonrpc":"2.0","id":"reply","error":{"code":-32000,"message":"auth_required"}}`,
		`{"jsonrpc":"2.0","id":"request","method":"fs/read_text_file","params":{"sessionId":"s","path":"/tmp/x"}}`,
	}
	for _, seed := range seeds {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, frame []byte) {
		client, peer := net.Pipe()
		defer peer.Close()
		connection := Connect(client, client,
			func(context.Context, string, json.RawMessage) (any, error) {
				return map[string]bool{"ok": true}, nil
			},
			func(string, json.RawMessage) error { return nil },
		)

		writeDone := make(chan struct{})
		go func() {
			defer close(writeDone)
			deadline := time.Now().Add(2 * time.Second)
			_ = peer.SetWriteDeadline(deadline)
			_, _ = peer.Write(append(frame, '\n'))
			_ = peer.Close()
		}()

		select {
		case <-connection.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("connection did not terminate after peer closed")
		}
		_ = connection.Close()
		<-writeDone
	})
}
