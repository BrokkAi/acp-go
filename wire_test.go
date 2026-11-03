package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/internal/rpcctl"
	"github.com/BrokkAi/acp-go/internal/serdejson"
	"github.com/BrokkAi/acp-go/internal/serdewire"
)

// TestFramesMatchTheReferenceSerialization pins the bytes of a notification
// whose params are forwarded raw JSON with every case serde_json rewrites:
// HTML characters and their escapes, U+2028 and U+2029 raw and escaped,
// other escapes (\u0061, \/, upper-case hex, a surrogate pair, \u0008 for
// \b), serde_json's control-character escapes, floats, integers past i64
// and u64, -0, and repeated members, including two spellings of one key.
// testdata/serdeframe/rust_frame.jsonl is what the reference SDK 5.0.0's
// proxy writes when it forwards the same params.
func TestFramesMatchTheReferenceSerialization(t *testing.T) {
	params, err := os.ReadFile("testdata/serdeframe/params.json")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/serdeframe/rust_frame.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	c, peer := pipeClient(t, nil, nil)
	got := make(chan []byte, 1)
	go func() {
		line, _ := readLine(peer)
		got <- line
	}()
	if err := c.Notify(t.Context(), "x/note", json.RawMessage(params)); err != nil {
		t.Fatal(err)
	}
	if line := <-got; !bytes.Equal(line, want) {
		t.Fatalf("frame\n%s\nwant\n%s", line, want)
	}
}

func readLine(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var line []byte
	buf := make([]byte, 1)
	for {
		if _, err := r.Read(buf); err != nil {
			return line, err
		}
		line = append(line, buf[0])
		if buf[0] == '\n' {
			return line, nil
		}
	}
}

// TestEncodeFrameRewritesNestedMarshalers checks that values encoding/json
// writes through a MarshalJSON method, such as RPCError and its data, are
// rewritten too.
func TestEncodeFrameRewritesNestedMarshalers(t *testing.T) {
	data, _ := json.Marshal(map[string]any{"why": "<a & b>\u2028", "n": 1.5})
	frame := packet{Version: "2.0", ID: json.RawMessage(`"<id>"`), Error: &RPCError{Code: -32000, Message: "x < y & z", Data: data}}
	got, err := encodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"jsonrpc\":\"2.0\",\"id\":\"<id>\",\"error\":{\"code\":-32000,\"message\":\"x < y & z\",\"data\":{\"n\":1.5,\"why\":\"<a & b>\u2028\"}}}"
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

// TestEncodeFrameMatchesTheSlowPath checks that the result never depends on
// which path a frame takes.
func TestEncodeFrameMatchesTheSlowPath(t *testing.T) {
	for _, value := range []any{
		map[string]any{"a": "plain", "n": 5, "list": []int{1, 2}},
		map[string]any{"a": "<&>", "f": 0.5, "big": uint64(1) << 63},
		packet{Version: "2.0", ID: json.RawMessage(`7`), Method: "m", Params: json.RawMessage(`{"a":1,"a":2,"s":"\u0061"}`)},
		"line\nbreak \"quoted\" \\ \u2029",
	} {
		got, err := encodeFrame(value)
		if err != nil {
			t.Fatal(err)
		}
		marshaled, _ := json.Marshal(value)
		parsed, err := serdejson.ParseAnyDepth(marshaled)
		if err != nil {
			t.Fatal(err)
		}
		if want := serdejson.Encode(parsed); !bytes.Equal(got, want) {
			t.Errorf("encodeFrame(%v) = %s, want %s", value, got, want)
		}
	}
}

// reference is the frame serde_json writes for p, built from scratch: the
// packet marshaled by encoding/json, parsed and re-encoded by serdejson,
// with the empty method a request or notification carries, which
// json.Marshal's omitempty drops. It reports false for a packet the
// reference SDK cannot hold.
func reference(t testing.TB, p packet) ([]byte, bool) {
	marshaled, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal %+v: %v", p, err)
	}
	value, err := serdejson.ParseAnyDepth(marshaled)
	if err != nil {
		return nil, false
	}
	if p.Method == "" && p.Result == nil && p.Error == nil {
		method := &serdejson.Value{Kind: serdejson.String}
		var members []serdejson.Member
		for _, member := range value.Members {
			members = append(members, member)
			if member.Key == "id" || (member.Key == "jsonrpc" && len(p.ID) == 0) {
				members = append(members, serdejson.Member{Key: "method", Value: method})
			}
		}
		value.Members = members
	}
	return serdejson.Encode(value), true
}

