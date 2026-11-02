package acphttp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func notification(sessionID string, extra string) []byte {
	return fmt.Appendf(nil, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":%q%s}}`, sessionID, extra)
}

// routeLine routes one agent line as the connection router does.
func routeLine(out *outbound, line []byte) {
	if parsed, ok := parseMessage(line); ok {
		out.route(parsed, parsed.encode())
		return
	}
	out.routeMalformed(line)
}

func taken(t *testing.T, l *lease) []string {
	t.Helper()
	var frames []string
	for {
		payload, ok := l.next()
		if !ok {
			return frames
		}
		frames = append(frames, string(payload))
	}
}

func expectFrames(t *testing.T, l *lease, want ...string) {
	t.Helper()
	got := taken(t, l)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("frames = %q, want %q", got, want)
	}
}

func TestMailboxBuffersBurstsInOrderAcrossSubscribers(t *testing.T) {
	out := newOutbound()
	var want []string
	for i := range 1025 {
		frame := fmt.Sprintf(`{"jsonrpc":"2.0","method":"_test/n","params":{"n":%d}}`, i)
		routeLine(out, []byte(frame))
		want = append(want, frame)
	}
	first, err := out.leaseConnection()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.leaseConnection(); !errors.Is(err, errStreamBusy) {
		t.Fatalf("second subscriber = %v", err)
	}
	expectFrames(t, first, want...)
	routeLine(out, []byte(`{"jsonrpc":"2.0","method":"_test/queued","params":{}}`))
	first.release()
	resumed, err := out.leaseConnection()
	if err != nil {
		t.Fatal(err)
	}
	expectFrames(t, resumed, `{"jsonrpc":"2.0","method":"_test/queued","params":{}}`)
}

