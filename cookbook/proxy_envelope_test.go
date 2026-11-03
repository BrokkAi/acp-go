package cookbook_test

import (
	"bufio"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go/acptest"
)

// These tests drive the recipe's proxy over the raw wire, as the reference
// SDK 5.0.0 conductor does, and pin the proxy-chains RFD behavior: the
// envelope's params may be omitted or null, its own _meta stays on its hop,
// results and errors pass through unchanged, and cancellation is plain.

// wireProxy serves the recipe's v1 proxy and returns the conductor's side
// of the wire.
func wireProxy(t *testing.T) *bufio.ReadWriter {
	t.Helper()
	return acptest.Serve(t, toolsProxy{}.Serve)
}

func send(t *testing.T, rw *bufio.ReadWriter, line string) {
	t.Helper()
	if _, err := rw.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
}

func receive(t *testing.T, rw *bufio.ReadWriter) string {
	t.Helper()
	line, err := rw.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(line, "\n")
}

// receiveID reads one request the proxy sends and returns its id and the
// line with the id elided, since the proxy chooses its own ids.
func receiveID(t *testing.T, rw *bufio.ReadWriter) (string, string) {
	t.Helper()
	line := receive(t, rw)
	var frame struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil || frame.ID == nil {
		t.Fatalf("not a request: %s", line)
	}
	return string(frame.ID), strings.Replace(line, `"id":`+string(frame.ID), `"id":ID`, 1)
}

func TestProxyUnwrapsSuccessorNotifications(t *testing.T) {
	rw := wireProxy(t)
	for _, test := range []struct{ envelope, delivered string }{
		{`{"method":"session/update"}`, `{"jsonrpc":"2.0","method":"session/update"}`},
		{`{"method":"session/update","params":null}`, `{"jsonrpc":"2.0","method":"session/update"}`},
		{`{"method":"session/update","params":{"x":1,"_meta":{"inner":true}}}`, `{"jsonrpc":"2.0","method":"session/update","params":{"x":1,"_meta":{"inner":true}}}`},
		// The envelope's _meta is hop-local; the non-spec meta is not read.
		{`{"method":"session/update","params":{"x":2},"_meta":{"hop":true}}`, `{"jsonrpc":"2.0","method":"session/update","params":{"x":2}}`},
		{`{"method":"session/update","params":{"x":3},"meta":{"hop":true}}`, `{"jsonrpc":"2.0","method":"session/update","params":{"x":3}}`},
	} {
		send(t, rw, `{"jsonrpc":"2.0","method":"proxy/successor","params":`+test.envelope+`}`)
		if got := receive(t, rw); got != test.delivered {
			t.Fatalf("envelope %s delivered %s, want %s", test.envelope, got, test.delivered)
		}
	}
}