func checkPacket(t testing.TB, p packet) {
	t.Helper()
	want, ok := reference(t, p)
	got, err := encodeFrame(p)
	if !ok {
		if !errors.Is(err, serdewire.ErrUnrepresentable) {
			t.Fatalf("encodeFrame(%+v) = %s, %v; want ErrUnrepresentable", p, got, err)
		}
		return
	}
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("encodeFrame(%+v) = %s, %v; want %s", p, got, err, want)
	}
}

// TestAppendPacketMatchesTheReference checks packet frames against the
// general path, the definition of serde_json's form.
func TestAppendPacketMatchesTheReference(t *testing.T) {
	for _, p := range []packet{
		{Version: "2.0", Method: "session/update", Params: json.RawMessage(`{"a":"<&> ","n":1.5,"a":2}`)},
		{Version: "2.0", ID: json.RawMessage(`7`), Method: "x/ask"},
		{Version: "2.0", ID: json.RawMessage(`"<id>"`), Result: json.RawMessage(`null`)},
		{Version: "2.0", ID: json.RawMessage(`"b"`), Result: json.RawMessage(`{"stopReason":"end_turn","x":[1e2,"\/"]}`)},
		{Version: "2.0", Method: "m< \"\\\x01\xff", Params: json.RawMessage(`[]`)},
		{Version: "2.0", ID: json.RawMessage(`null`), Method: "m", Params: json.RawMessage(`{}`)},
		{Version: "2.0", ID: json.RawMessage(`4`), Error: &RPCError{Code: -32602, Message: "a < b", Data: json.RawMessage(` { "x" : [ 1.0 , "&" ] , "x" : 2 } `)}},
		{Version: "2.0", ID: json.RawMessage(`5`), Error: &RPCError{Code: -32000, Message: "m", Extra: map[string]json.RawMessage{"z": json.RawMessage(`"\/"`), "a": json.RawMessage(` 1 `)}}},
		// The reference SDK writes an empty method rather than omitting it.
		{Version: "2.0", ID: json.RawMessage(`6`), Method: ""},
		{Version: "2.0", Method: "", Params: json.RawMessage(`{}`)},
		// JSON the reference SDK cannot hold is refused.
		{Version: "2.0", Method: "m", Params: json.RawMessage("[\"\xff\"]")},
		{Version: "2.0", Method: "m", Params: json.RawMessage(`["\ud800"]`)},
		{Version: "2.0", ID: json.RawMessage(`8`), Result: json.RawMessage(`[1e400]`)},
	} {
		checkPacket(t, p)
	}
	got, err := encodeFrame(packet{Version: "2.0", ID: json.RawMessage(`6`)})
	if err != nil || string(got) != `{"jsonrpc":"2.0","id":6,"method":""}` {
		t.Fatalf("empty method frame = %s, %v", got, err)
	}
}

// FuzzAppendPacket checks packet frames against the general path for any
// method and compact params.
func FuzzAppendPacket(f *testing.F) {
	f.Add("session/update", []byte(`{"a":"<"}`), true)
	f.Add("m ", []byte(`[1.0,"a"]`), false)
	f.Add("", []byte(`{"a":1,"a":2}`), true)
	f.Fuzz(func(t *testing.T, method string, params []byte, request bool) {
		var compact bytes.Buffer
		if json.Compact(&compact, params) != nil {
			return
		}
		p := packet{Version: "2.0", Method: method, Params: compact.Bytes()}
		if request {
			p.ID = json.RawMessage(`3`)
		}
		if _, err := json.Marshal(p); err != nil {
			return
		}
		checkPacket(t, p)
	})
}

