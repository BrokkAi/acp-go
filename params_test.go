package acp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/BrokkAi/acp-go/schema"
)

// paramsCase is one request whose params the reference SDK's
// json_cast_params decodes: err is the serde error it answers with, and echo
// the params as its data's json member holds them. Empty params are absent
// from the message, which the reference SDK reads as null. The wants are
// captured from agent-client-protocol 5.0.0 with
// agent-client-protocol-schema 1.15.1.
type paramsCase struct {
	method string
	params string
	err    string
	echo   string
}

// requireInvalidParams checks that err is the reference SDK's -32602 for a
// params deserialization failure, as tc describes it.
func requireInvalidParams(t *testing.T, err error, tc paramsCase) {
	t.Helper()
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("%s %s: error = %v", tc.method, tc.params, err)
	}
	got, err := json.Marshal(rpcErr)
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := json.Marshal(tc.err)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"code":-32602,"message":"Invalid params","data":{"error":` + string(quoted) + `,"json":` + tc.echo + `,"phase":"deserialization"}}`
	if string(got) != want {
		t.Fatalf("%s %s:\n got %s\nwant %s", tc.method, tc.params, got, want)
	}
}

// TestHandleElicitationAnswersMalformedParamsAsTheReferenceSDK sends
// elicitation/create absent, null, and malformed params.
func TestHandleElicitationAnswersMalformedParamsAsTheReferenceSDK(t *testing.T) {
	handler := HandleElicitation(nil, func(context.Context, schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error) {
		return CancelElicitation(), nil
	})
	for _, tc := range []paramsCase{
		{schema.ElicitationCreateMethodName, "", "invalid type: null, expected struct CreateElicitationRequest", `null`},
		{schema.ElicitationCreateMethodName, `null`, "invalid type: null, expected struct CreateElicitationRequest", `null`},
		{schema.ElicitationCreateMethodName, `{"elicitationId":5}`, "missing field `message`", `{"elicitationId":5}`},
	} {
		_, err := handler(context.Background(), tc.method, json.RawMessage(tc.params))
		requireInvalidParams(t, err, tc)
	}
}

// TestNotificationAdaptersReadAbsentParamsAsNull requires a notification
// without params to decode exactly as one with null params.
func TestNotificationAdaptersReadAbsentParamsAsNull(t *testing.T) {
	for _, tc := range []struct {
		method  string
		adapter Notifications
	}{
		{schema.SessionUpdateMethodName, SessionUpdates(func(Update) error { return nil })},
		{schema.ElicitationCompleteMethodName, ElicitationComplete(func(schema.CompleteElicitationNotification) error { return nil })},
		{schema.SessionUpdateMethodName, NewSessionProjection().Notifications(nil)},
	} {
		absent := tc.adapter(tc.method, nil)
		null := tc.adapter(tc.method, json.RawMessage(`null`))
		if absent == nil || null == nil || absent.Error() != null.Error() {
			t.Fatalf("%s: absent params = %v, null params = %v", tc.method, absent, null)
		}
	}
}

// requireMethodNotFound checks that err is the reference SDK's answer to a
// request no handler serves: -32601 "Method not found" with the method as
// data.
func requireMethodNotFound(t *testing.T, err error, method string) {
	t.Helper()
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("%s: error = %v", method, err)
	}
	got, _ := json.Marshal(rpcErr)
	quoted, _ := json.Marshal(method)
	if want := `{"code":-32601,"message":"Method not found","data":` + string(quoted) + `}`; string(got) != want {
		t.Fatalf("%s: got %s, want %s", method, got, want)
	}
}

// TestHandleElicitationAnswersUnservedMethodsAsTheReferenceSDK covers other
// methods without next, and elicitation/create without a handler.
func TestHandleElicitationAnswersUnservedMethodsAsTheReferenceSDK(t *testing.T) {
	for _, method := range []string{"foo/bar", "_ext/thing", schema.SessionNewMethodName, schema.ElicitationCreateMethodName} {
		_, err := HandleElicitation(nil, nil)(context.Background(), method, json.RawMessage(`{}`))
		requireMethodNotFound(t, err, method)
	}
}