func TestProxyWrapsClientNotificationsWithParams(t *testing.T) {
	rw := wireProxy(t)
	send(t, rw, `{"jsonrpc":"2.0","method":"x/note"}`)
	if got, want := receive(t, rw), `{"jsonrpc":"2.0","method":"proxy/successor","params":{"method":"x/note","params":null}}`; got != want {
		t.Fatalf("forwarded %s, want %s", got, want)
	}
	send(t, rw, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`)
	if got, want := receive(t, rw), `{"jsonrpc":"2.0","method":"proxy/successor","params":{"method":"session/cancel","params":{"sessionId":"s"}}}`; got != want {
		t.Fatalf("forwarded %s, want %s", got, want)
	}
}

func TestProxyPassesSuccessorResultsAndErrorsThroughUnchanged(t *testing.T) {
	rw := wireProxy(t)
	for _, test := range []struct{ envelope, forwarded, answer, relayed string }{
		{
			`{"method":"fs/read_text_file"}`,
			`{"jsonrpc":"2.0","id":ID,"method":"fs/read_text_file"}`,
			`"result":{"content":"x","_meta":{"k":1},"future":[1]}`,
			`{"jsonrpc":"2.0","id":7,"result":{"content":"x","_meta":{"k":1},"future":[1]}}`,
		},
		{
			`{"method":"fs/read_text_file","params":{"path":"/a"},"_meta":{"hop":true}}`,
			`{"jsonrpc":"2.0","id":ID,"method":"fs/read_text_file","params":{"path":"/a"}}`,
			`"error":{"code":-32042,"message":"Custom","data":{"k":[1,2]}}`,
			`{"jsonrpc":"2.0","id":7,"error":{"code":-32042,"message":"Custom","data":{"k":[1,2]}}}`,
		},
	} {
		send(t, rw, `{"jsonrpc":"2.0","id":7,"method":"proxy/successor","params":`+test.envelope+`}`)
		id, forwarded := receiveID(t, rw)
		if forwarded != test.forwarded {
			t.Fatalf("forwarded %s, want %s", forwarded, test.forwarded)
		}
		send(t, rw, `{"jsonrpc":"2.0","id":`+id+`,`+test.answer+`}`)
		if got := receive(t, rw); got != test.relayed {
			t.Fatalf("relayed %s, want %s", got, test.relayed)
		}
	}
	// The client's answer to a request from the client side reaches it the
	// same way: a result and an error from the successor, unchanged.
	send(t, rw, `{"jsonrpc":"2.0","id":9,"method":"x/ask","params":{"q":1}}`)
	id, forwarded := receiveID(t, rw)
	if want := `{"jsonrpc":"2.0","id":ID,"method":"proxy/successor","params":{"method":"x/ask","params":{"q":1}}}`; forwarded != want {
		t.Fatalf("forwarded %s, want %s", forwarded, want)
	}
	send(t, rw, `{"jsonrpc":"2.0","id":`+id+`,"error":{"code":-32601,"message":"Method not found","data":"x/ask"}}`)
	if got, want := receive(t, rw), `{"jsonrpc":"2.0","id":9,"error":{"code":-32601,"message":"Method not found","data":"x/ask"}}`; got != want {
		t.Fatalf("relayed %s, want %s", got, want)
	}
}

// receiveTwo reads two lines in either order.
func receiveTwo(t *testing.T, rw *bufio.ReadWriter) []string {
	t.Helper()
	lines := []string{receive(t, rw), receive(t, rw)}
	sort.Strings(lines)
	return lines
}

func TestProxyCancelsForwardedRequestsPlainly(t *testing.T) {
	rw := wireProxy(t)
	// A client request cancelled with a plain $/cancel_request: the proxy
	// cancels its proxy/successor request with a plain $/cancel_request
	// naming that request, not a wrapped one.
	send(t, rw, `{"jsonrpc":"2.0","id":3,"method":"x/slow","params":{}}`)
	id, _ := receiveID(t, rw)
	send(t, rw, `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":3}}`)
	got := receiveTwo(t, rw)
	cancel := `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":` + id + `}}`
	if !contains(got, cancel) || !hasErrorFor(got, "3") {
		t.Fatalf("after a plain cancel the proxy sent %q, want %s and an answer to 3", got, cancel)
	}

	// A successor request cancelled with the wrapped form, which the
	// reference SDK still accepts: the proxy cancels the request it forwarded
	// to the client, plainly, and does not forward the wrapped cancellation.
	send(t, rw, `{"jsonrpc":"2.0","id":8,"method":"proxy/successor","params":{"method":"x/slow","params":{}}}`)
	id, _ = receiveID(t, rw)
	send(t, rw, `{"jsonrpc":"2.0","method":"proxy/successor","params":{"method":"$/cancel_request","params":{"requestId":8}}}`)
	got = receiveTwo(t, rw)
	cancel = `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":` + id + `}}`
	if !contains(got, cancel) || !hasErrorFor(got, "8") {
		t.Fatalf("after a wrapped cancel the proxy sent %q, want %s and an answer to 8", got, cancel)
	}

	// A cancellation wrapped twice is peeled to the same request and is not
	// forwarded either.
	send(t, rw, `{"jsonrpc":"2.0","id":10,"method":"proxy/successor","params":{"method":"x/slow","params":{}}}`)
	id, _ = receiveID(t, rw)
	send(t, rw, `{"jsonrpc":"2.0","method":"proxy/successor","params":{"method":"proxy/successor","params":{"method":"$/cancel_request","params":{"requestId":10}}}}`)
	got = receiveTwo(t, rw)
	cancel = `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":` + id + `}}`
	if !contains(got, cancel) || !hasErrorFor(got, "10") {
		t.Fatalf("after a doubly wrapped cancel the proxy sent %q, want %s and an answer to 10", got, cancel)
	}
	// Nothing else was sent: the next frame answers the next request.
	send(t, rw, `{"jsonrpc":"2.0","method":"x/note"}`)
	if got, want := receive(t, rw), `{"jsonrpc":"2.0","method":"proxy/successor","params":{"method":"x/note","params":null}}`; got != want {
		t.Fatalf("next frame %s, want %s", got, want)
	}
}

func contains(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func hasErrorFor(lines []string, id string) bool {
	for _, line := range lines {
		var frame struct {
			ID    json.RawMessage `json:"id"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(line), &frame) == nil && string(frame.ID) == id && frame.Error != nil {
			return true
		}
	}
	return false
}
