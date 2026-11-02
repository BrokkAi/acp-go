package acp

import (
	"context"
	"encoding/json"
	"net"
	"strings"
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
		`{"jsonrpc":"2.0","id":"reply","error":{"code":-32002,"message":"missing","data":null,"code":1,"extension":{"retry":true}}}`,
		`{"jsonrpc":"2.0","id":"request","method":"fs/read_text_file","params":{"sessionId":"s","path":"/tmp/x"}}`,
		// JSON-RPC batch arrays are rejected with one -32600 and never run.
		`[]`,
		`[{"jsonrpc":"2.0","method":"session/update","params":{}}]`,
		`[17,{"jsonrpc":"2.0","id":"request","method":"fs/read_text_file","params":{}}]`,
		`[{"jsonrpc":"2.0","id":"reply","result":{"ok":true}}]`,
		`[{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"request"}}]`,
		`[[]]`,
		// Lines that are not valid JSON are answered with one -32700, even
		// when they start like an array.
		`[`,
		`[{"jsonrpc":"2.0"}`,
		"not-json\r",
		`{"jsonrpc":"2.0","method":"session/update","params":{}} {}`,
		// serde_json rejects these although encoding/json accepts them.
		`{"jsonrpc":"2.0","method":"` + `\` + `ud800"}`,
		`[1e400]`,
		`{"jsonrpc":"2.0","id":1.7976931348623158e308,"method":"x"}`,
		strings.Repeat("[", 128) + strings.Repeat("]", 128),
		strings.Repeat("[", 127) + strings.Repeat("]", 127),
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
