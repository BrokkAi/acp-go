package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

// Codes just outside int32. On 32-bit platforms they wrap, and the tests that
// use them are skipped there because int cannot hold such a code at all.
var (
	int32Max, int32Min int64 = math.MaxInt32, math.MinInt32
	aboveInt32               = int(int32Max + 1)
	belowInt32               = int(int32Min - 1)
)

func TestRPCErrorRoundTripsDataPresenceAndExtensions(t *testing.T) {
	for _, code := range []int{math.MinInt32, -32000, -32002, -32602, 0, 12345, math.MaxInt32} {
		for _, data := range []string{"", "null", `{"detail":[1,2]}`} {
			for _, extra := range []string{"", `,"_meta":{"opaque":"kept"},"extension":{"retry":true}`} {
				wire := fmt.Sprintf(`{"code":%d,"message":"peer"`, code)
				if data != "" {
					wire += `,"data":` + data
				}
				wire += extra + "}"
				var decoded RPCError
				if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
					t.Fatalf("decode %s: %v", wire, err)
				}
				if decoded.Code != code || decoded.Message != "peer" || string(decoded.Data) != data {
					t.Fatalf("decode %s: %+v", wire, decoded)
				}
				if data == "" && decoded.Data != nil {
					t.Fatalf("omitted data decoded as %q", decoded.Data)
				}
				if extra == "" && decoded.Extra != nil {
					t.Fatalf("no extension members decoded as %v", decoded.Extra)
				}
				if extra != "" && (string(decoded.Extra["extension"]) != `{"retry":true}` || string(decoded.Extra["_meta"]) != `{"opaque":"kept"}` || len(decoded.Extra) != 2) {
					t.Fatalf("extension members: %v", decoded.Extra)
				}
				for _, value := range []any{decoded, &decoded} {
					encoded, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					if string(encoded) != wire {
						t.Fatalf("round trip changed the error object:\n got %s\nwant %s", encoded, wire)
					}
				}
			}
		}
	}
}

func TestRPCErrorRelaysThroughPacket(t *testing.T) {
	wire := `{"jsonrpc":"2.0","id":"relay","error":{"code":-32002,"message":"missing","data":null,"extension":["kept"]}}`
	var p packet
	if err := json.Unmarshal([]byte(wire), &p); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != wire {
		t.Fatalf("relay changed the frame:\n got %s\nwant %s", encoded, wire)
	}
}

func TestRPCErrorReservedExtraMembersNeverDuplicate(t *testing.T) {
	err := RPCError{
		Code:    -32000,
		Message: "typed",
		Extra: map[string]json.RawMessage{
			"code":    json.RawMessage(`1`),
			"message": json.RawMessage(`"shadow"`),
			"data":    json.RawMessage(`"shadow"`),
			"z":       json.RawMessage(`true`),
			"Code":    json.RawMessage(`2`),
			"empty":   nil,
		},
	}
	encoded, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if want := `{"code":-32000,"message":"typed","Code":2,"z":true}`; string(encoded) != want {
		t.Fatalf("got %s, want %s", encoded, want)
	}
	err.Data = json.RawMessage(`{"typed":true}`)
	encoded, marshalErr = json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if want := `{"code":-32000,"message":"typed","data":{"typed":true},"Code":2,"z":true}`; string(encoded) != want {
		t.Fatalf("got %s, want %s", encoded, want)
	}
}

