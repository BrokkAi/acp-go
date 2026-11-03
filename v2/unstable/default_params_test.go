package unstable_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	schema "github.com/BrokkAi/acp-go/schema/v2/unstable"
	"github.com/BrokkAi/acp-go/v2/agent"
	acpunstable "github.com/BrokkAi/acp-go/v2/unstable"
)

// defaultParamsCase is one params value and the request the reference SDK
// decodes from it, re-encoded, or "" where it answers -32602. The cases were
// captured from agent-client-protocol-schema 1.15.1, whose ListProvidersRequest,
// AuthStatusRequest, and ListSessionsRequest are DefaultOnNull structs: serde
// reads absent and null params as the default request and array params
// positionally, in declaration order.
type defaultParamsCase struct{ params, want string }

// absent stands for a request without params.
const absent = "<absent>"

// metaOnlyCases holds for ListProvidersRequest and AuthStatusRequest, whose
// only field is _meta, which reads as absent when malformed.
var metaOnlyCases = []defaultParamsCase{
	{absent, `{}`},
	{`null`, `{}`},
	{`{}`, `{}`},
	{`[]`, `{}`},
	{`[null]`, `{}`},
	{`[{}]`, `{"_meta":{}}`},
	{`[{"k":1}]`, `{"_meta":{"k":1}}`},
	{`{"_meta":{"k":1}}`, `{"_meta":{"k":1}}`},
	{`{"_meta":5}`, `{}`},
	{`{"_meta":null}`, `{}`},
	{`[5]`, `{}`},
	{`["x"]`, `{}`},
	{`[true]`, `{}`},
	{`{"cwd":"/a"}`, `{}`},
	{`[{},{}]`, ""},
	{`[null,"c"]`, ""},
	{`5`, ""},
	{`"x"`, ""},
	{`true`, ""},
}

// listSessionsCases holds for ListSessionsRequest: cwd, cursor, archived,
// and _meta, in that order.
var listSessionsCases = []defaultParamsCase{
	{absent, `{}`},
	{`null`, `{}`},
	{`[]`, `{}`},
	{`[null]`, `{}`},
	{`["/a"]`, `{"cwd":"/a"}`},
	{`[null,"c"]`, `{"cursor":"c"}`},
	{`[null,null,"only"]`, `{"archived":"only"}`},
	{`[null,null,null,{"k":1}]`, `{"_meta":{"k":1}}`},
	{`[null,null,null,null]`, `{}`},
	{`[null,null,null,5]`, `{}`},
	{`{"_meta":5}`, `{}`},
	{`{"cwd":null}`, `{}`},
	{`{"archived":"only"}`, `{"archived":"only"}`},
	{`{"cursor":"c"}`, `{"cursor":"c"}`},
	{`{"cwd":"/a","archived":"only"}`, `{"cwd":"/a","archived":"only"}`},
	{`[null,null,null,null,null]`, ""},
	{`[5]`, ""},
	{`[{}]`, ""},
	{`[{},{}]`, ""},
	{`{"cwd":5}`, ""},
	{`5`, ""},
}

// paramsRecorder serves providers/list, auth/status, and the archive-aware
// session/list and records the request each one received.
type paramsRecorder struct {
	*acptest.V2Agent
	received any
}

func (r *paramsRecorder) ListProviders(_ context.Context, request schema.ListProvidersRequest) (schema.ListProvidersResponse, error) {
	r.received = request
	return schema.ListProvidersResponse{Providers: []schema.ProviderInfo{}}, nil
}

func (r *paramsRecorder) SetProvider(context.Context, schema.SetProviderRequest) (schema.SetProviderResponse, error) {
	return schema.SetProviderResponse{}, nil
}

func (r *paramsRecorder) DisableProvider(context.Context, schema.DisableProviderRequest) (schema.DisableProviderResponse, error) {
	return schema.DisableProviderResponse{}, nil
}

func (r *paramsRecorder) AuthStatus(_ context.Context, request schema.AuthStatusRequest) (schema.AuthStatusResponse, error) {
	r.received = request
	return schema.AuthStatusResponse{State: schema.AuthStateUnknown}, nil
}

func (r *paramsRecorder) ArchiveSession(context.Context, schema.ArchiveSessionRequest) (schema.ArchiveSessionResponse, error) {
	return schema.ArchiveSessionResponse{}, nil
}