func TestUnknownSessionStreamsAreBoundedAndReleased(t *testing.T) {
	out := newOutbound()
	leases := make([]*lease, 0, maxUnconfirmedSessionStreams)
	for i := range maxUnconfirmedSessionStreams {
		l, err := out.leaseSession(fmt.Sprintf("made-up-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
	}
	if _, err := out.leaseSession("one-too-many"); !errors.Is(err, errUnknownSession) {
		t.Fatalf("lease beyond the cap = %v", err)
	}
	leases[0].release()
	if len(out.sessions) != maxUnconfirmedSessionStreams-1 {
		t.Fatalf("sessions = %d", len(out.sessions))
	}
	if _, err := out.leaseSession("one-too-many"); err != nil {
		t.Fatal(err)
	}
	for _, l := range leases[1:] {
		l.release()
	}
	if len(out.sessions) != 1 {
		t.Fatalf("sessions = %d", len(out.sessions))
	}
}

func TestCreatedSessionIsKnownBeforeItsResponseIsQueued(t *testing.T) {
	out := newOutbound()
	for i := range maxUnconfirmedSessionStreams {
		if _, err := out.leaseSession(fmt.Sprintf("made-up-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	connection, err := out.leaseConnection()
	if err != nil {
		t.Fatal(err)
	}
	out.recordPending(json.RawMessage("1"), pendingRoute{effect: effectFor("session/new")})
	routeLine(out, []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"created"}}`))
	routeLine(out, notification("created", ""))

	expectFrames(t, connection, `{"jsonrpc":"2.0","id":1,"result":{"sessionId":"created"}}`)
	session, err := out.leaseSession("created")
	if err != nil {
		t.Fatal(err)
	}
	expectFrames(t, session, string(notification("created", "")))
	// A known session survives its subscriber leaving.
	session.release()
	if _, err := out.leaseSession("created"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionRequestForUnknownSessionAllocatesNoStream(t *testing.T) {
	out := newOutbound()
	connection, _ := out.leaseConnection()
	out.recordPending(json.RawMessage("7"), pendingRoute{session: "made-up", scoped: true, effect: effectFor("session/prompt")})
	if len(out.sessions) != 0 {
		t.Fatal("recording a route allocated a stream")
	}
	failure := `{"jsonrpc":"2.0","id":7,"error":{"code":-32602,"message":"Invalid params"}}`
	routeLine(out, []byte(failure))
	if len(out.sessions) != 0 {
		t.Fatal("an error response allocated a stream")
	}
	expectFrames(t, connection, failure)

	// A success names the session, but its own response still goes to the
	// connection stream, which is where the client was told to look.
	out.recordPending(json.RawMessage(`"s8"`), pendingRoute{session: "made-up", scoped: true})
	routeLine(out, []byte(`{"jsonrpc":"2.0","id":"s8","result":{}}`))
	expectFrames(t, connection, `{"jsonrpc":"2.0","id":"s8","result":{}}`)
	if stream := out.sessions["made-up"]; stream == nil || !stream.confirmed {
		t.Fatal("a successful session-scoped response must name the session")
	}
}

func TestStreamOpenedBeforeLoadIsConfirmedByItsResult(t *testing.T) {
	out := newOutbound()
	session, err := out.leaseSession("loaded")
	if err != nil {
		t.Fatal(err)
	}
	out.recordPending(json.RawMessage("2"), pendingRoute{session: "loaded", scoped: true, effect: effectFor("session/load")})
	routeLine(out, notification("loaded", ""))
	routeLine(out, []byte(`{"jsonrpc":"2.0","id":2,"result":{}}`))
	expectFrames(t, session, string(notification("loaded", "")), `{"jsonrpc":"2.0","id":2,"result":{}}`)
	session.release()
	if len(out.sessions) != 1 {
		t.Fatal("a loaded session must stay known")
	}
}

func TestFailedLoadLeavesSessionUnknown(t *testing.T) {
	out := newOutbound()
	session, err := out.leaseSession("missing")
	if err != nil {
		t.Fatal(err)
	}
	out.recordPending(json.RawMessage("3"), pendingRoute{session: "missing", scoped: true, effect: effectFor("session/load")})
	routeLine(out, []byte(`{"jsonrpc":"2.0","id":3,"error":{"code":-32602,"message":"Invalid params"}}`))
	expectFrames(t, session, `{"jsonrpc":"2.0","id":3,"error":{"code":-32602,"message":"Invalid params"}}`)
	session.release()
	if len(out.sessions) != 0 {
		t.Fatal("a failed load must leave the session unknown")
	}
}

func TestClosedSessionStreamIsRemoved(t *testing.T) {
	out := newOutbound()
	connection, _ := out.leaseConnection()
	out.mu.Lock()
	out.confirm("open")
	out.confirm("unwatched")
	out.mu.Unlock()
	session, err := out.leaseSession("open")
	if err != nil {
		t.Fatal(err)
	}
	for id, sessionID := range map[string]string{"4": "open", "5": "unwatched"} {
		out.recordPending(json.RawMessage(id), pendingRoute{session: sessionID, scoped: true, effect: effectFor("session/close")})
		routeLine(out, fmt.Appendf(nil, `{"jsonrpc":"2.0","id":%s,"result":{}}`, id))
	}
	if len(out.sessions) != 0 {
		t.Fatalf("sessions = %d", len(out.sessions))
	}
	// A subscribed stream still gets its close response; without a
	// subscriber the response goes to the connection stream.
	expectFrames(t, session, `{"jsonrpc":"2.0","id":4,"result":{}}`)
	expectFrames(t, connection, `{"jsonrpc":"2.0","id":5,"result":{}}`)
}

func TestConnectionScopedFramesUseTheConnectionStream(t *testing.T) {
	out := newOutbound()
	connection, _ := out.leaseConnection()
	session, _ := out.leaseSession("session-1")
	frames := []string{
		`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1,"sessionId":"session-1"}}`,
		`{"jsonrpc":"2.0","method":"proxy/successor","params":{"method":"proxy/successor","params":{"method":"$/cancel_request","params":{"requestId":1,"sessionId":"session-1"}}}}`,
		`not json`,
		`{"jsonrpc":"2.0","id":9,"result":{"sessionId":"session-1"}}`,
	}
	for _, frame := range frames {
		routeLine(out, []byte(frame))
	}
	expectFrames(t, connection, frames...)
	expectFrames(t, session)

	// A successor envelope around anything else follows params.sessionId.
	wrapped := `{"jsonrpc":"2.0","method":"proxy/successor","params":{"method":"session/update","sessionId":"session-1"}}`
	routeLine(out, []byte(wrapped))
	expectFrames(t, session, wrapped)
}

func TestApplySessionHeader(t *testing.T) {
	for _, test := range []struct{ body, want, err string }{
		{`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"prompt":[]}}`, `{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"prompt":[],"sessionId":"s"}}`, ""},
		// The header is appended after the other params, which keep their
		// order, and the message is written as the Rust SDK writes it.
		{`{"params":{"z":1,"a":2},"method":"x/y","jsonrpc":"2.0","extra":1}`, `{"jsonrpc":"2.0","method":"x/y","params":{"z":1,"a":2,"sessionId":"s"}}`, ""},
		{`{"jsonrpc":"2.0","method":"","params":{"a":1}}`, `{"jsonrpc":"2.0","method":"","params":{"a":1,"sessionId":"s"}}`, ""},
		{`{"jsonrpc":"2.0","method":"x/y","params":{"sessionId":"t","sessionId":"s"}}`, `{"jsonrpc":"2.0","method":"x/y","params":{"sessionId":"s"}}`, ""},
		{`{"jsonrpc":"2.0","method":"proxy/successor","params":{"method":"$/cancel_request","params":{"requestId":1}}}`, `{"jsonrpc":"2.0","method":"proxy/successor","params":{"method":"$/cancel_request","params":{"requestId":1}}}`, ""},
		{`{"jsonrpc":"2.0","method":"session/cancel"}`, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`, ""},
		{`{"jsonrpc":"2.0","method":"session/cancel","params":null}`, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`, ""},
		{`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`, ""},
		{`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`, `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`, ""},
		{`{"jsonrpc":"2.0","id":1,"result":{}}`, `{"jsonrpc":"2.0","id":1,"result":{}}`, ""},
		{`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"t"}}`, "", "Acp-Session-Id header does not match params.sessionId"},
		{`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":null}}`, "", "params.sessionId must be a string"},
		{`{"jsonrpc":"2.0","method":"session/cancel","params":[1]}`, "", "Acp-Session-Id header requires object params"},
	} {
		parsed, ok := parseMessage([]byte(test.body))
		if !ok {
			t.Fatalf("%s is not a message", test.body)
		}
		updated, err := applySessionHeader(parsed, "s")
		if test.err != "" {
			if err == nil || err.Error() != test.err {
				t.Errorf("%s: err = %v, want %q", test.body, err, test.err)
			}
			continue
		}
		if got := updated.encode(); err != nil || string(got) != test.want {
			t.Errorf("%s: got %s, %v; want %s", test.body, got, err, test.want)
		}
		if id, ok := updated.sessionID(); updated.hasMethod() && !updated.connectionScoped() && (!ok || id != "s") {
			t.Errorf("%s: frame session = %q", test.body, id)
		}
	}
}

func TestCORSOriginCheck(t *testing.T) {
	header := func(origin string) http.Header {
		h := http.Header{}
		if origin != "" {
			h.Set("Origin", origin)
		}
		return h
	}
	allowlist, err := AllowOrigins("http://localhost:5173")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		cors   CORSOptions
		origin string
		want   bool
	}{
		{CORSDisabled(), "", true},
		{CORSDisabled(), "http://localhost:5173", false},
		{allowlist, "", true},
		{allowlist, "http://localhost:5173", true},
		{allowlist, "http://localhost:3000", false},
		{AllowAnyOrigin(), "https://example.com", true},
	} {
		if got := test.cors.allowsOrigin(header(test.origin)); got != test.want {
			t.Errorf("%+v allowsOrigin(%q) = %v", test.cors, test.origin, got)
		}
	}
}
