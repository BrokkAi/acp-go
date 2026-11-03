package schema

import (
	"encoding/json"
	"testing"

	"github.com/BrokkAi/acp-go/internal/serdewire"
)

// metaBenchUpdates are one session/update notification without _meta, with
// a small _meta, and with a larger nested one.
var metaBenchUpdates = []struct{ name, frame string }{
	{"none", `{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hello, world. This is a streamed chunk of text."},"messageId":"m"}}`},
	{"small", `{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hello, world. This is a streamed chunk of text."},"messageId":"m"},"_meta":{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}}`},
	{"rich", `{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hello, world. This is a streamed chunk of text.","_meta":{"zed.dev/chunk":17}},"messageId":"m","_meta":{"zed.dev/model":"m-1","zed.dev/usage":{"input":1234,"output":56,"cost":0.0123}}},"_meta":{"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01","tracestate":"congo=t61rcWkgMzE","acme.dev/ids":[1,2,3,4,5],"acme.dev/flags":{"b":true,"a":false,"n":null},"acme.dev/label":"agent \"main\""}}`},
}

// BenchmarkSessionUpdateMeta decodes a session/update notification, and
// encodes the decoded value with json.Marshal and as the connection writes
// it (serdewire.MarshalTyped), with and without _meta.
func BenchmarkSessionUpdateMeta(b *testing.B) {
	for _, u := range metaBenchUpdates {
		data := []byte(u.frame)
		b.Run("decode/"+u.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				var n SessionNotification
				if err := json.Unmarshal(data, &n); err != nil {
					b.Fatal(err)
				}
			}
		})
		var n SessionNotification
		if err := json.Unmarshal(data, &n); err != nil {
			b.Fatal(err)
		}
		b.Run("marshal/"+u.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := json.Marshal(&n); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run("frame/"+u.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := serdewire.MarshalTyped(&n); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
