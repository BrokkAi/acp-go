package acp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The frame encoding benchmarks compare encodeFrame with json.Marshal, which
// the connection used to encode frames, on frames already in serde_json's
// form, frames it rewrites, and an error frame, which is marshaled first.
func BenchmarkEncodeFrame(b *testing.B) {
	text := strings.Repeat("Some agent output, with \"quotes\" and a\nnewline. ", 1400)
	update := func(text string) packet {
		params, _ := json.Marshal(map[string]any{
			"sessionId": "sess-1",
			"update": map[string]any{
				"sessionUpdate": "agent_message_chunk",
				"content":       map[string]any{"type": "text", "text": text},
			},
		})
		return packet{Version: "2.0", Method: "session/update", Params: params}
	}
	values := make([]float64, 10000)
	for i := range values {
		values[i] = float64(i) + 0.25
	}
	floats, _ := json.Marshal(values)
	for i := range values {
		values[i] = float64(i) / 3
	}
	thirds, _ := json.Marshal(values)
	variables := make(map[string]string, 100)
	for i := 0; i < 100; i++ {
		variables[fmt.Sprintf("VARIABLE_%03d", i)] = fmt.Sprintf("value-%d", i)
	}
	env, _ := json.Marshal(map[string]any{"env": variables})
	frames := []struct {
		name  string
		value any
	}{
		{"small", update("Hello.")},
		{"small-response", packet{Version: "2.0", ID: json.RawMessage(`12`), Result: json.RawMessage(`{"stopReason":"end_turn"}`)}},
		{"small-html", update("a < b & c")},
		{"small-error", packet{Version: "2.0", ID: json.RawMessage(`12`), Error: &RPCError{Code: -32602, Message: "Invalid params", Data: json.RawMessage(`"missing field` + "`sessionId`" + `"`)}}},
		{"large-64KiB", update(text)},
		{"large-64KiB-html", update(text + "<&>")},
		{"floats-10k", packet{Version: "2.0", Method: "x/floats", Params: floats}},
		{"floats-10k-17-digits", packet{Version: "2.0", Method: "x/floats", Params: thirds}},
		{"env-100", packet{Version: "2.0", Method: "x/env", Params: env}},
	}
	for _, frame := range frames {
		b.Run(frame.name+"/json.Marshal", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := json.Marshal(frame.value); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(frame.name+"/encodeFrame", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := encodeFrame(frame.value); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
