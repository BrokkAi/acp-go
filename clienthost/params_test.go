package clienthost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/BrokkAi/acp-go"
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

// TestRequestAnswersMalformedParamsAsTheReferenceSDK sends each client method
// the host serves absent, null, and malformed params. The params are checked
// before the session.
func TestRequestAnswersMalformedParamsAsTheReferenceSDK(t *testing.T) {
	h := answerHost(t)
	for _, tc := range []paramsCase{
		{schema.FsReadTextFileMethodName, "", "invalid type: null, expected struct ReadTextFileRequest", `null`},
		{schema.FsReadTextFileMethodName, `null`, "invalid type: null, expected struct ReadTextFileRequest", `null`},
		{schema.FsReadTextFileMethodName, `{"sessionId":"s","path":5}`, "invalid type: integer `5`, expected path string", `{"sessionId":"s","path":5}`},
		{schema.FsWriteTextFileMethodName, "", "invalid type: null, expected struct WriteTextFileRequest", `null`},
		{schema.FsWriteTextFileMethodName, `null`, "invalid type: null, expected struct WriteTextFileRequest", `null`},
		{schema.FsWriteTextFileMethodName, `{"sessionId":"s","path":5}`, "invalid type: integer `5`, expected path string", `{"sessionId":"s","path":5}`},
		{schema.TerminalCreateMethodName, "", "invalid type: null, expected struct CreateTerminalRequest", `null`},
		{schema.TerminalCreateMethodName, `null`, "invalid type: null, expected struct CreateTerminalRequest", `null`},
		{schema.TerminalCreateMethodName, `{"command":5,"sessionId":"s"}`, "invalid type: integer `5`, expected a string", `{"command":5,"sessionId":"s"}`},
		{schema.TerminalOutputMethodName, "", "invalid type: null, expected struct TerminalOutputRequest", `null`},
		{schema.TerminalOutputMethodName, `null`, "invalid type: null, expected struct TerminalOutputRequest", `null`},
		{schema.TerminalOutputMethodName, `{"sessionId":"s","terminalId":5}`, "invalid type: integer `5`, expected a string", `{"sessionId":"s","terminalId":5}`},
		{schema.TerminalWaitForExitMethodName, "", "invalid type: null, expected struct WaitForTerminalExitRequest", `null`},
		{schema.TerminalWaitForExitMethodName, `null`, "invalid type: null, expected struct WaitForTerminalExitRequest", `null`},
		{schema.TerminalWaitForExitMethodName, `{"sessionId":"s","terminalId":5}`, "invalid type: integer `5`, expected a string", `{"sessionId":"s","terminalId":5}`},
		{schema.TerminalKillMethodName, "", "invalid type: null, expected struct KillTerminalRequest", `null`},
		{schema.TerminalKillMethodName, `null`, "invalid type: null, expected struct KillTerminalRequest", `null`},
		{schema.TerminalKillMethodName, `{"sessionId":"s","terminalId":5}`, "invalid type: integer `5`, expected a string", `{"sessionId":"s","terminalId":5}`},
		{schema.TerminalReleaseMethodName, "", "invalid type: null, expected struct ReleaseTerminalRequest", `null`},
		{schema.TerminalReleaseMethodName, `null`, "invalid type: null, expected struct ReleaseTerminalRequest", `null`},
		{schema.TerminalReleaseMethodName, `{"sessionId":"s","terminalId":5}`, "invalid type: integer `5`, expected a string", `{"sessionId":"s","terminalId":5}`},
		{schema.SessionRequestPermissionMethodName, "", "invalid type: null, expected struct RequestPermissionRequest", `null`},
		{schema.SessionRequestPermissionMethodName, `null`, "invalid type: null, expected struct RequestPermissionRequest", `null`},
		{schema.SessionRequestPermissionMethodName, `{"sessionId":"s","toolCall":5,"options":[]}`, "invalid type: integer `5`, expected struct ToolCallUpdate", `{"sessionId":"s","toolCall":5,"options":[]}`},
	} {
		_, err := h.Request(context.Background(), tc.method, json.RawMessage(tc.params))
		requireInvalidParams(t, err, tc)
	}
}

// TestNotificationReadsAbsentParamsAsNull requires a session/update without
// params to fail exactly as one with null params.
func TestNotificationReadsAbsentParamsAsNull(t *testing.T) {
	h := answerHost(t)
	absent := h.Notification(schema.SessionUpdateMethodName, nil)
	null := h.Notification(schema.SessionUpdateMethodName, json.RawMessage(`null`))
	if absent == nil || null == nil || absent.Error() != null.Error() {
		t.Fatalf("absent params = %v, null params = %v", absent, null)
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

// TestRequestAnswersUnservedMethodsAsTheReferenceSDK covers methods the host
// does not serve.
func TestRequestAnswersUnservedMethodsAsTheReferenceSDK(t *testing.T) {
	h := answerHost(t)
	for _, method := range []string{"foo/bar", "_ext/thing", schema.SessionNewMethodName, schema.ElicitationCreateMethodName} {
		_, err := h.Request(context.Background(), method, json.RawMessage(`{}`))
		requireMethodNotFound(t, err, method)
	}
}
