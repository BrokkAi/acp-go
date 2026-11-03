package acp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/internal/rpcctl"
)

// TestCancelRequestIDMatchesTheReferenceSDK pins the reference SDK 5.0.0's
// reading of a cancellation: plain or wrapped in proxy/successor envelopes,
// with params parsed as the v1 CancelRequestNotification.
func TestCancelRequestIDMatchesTheReferenceSDK(t *testing.T) {
	for _, test := range []struct {
		method, params string
		id             string
		kind           cancellation
	}{
		{"$/cancel_request", `{"requestId":7}`, `7`, cancelsRequest},
		{"$/cancel_request", `{"requestId":null}`, `null`, cancelsRequest},
		{"$/cancel_request", `{"requestId":7,"_meta":5}`, `7`, cancelsRequest},
		// serde's positional form of the struct: [requestId, _meta].
		{"$/cancel_request", `[8,{}]`, `8`, cancelsRequest},
		{"$/cancel_request", `[12]`, `12`, cancelsRequest},
		{"$/cancel_request", ` [ "a" ] `, `"a"`, cancelsRequest},
		{"$/cancel_request", `[]`, ``, malformedCancellation},
		{"$/cancel_request", `[[5]]`, ``, malformedCancellation},
		// Ids compare as the reference SDK decodes them.
		{"$/cancel_request", `{"requestId":"\u0061"}`, `"a"`, cancelsRequest},
		{"$/cancel_request", `{"requestId":10,"requestId":11}`, `11`, cancelsRequest},
		{"$/cancel_request", `{"requestId":9223372036854775808}`, ``, malformedCancellation},
		{"$/cancel_request", `{"requestId":[5]}`, ``, malformedCancellation},
		// The positional form of the envelope is not peeled.
		{"proxy/successor", `["$/cancel_request",{"requestId":7}]`, ``, notCancellation},
		{"proxy/successor", `{"method":"$/cancel_request","params":{"requestId":"req-1"}}`, `"req-1"`, cancelsRequest},
		{"proxy/successor", `{"method":"proxy/successor","params":{"method":"$/cancel_request","params":{"requestId":2}}}`, `2`, cancelsRequest},
		{"$/cancel_request", `{}`, ``, malformedCancellation},
		{"$/cancel_request", ``, ``, malformedCancellation},
		{"$/cancel_request", `null`, ``, malformedCancellation},
		{"$/cancel_request", `{"requestId":{"not":"an id"}}`, ``, malformedCancellation},
		{"$/cancel_request", `{"requestId":7.5}`, ``, malformedCancellation},
		{"proxy/successor", `{"method":"$/cancel_request"}`, ``, malformedCancellation},
		{"proxy/successor", `{"method":"session/update","params":{}}`, ``, notCancellation},
		{"proxy/successor", `{"bogus":true}`, ``, notCancellation},
		{"proxy/successor", `{"method":5}`, ``, notCancellation},
		{"proxy/successor", `[]`, ``, notCancellation},
		// The pre-RFD name is an ordinary method, not an envelope.
		{"_proxy/successor", `{"method":"$/cancel_request","params":{"requestId":7}}`, ``, notCancellation},
	} {
		id, kind := cancelRequestID(test.method, json.RawMessage(test.params))
		if kind != test.kind || string(id) != test.id {
			t.Errorf("%s %s = %s, %d; want %s, %d", test.method, test.params, id, kind, test.id, test.kind)
		}
	}
}