func TestRPCErrorMemberNamesAreCaseSensitive(t *testing.T) {
	var decoded RPCError
	if err := json.Unmarshal([]byte(`{"code":1,"message":"m","Code":2,"DATA":null}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Code != 1 || decoded.Data != nil || string(decoded.Extra["Code"]) != "2" || string(decoded.Extra["DATA"]) != "null" {
		t.Fatalf("case-folded member names: %+v", decoded)
	}
}

func TestRPCErrorDecodingReplacesAndRejectsMalformedObjects(t *testing.T) {
	decoded := RPCError{Code: 9, Message: "old", Data: json.RawMessage(`1`), Extra: map[string]json.RawMessage{"stale": json.RawMessage(`1`)}}
	if err := json.Unmarshal([]byte(`{"code":2,"message":"new"}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Code != 2 || decoded.Message != "new" || decoded.Data != nil || decoded.Extra != nil {
		t.Fatalf("stale members survived decoding: %+v", decoded)
	}
	for _, malformed := range []string{
		// The reference SDK's malformed_raw_errors_are_still_rejected list.
		`null`,
		`{"code":-32000}`,
		`{"message":"peer"}`,
		`{"code":null,"message":"peer"}`,
		`{"code":"-32000","message":"peer"}`,
		`{"code":1.5,"message":"peer"}`,
		`{"code":-32000,"message":null}`,
		// Codes the reference SDK's i32 rejects, including float spellings.
		`{"code":2147483648,"message":"peer"}`,
		`{"code":-2147483649,"message":"peer"}`,
		`{"code":1.0,"message":"peer"}`,
		`{"code":1e3,"message":"peer"}`,
		`{"code":-0,"message":"peer"}`,
		// Repeated typed members, which serde rejects as duplicate fields.
		`{"code":1,"message":"peer","code":2}`,
		`{"code":1,"message":"peer","message":"again"}`,
		`{"code":1,"message":"peer","data":1,"data":2}`,
		// Non-objects and wrong member types.
		`17`, `[]`, `"error"`, `{"code":-32000,"message":7}`,
	} {
		target := decoded
		if err := json.Unmarshal([]byte(malformed), &target); err == nil {
			t.Fatalf("decoded malformed error object %s as %+v", malformed, target)
		}
		if target.Code != 2 || target.Message != "new" {
			t.Fatalf("rejected %s but changed the receiver to %+v", malformed, target)
		}
	}
	var repeated RPCError
	if err := json.Unmarshal([]byte(`{"code":1,"message":"peer","x":1,"x":2}`), &repeated); err != nil || string(repeated.Extra["x"]) != "2" {
		t.Fatalf("a repeated extension member should keep its last value: %v %+v", err, repeated)
	}
	for _, code := range []int{math.MinInt32, math.MaxInt32} {
		wire := fmt.Sprintf(`{"code":%d,"message":"edge"}`, code)
		if err := json.Unmarshal([]byte(wire), &repeated); err != nil || repeated.Code != code {
			t.Fatalf("rejected int32 bound %s: %v", wire, err)
		}
	}
}

func TestRPCErrorRoundTripKeepsExtensionMemberOrder(t *testing.T) {
	// As the reference SDK's relays write a received error object, members
	// after code, message, and data keep the order they arrived in, a
	// repeated one at its first position with its last value, and
	// whitespace is dropped. Members added afterwards follow in sorted order.
	wire := `{"zeta":1, "message":"peer","alpha":{"b":2},"data":null,"code":-32000,"_meta":{},"zeta":3}`
	var decoded RPCError
	if err := json.Unmarshal([]byte(wire), &decoded); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"code":-32000,"message":"peer","data":null,"zeta":3,"alpha":{"b":2},"_meta":{}}`; string(encoded) != want {
		t.Fatalf("got  %s\nwant %s", encoded, want)
	}
	decoded.Extra["mid"] = json.RawMessage(`true`)
	decoded.Extra["aaa"] = json.RawMessage(`0`)
	delete(decoded.Extra, "alpha")
	encoded, err = json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"code":-32000,"message":"peer","data":null,"zeta":3,"_meta":{},"aaa":0,"mid":true}`; string(encoded) != want {
		t.Fatalf("got  %s\nwant %s", encoded, want)
	}
	// An Extra map built in Go is written in sorted order.
	built, err := json.Marshal(RPCError{Code: 1, Message: "m", Extra: map[string]json.RawMessage{"b": json.RawMessage(`1`), "a": json.RawMessage(`2`)}})
	if err != nil || string(built) != `{"code":1,"message":"m","a":2,"b":1}` {
		t.Fatalf("built = %s, %v", built, err)
	}
}

func TestRPCErrorRejectsInvalidRawJSON(t *testing.T) {
	for _, err := range []RPCError{
		{Code: 1, Data: json.RawMessage(`{`)},
		{Code: 1, Extra: map[string]json.RawMessage{"bad": json.RawMessage(`nope`)}},
		{Code: aboveInt32},
		{Code: belowInt32},
	} {
		if strconv.IntSize == 32 && err.Data == nil && err.Extra == nil {
			continue
		}
		if _, marshalErr := json.Marshal(err); marshalErr == nil {
			t.Fatalf("encoded invalid raw JSON in %+v", err)
		}
	}
}

func TestRPCErrorTextAndLiteralsAreStable(t *testing.T) {
	err := &RPCError{Code: -32601, Message: "unsupported"}
	if err.Error() != "ACP error -32601: unsupported" {
		t.Fatalf("error text changed: %q", err.Error())
	}
	encoded, marshalErr := json.Marshal(err)
	if marshalErr != nil || string(encoded) != `{"code":-32601,"message":"unsupported"}` {
		t.Fatalf("literal encoding changed: %s %v", encoded, marshalErr)
	}
}

func TestRPCErrorSchemaErrorDropsExtensions(t *testing.T) {
	for _, data := range []string{"", "null", `{"detail":"kept"}`} {
		raw := &RPCError{Code: -32000, Message: "peer", Data: json.RawMessage(data), Extra: map[string]json.RawMessage{"extension": json.RawMessage(`true`)}}
		if data == "" {
			raw.Data = nil
		}
		typed, err := raw.SchemaError()
		if err != nil {
			t.Fatal(err)
		}
		if typed.Code != -32000 || typed.Message != "peer" || string(typed.Data) != data {
			t.Fatalf("schema error %+v", typed)
		}
		encoded, err := json.Marshal(typed)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"code":-32000,"message":"peer"}`
		if data != "" {
			want = `{"code":-32000,"message":"peer","data":` + data + `}`
		}
		if string(encoded) != want {
			t.Fatalf("got %s, want %s", encoded, want)
		}
	}
	if strconv.IntSize == 64 {
		if _, err := (&RPCError{Code: aboveInt32}).SchemaError(); err == nil {
			t.Fatal("converted a code outside int32")
		}
	}
}

func TestCallReturnsPeerErrorWithExtensions(t *testing.T) {
	c, peer := pipeClient(t, nil, nil)
	const errorObject = `{"code":-32000,"message":"login","data":null,"extension":{"retry":true}}`
	go func() {
		var request packet
		if err := json.NewDecoder(peer).Decode(&request); err != nil {
			return
		}
		_, _ = fmt.Fprintf(peer, `{"jsonrpc":"2.0","id":%s,"error":%s}`+"\n", request.ID, errorObject)
	}()
	err := c.Call(context.Background(), "session/prompt", map[string]string{}, nil)
	var rpc *RPCError
	if !errors.As(err, &rpc) {
		t.Fatalf("expected *RPCError, got %v", err)
	}
	if !IsAuthRequired(err) || IsResourceNotFound(err) {
		t.Fatalf("classification with extension members: %v", err)
	}
	if string(rpc.Data) != "null" || string(rpc.Extra["extension"]) != `{"retry":true}` {
		t.Fatalf("lost error members: %+v", rpc)
	}
	encoded, marshalErr := json.Marshal(rpc)
	if marshalErr != nil || string(encoded) != errorObject {
		t.Fatalf("re-emitted %s %v, want %s", encoded, marshalErr, errorObject)
	}
}

// forwardedPeerError returns the *RPCError a Call would return for the given
// peer error object, as a relaying handler would receive it.
func forwardedPeerError(t *testing.T, errorObject string) error {
	t.Helper()
	c, peer := pipeClient(t, nil, nil)
	go func() {
		var request packet
		if err := json.NewDecoder(peer).Decode(&request); err != nil {
			return
		}
		_, _ = fmt.Fprintf(peer, `{"jsonrpc":"2.0","id":%s,"error":%s}`+"\n", request.ID, errorObject)
	}()
	err := c.Call(context.Background(), "upstream/request", nil, nil)
	var rpc *RPCError
	if !errors.As(err, &rpc) {
		t.Fatalf("expected *RPCError from Call, got %v", err)
	}
	return rpc
}

func TestHandlerErrorsEncodeExtensions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		want     string
		contains string
	}{
		{
			name: "extensions and null data",
			err: fmt.Errorf("wrapped: %w", &RPCError{Code: -32002, Message: "gone", Data: json.RawMessage(`null`), Extra: map[string]json.RawMessage{
				"extension": json.RawMessage(`{"retry":false}`),
				"code":      json.RawMessage(`1`),
			}}),
			want: `{"code":-32002,"message":"gone","data":null,"extension":{"retry":false}}`,
		},
		{
			name: "omitted data",
			err:  &RPCError{Code: -32602, Message: "bad"},
			want: `{"code":-32602,"message":"bad"}`,
		},
		{
			name: "forwarded peer error",
			err:  forwardedPeerError(t, `{"zeta":"z","code":-32000,"message":"login","_meta":{"trace":1}}`),
			// The peer's member order is kept, as the reference SDK relays it.
			want: `{"code":-32000,"message":"login","zeta":"z","_meta":{"trace":1}}`,
		},
		{
			name:     "invalid extension JSON",
			err:      &RPCError{Code: -32602, Message: "bad", Extra: map[string]json.RawMessage{"broken": json.RawMessage(`{`)}},
			want:     `{"code":-32603,"message":"encode error response: `,
			contains: `broken`,
		},
		{
			name:     "code outside int32",
			err:      &RPCError{Code: aboveInt32, Message: "too wide"},
			want:     `{"code":-32603,"message":"encode error response: `,
			contains: `2147483648`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "code outside int32" && strconv.IntSize == 32 {
				t.Skip("int cannot hold a code outside int32")
			}
			_, peer := pipeClient(t, func(context.Context, string, json.RawMessage) (any, error) {
				return nil, tc.err
			}, nil)
			go func() {
				_, _ = peer.Write([]byte(`{"jsonrpc":"2.0","id":4,"method":"fs/read_text_file","params":{}}` + "\n"))
			}()
			line, err := bufio.NewReader(peer).ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			want := `{"jsonrpc":"2.0","id":4,"error":` + tc.want + "}\n"
			if tc.contains != "" {
				// The encoder's error text belongs to encoding/json; only the
				// fallback code and prefix are part of this package's contract.
				want = `{"jsonrpc":"2.0","id":4,"error":` + tc.want
				if !strings.HasPrefix(string(line), want) || !strings.Contains(string(line), tc.contains) {
					t.Fatalf("got  %s\nwant prefix %s", line, want)
				}
				return
			}
			if string(line) != want {
				t.Fatalf("got  %s\nwant %s", line, want)
			}
		})
	}
}