func (r *paramsRecorder) UnarchiveSession(context.Context, schema.UnarchiveSessionRequest) (schema.UnarchiveSessionResponse, error) {
	return schema.UnarchiveSessionResponse{}, nil
}

func (r *paramsRecorder) ListSessionsWithArchive(_ context.Context, request schema.ListSessionsRequest) (schema.ListSessionsResponse, error) {
	r.received = request
	return schema.ListSessionsResponse{Sessions: []schema.SessionInfo{}}, nil
}

func TestDefaultRequestsDecodeParamsAsTheReferenceSDK(t *testing.T) {
	ctx := context.Background()
	recorder := &paramsRecorder{V2Agent: &acptest.V2Agent{}}
	runtime := agent.New(recorder)
	if err := acpunstable.Install(runtime, recorder); err != nil {
		t.Fatal(err)
	}
	handle := acpunstable.Handle(nil, recorder)
	for _, method := range []struct {
		name   string
		serve  acp.Handler
		cases  []defaultParamsCase
		decode func(string) any
	}{
		{schema.ProvidersListMethodName, handle, metaOnlyCases, decodeAs[schema.ListProvidersRequest]},
		{schema.AuthStatusMethodName, handle, metaOnlyCases, decodeAs[schema.AuthStatusRequest]},
		{schema.SessionListMethodName, runtime.Overrides[schema.SessionListMethodName], listSessionsCases, decodeAs[schema.ListSessionsRequest]},
	} {
		for _, c := range method.cases {
			var raw json.RawMessage
			if c.params != absent {
				raw = json.RawMessage(c.params)
			}
			recorder.received = nil
			_, err := method.serve(ctx, method.name, raw)
			if c.want == "" {
				rpc := rpcError(t, err)
				echo := c.params
				if c.params == absent {
					echo = "null"
				}
				if rpc.Code != -32602 || !strings.Contains(string(rpc.Data), `"json":`+echo) || recorder.received != nil {
					t.Errorf("%s %s = %+v (data %s), want -32602 echoing the params", method.name, c.params, rpc, rpc.Data)
				}
				continue
			}
			// Draft-v2 Nullable fields keep an explicit null, which means the
			// same as omission here; compare without null members.
			if err != nil || withoutNulls(t, recorder.received) != withoutNulls(t, method.decode(c.want)) {
				t.Errorf("%s %s received %+v, %v; want %s", method.name, c.params, recorder.received, err, c.want)
			}
		}
	}

	_, err := handle(ctx, schema.AuthStatusMethodName, json.RawMessage(`[{},{}]`))
	var detail struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rpcError(t, err).Data, &detail)
	if detail.Error != "invalid length 2, expected fewer elements in array" {
		t.Fatalf("extra positional params error = %q", detail.Error)
	}
}

// withoutNulls encodes value with its null object members removed.
func withoutNulls(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]any
	if err := json.Unmarshal(encoded, &members); err != nil {
		t.Fatal(err)
	}
	for key, member := range members {
		if member == nil {
			delete(members, key)
		}
	}
	encoded, _ = json.Marshal(members)
	return string(encoded)
}

func decodeAs[T any](wire string) any {
	var value T
	if err := json.Unmarshal([]byte(wire), &value); err != nil {
		panic(err)
	}
	return value
}

// The reference SDK's JSON-RPC layer hands absent params to every request as
// null, so a method with required fields answers both the same way.
func TestAbsentParamsDecodeAsNull(t *testing.T) {
	handle := acpunstable.Handle(nil, &paramsRecorder{})
	_, absentErr := handle(context.Background(), schema.ProvidersSetMethodName, nil)
	_, nullErr := handle(context.Background(), schema.ProvidersSetMethodName, json.RawMessage(`null`))
	absentRPC, nullRPC := rpcError(t, absentErr), rpcError(t, nullErr)
	if absentRPC.Code != -32602 || !reflect.DeepEqual(absentRPC, nullRPC) || !strings.Contains(string(absentRPC.Data), `"json":null`) {
		t.Fatalf("absent params = %+v (data %s), null params = %+v (data %s)", absentRPC, absentRPC.Data, nullRPC, nullRPC.Data)
	}
}