// TestCancellationCancelsAndReachesTheHandler checks that a
// $/cancel_request, plain or wrapped in proxy/successor, cancels the inbound
// request it names and is then delivered to the notification handler, as
// the reference SDK dispatches it, while one whose params do not parse is
// dropped.
func TestCancellationCancelsAndReachesTheHandler(t *testing.T) {
	delivered := make(chan string, 4)
	_, peer := pipeClient(t, func(ctx context.Context, method string, p json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, func(method string, p json.RawMessage) error {
		delivered <- method + " " + string(p)
		return nil
	})
	e := json.NewEncoder(peer)
	d := json.NewDecoder(peer)
	for _, test := range []struct{ id, method, params string }{
		{`8`, "proxy/successor", `{"method":"$/cancel_request","params":{"requestId":8}}`},
		{`9`, "$/cancel_request", `{"requestId":9}`},
	} {
		_ = e.Encode(packet{Version: "2.0", ID: json.RawMessage(test.id), Method: "x/slow", Params: json.RawMessage(`{}`)})
		// A malformed cancellation is dropped before the real one.
		_ = e.Encode(packet{Version: "2.0", Method: "$/cancel_request", Params: json.RawMessage(`{"requestId":{"x":1}}`)})
		_ = e.Encode(packet{Version: "2.0", Method: test.method, Params: json.RawMessage(test.params)})
		var p packet
		if err := d.Decode(&p); err != nil {
			t.Fatal(err)
		}
		if p.Error == nil || p.Error.Code != -32800 || string(p.ID) != test.id {
			t.Fatalf("bad cancelled response: %+v", p)
		}
		select {
		case got := <-delivered:
			if want := test.method + " " + test.params; got != want {
				t.Fatalf("delivered %s, want %s", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("cancellation not delivered")
		}
	}
	if len(delivered) != 0 {
		t.Fatalf("malformed cancellation delivered: %s", <-delivered)
	}
}

// TestOutgoingParamsMatchTheReferenceSerialization checks that a request or
// notification without params omits the member, as the reference SDK
// serializes one, and that params JSON-RPC does not allow are refused before
// anything is written.
func TestOutgoingParamsMatchTheReferenceSerialization(t *testing.T) {
	reported := make(chan error, 8)
	c, peer := pipeClient(t, nil, nil, WithErrorHandler(func(err error) { reported <- err }))
	lines := make(chan string, 8)
	go func() {
		d := json.NewDecoder(peer)
		for {
			var raw json.RawMessage
			if d.Decode(&raw) != nil {
				return
			}
			lines <- string(raw)
		}
	}()
	ctx := context.Background()
	for _, params := range []any{nil, json.RawMessage(nil), json.RawMessage("null"), json.RawMessage(" null ")} {
		if err := c.Notify(ctx, "x/note", params); err != nil {
			t.Fatal(err)
		}
		if got, want := <-lines, `{"jsonrpc":"2.0","method":"x/note"}`; got != want {
			t.Fatalf("Notify(%#v) wrote %s, want %s", params, got, want)
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_ = c.Call(callCtx, "x/ask", nil, nil)
	if got := <-lines; got != `{"jsonrpc":"2.0","id":1,"method":"x/ask"}` {
		t.Fatalf("Call(nil) wrote %s", got)
	}
	for _, params := range []any{5, "text", true, json.RawMessage(`"x"`)} {
		// As in the reference SDK, a notification send succeeds and the
		// frame is dropped, with the error reported; a request fails.
		if err := c.Notify(ctx, "x/note", params); err != nil {
			t.Fatalf("Notify(%#v) = %v", params, err)
		}
		var rpcErr *RPCError
		select {
		case err := <-reported:
			if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 || string(rpcErr.Data) != `"JSON-RPC params must be an object or array"` {
				t.Fatalf("Notify(%#v) reported %v", params, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("Notify(%#v) reported nothing", params)
		}
		err := c.Call(ctx, "x/ask", params, nil)
		if !asRPCError(err, &rpcErr) || rpcErr.Code != -32602 || string(rpcErr.Data) != `"JSON-RPC params must be an object or array"` {
			t.Fatalf("Call(%#v) = %v", params, err)
		}
	}
	// The cancellation of the timed-out call follows; nothing else was sent.
	select {
	case got := <-lines:
		if got != `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}` {
			t.Fatalf("unexpected frame %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no cancellation for the timed-out call")
	}
	select {
	case got := <-lines:
		t.Fatalf("refused params were written: %s", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func asRPCError(err error, target **RPCError) bool {
	e, ok := err.(*RPCError)
	if ok {
		*target = e
	}
	return ok
}

// TestRequestIDsCompareAsDecoded checks that the connection matches and echoes
// JSON-RPC ids as the reference SDK decodes them: a cancellation spelled with
// escapes cancels the request, the response carries the id re-serialized, and
// a response to an outgoing call is matched the same way.
func TestRequestIDsCompareAsDecoded(t *testing.T) {
	c, peer := pipeClient(t, func(ctx context.Context, method string, p json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, nil)
	lines := make(chan string, 8)
	go func() {
		d := json.NewDecoder(peer)
		for {
			var raw json.RawMessage
			if d.Decode(&raw) != nil {
				return
			}
			lines <- string(raw)
		}
	}()
	write := func(line string) {
		t.Helper()
		if _, err := peer.Write([]byte(line + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	receive := func() string {
		t.Helper()
		select {
		case line := <-lines:
			return line
		case <-time.After(2 * time.Second):
			t.Fatal("no frame")
			return ""
		}
	}
	for _, test := range []struct{ id, cancel, echoed string }{
		{`"a"`, `"\u0061"`, `"a"`},
		{`"\u0062"`, `"b"`, `"b"`},
		{`"\u00e9"`, `"\u00E9"`, `"é"`},
	} {
		write(`{"jsonrpc":"2.0","id":` + test.id + `,"method":"x/slow","params":{}}`)
		write(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":` + test.cancel + `}}`)
		want := `{"jsonrpc":"2.0","id":` + test.echoed + `,"error":{"code":-32800,"message":"Request cancelled"}}`
		if got := receive(); got != want {
			t.Fatalf("request %s cancelled with %s: got %s, want %s", test.id, test.cancel, got, want)
		}
	}

	// A response whose id is an escaped string does not match an integer
	// id, and an integer id matches only its decoded value.
	done := make(chan error, 1)
	var result map[string]int
	go func() { done <- c.Call(context.Background(), "x/ask", map[string]int{}, &result) }()
	request := receive()
	var frame struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal([]byte(request), &frame); err != nil {
		t.Fatal(err)
	}
	write(`{"jsonrpc":"2.0","id":"` + string(frame.ID) + `","result":{"n":1}}`)
	write(`{"jsonrpc":"2.0","id":` + string(frame.ID) + `,"result":{"n":2}}`)
	if err := <-done; err != nil || result["n"] != 2 {
		t.Fatalf("Call = %v, %v", result, err)
	}
}

func TestIsCancelRequestNotificationPeelsEveryEnvelope(t *testing.T) {
	for _, test := range []struct {
		method, params string
		want           bool
	}{
		{"$/cancel_request", `{"requestId":1}`, true},
		// Like the reference SDK's check, only the method is compared.
		{"$/cancel_request", `{}`, true},
		{"proxy/successor", `{"method":"$/cancel_request","params":{"requestId":1}}`, true},
		{"proxy/successor", `{"method":"proxy/successor","params":{"method":"$/cancel_request"}}`, true},
		{"proxy/successor", `{"method":"session/update","params":{}}`, false},
		{"proxy/successor", `{"bogus":true}`, false},
		{"session/cancel", `{"sessionId":"s"}`, false},
	} {
		if got := IsCancelRequestNotification(test.method, json.RawMessage(test.params)); got != test.want {
			t.Errorf("%s %s = %t, want %t", test.method, test.params, got, test.want)
		}
	}
}

// TestOutgoingGuardRunsBeforeTheParamsCheck checks the reference SDK's order:
// the protocol guard sees an outgoing message first, so its refusal wins over
// the params check, and a request the guard admitted whose params then cannot
// be sent is settled, so what the guard began does not stay in progress.
func TestOutgoingGuardRunsBeforeTheParamsCheck(t *testing.T) {
	refusal := &RPCError{Code: -32600, Message: "Invalid request"}
	var seen []string
	guard := rpcctl.WithOutgoingGuard(func(method string, params json.RawMessage, notification bool) (json.RawMessage, error) {
		seen = append(seen, method+" "+string(params))
		if method == "x/refused" {
			return nil, refusal
		}
		return nil, nil
	}).(ConnectOption)
	reported := make(chan error, 2)
	c, _ := pipeClient(t, nil, nil, guard, WithErrorHandler(func(err error) { reported <- err }))
	ctx := context.Background()
	if err := c.Call(ctx, "x/refused", 5, nil); err != refusal {
		t.Fatalf("refused Call = %v, want the guard's error", err)
	}
	settled := make(chan error, 1)
	err := c.Call(rpcctl.WithSettled(ctx, func(method string, err error) { settled <- err }), "x/admitted", 5, nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 {
		t.Fatalf("admitted Call = %v", err)
	}
	select {
	case got := <-settled:
		if got != err {
			t.Fatalf("settled with %v, want %v", got, err)
		}
	default:
		t.Fatal("the admitted call was not settled")
	}
	if err := c.Notify(ctx, "x/refused", 5); err != refusal {
		t.Fatalf("refused Notify = %v, want the guard's error", err)
	}
	if len(seen) != 3 || seen[0] != "x/refused 5" || seen[1] != "x/admitted 5" {
		t.Fatalf("guard saw %q", seen)
	}
	if len(reported) != 0 {
		t.Fatalf("reported %v", <-reported)
	}
}