// FuzzAppendErrorPacket checks error frames against the general path for any
// message and error data.
func FuzzAppendErrorPacket(f *testing.F) {
	f.Add("Invalid params", []byte(`{"a":"<"}`))
	f.Add("m<", []byte(` [ 1.0 , "\/" ] `))
	f.Fuzz(func(t *testing.T, message string, data []byte) {
		if !json.Valid(data) {
			data = nil
		}
		p := packet{Version: "2.0", ID: json.RawMessage(`9`), Error: &RPCError{Code: -32000, Message: message, Data: data}}
		if _, err := json.Marshal(p); err != nil {
			return
		}
		checkPacket(t, p)
	})
}

// TestUnrepresentableFramesAreNeverWritten checks frames holding JSON the
// reference SDK cannot hold, which a Rust peer would answer with -32700 or
// fail the transport over: a request fails before anything is written, a
// notification is dropped and reported, and a handler result is answered
// with -32603.
func TestUnrepresentableFramesAreNeverWritten(t *testing.T) {
	reported := make(chan error, 4)
	invalid := json.RawMessage("{\"a\":\"\xff\"}")
	lone := json.RawMessage(`{"a":"\ud800"}`)
	c, peer := pipeClient(t, func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		return invalid, nil
	}, nil, WithErrorHandler(func(err error) { reported <- err }))
	lines := make(chan string, 8)
	go func() {
		for {
			line, err := readLine(peer)
			if err != nil {
				return
			}
			lines <- string(line)
		}
	}()
	ctx := context.Background()
	for _, params := range []json.RawMessage{invalid, lone} {
		if err := c.Call(ctx, "x/ask", params, nil); !errors.Is(err, serdewire.ErrUnrepresentable) {
			t.Fatalf("Call = %v, want ErrUnrepresentable", err)
		}
		if err := c.Notify(ctx, "x/note", params); err != nil {
			t.Fatalf("Notify = %v", err)
		}
		select {
		case err := <-reported:
			if !errors.Is(err, serdewire.ErrUnrepresentable) {
				t.Fatalf("reported %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("the dropped notification was not reported")
		}
	}
	if _, err := peer.Write([]byte(`{"jsonrpc":"2.0","id":5,"method":"x/serve"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case line := <-lines:
		if !strings.HasPrefix(line, `{"jsonrpc":"2.0","id":5,"error":{"code":-32603,"message":"encode response: `) {
			t.Fatalf("answer %s", line)
		}
	case <-time.After(time.Second):
		t.Fatal("the request was not answered")
	}
	select {
	case line := <-lines:
		t.Fatalf("an unrepresentable frame was written: %s", line)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestGuardRewritesAreCompacted checks that params an outgoing guard
// rewrites are compacted before they are written, so no newline inside them
// reaches the wire.
func TestGuardRewritesAreCompacted(t *testing.T) {
	guard := rpcctl.WithOutgoingGuard(func(method string, params json.RawMessage, notification bool) (json.RawMessage, error) {
		return json.RawMessage("{\n  \"a\" : [ 1 ,\n 2 ]\n}"), nil
	}).(ConnectOption)
	c, peer := pipeClient(t, nil, nil, guard)
	got := make(chan []byte, 1)
	go func() { line, _ := readLine(peer); got <- line }()
	if err := c.Notify(context.Background(), "x/note", map[string]int{}); err != nil {
		t.Fatal(err)
	}
	if line := <-got; string(line) != `{"jsonrpc":"2.0","method":"x/note","params":{"a":[1,2]}}`+"\n" {
		t.Fatalf("frame %q", line)
	}
}
