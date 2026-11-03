package v2

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/BrokkAi/acp-go"
	schema "github.com/BrokkAi/acp-go/schema/v2"
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
	var rpcErr *acp.RPCError
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

type paramsHost struct{}

func (paramsHost) RequestPermission(context.Context, schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
	return CancelPermission(), nil
}

func (paramsHost) CreateElicitation(context.Context, schema.CreateElicitationRequest) (schema.CreateElicitationResponse, error) {
	return schema.CreateElicitationResponse{}, nil
}

// TestHandleClientHostAnswersMalformedParamsAsTheReferenceSDK sends each
// client method absent, null, and malformed params.
func TestHandleClientHostAnswersMalformedParamsAsTheReferenceSDK(t *testing.T) {
	handler := HandleClientHost(paramsHost{}, paramsHost{})
	for _, tc := range []paramsCase{
		{schema.SessionRequestPermissionMethodName, "", "invalid type: null, expected struct RequestPermissionRequest", `null`},
		{schema.SessionRequestPermissionMethodName, `null`, "invalid type: null, expected struct RequestPermissionRequest", `null`},
		{schema.SessionRequestPermissionMethodName, `{"sessionId":"s","toolCall":5,"options":[]}`, "missing field `title`", `{"sessionId":"s","toolCall":5,"options":[]}`},
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
		adapter acp.Notifications
	}{
		{schema.SessionUpdateMethodName, SessionUpdates(func(Update) error { return nil })},
		{schema.ElicitationCompleteMethodName, ElicitationComplete(func(schema.CompleteElicitationNotification) error { return nil })},
		{schema.SessionUpdateMethodName, NewSessionTracker().TrackerNotifications(nil)},
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
	var rpcErr *acp.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("%s: error = %v", method, err)
	}
	got, _ := json.Marshal(rpcErr)
	quoted, _ := json.Marshal(method)
	if want := `{"code":-32601,"message":"Method not found","data":` + string(quoted) + `}`; string(got) != want {
		t.Fatalf("%s: got %s, want %s", method, got, want)
	}
}

// TestHandleClientHostAnswersUnservedMethodsAsTheReferenceSDK covers unknown
// and extension methods, agent methods, and client methods without a host.
func TestHandleClientHostAnswersUnservedMethodsAsTheReferenceSDK(t *testing.T) {
	handler := HandleClientHost(nil, nil)
	for _, method := range []string{"foo/bar", "_ext/thing", schema.SessionNewMethodName,
		schema.SessionRequestPermissionMethodName, schema.ElicitationCreateMethodName} {
		_, err := handler(context.Background(), method, json.RawMessage(`{}`))
		requireMethodNotFound(t, err, method)
	}
}
