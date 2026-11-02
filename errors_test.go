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
	if err := json.Unmarshal([]byte(`null`), &decoded); err != nil || decoded.Code != 2 {
		t.Fatalf("null should leave the error unchanged: %v %+v", err, decoded)
	}
	for _, malformed := range []string{`17`, `[]`, `"error"`, `{"code":"-32000","message":"peer"}`, `{"code":1.5,"message":"peer"}`, `{"code":-32000,"message":7}`} {
		var target RPCError
		if err := json.Unmarshal([]byte(malformed), &target); err == nil {
			t.Fatalf("decoded malformed error object %s as %+v", malformed, target)
		}
	}
}

func TestRPCErrorRejectsInvalidRawJSON(t *testing.T) {
	for _, err := range []RPCError{
		{Code: 1, Data: json.RawMessage(`{`)},
		{Code: 1, Extra: map[string]json.RawMessage{"bad": json.RawMessage(`nope`)}},
	} {
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
			want = `{"code":-32000,"data":` + data + `,"message":"peer"}`
		}
		if string(encoded) != want {
			t.Fatalf("got %s, want %s", encoded, want)
		}
	}
	if strconv.IntSize == 64 {
		if _, err := (&RPCError{Code: math.MaxInt32 + 1}).SchemaError(); err == nil {
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

func TestHandlerErrorsEncodeExtensions(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
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
			name: "invalid extension JSON",
			err:  &RPCError{Code: -32602, Message: "bad", Extra: map[string]json.RawMessage{"broken": json.RawMessage(`{`)}},
			want: `{"code":-32603,"message":"encode error response: `,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			if strings.HasSuffix(tc.want, ": ") {
				// The encoder's error text belongs to encoding/json; only the
				// fallback code and prefix are part of this package's contract.
				want = `{"jsonrpc":"2.0","id":4,"error":` + tc.want
				if !strings.HasPrefix(string(line), want) || !strings.Contains(string(line), `broken`) {
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
