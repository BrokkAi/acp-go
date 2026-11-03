package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/schema"
)

// paramsCase is one request whose params the reference SDK's
// json_cast_params decodes. A non-empty err is the serde error it answers
// with and echo the params as its data's json member holds them; an empty
// err means the request succeeds. Empty params are absent from the frame. The
// wants are captured from agent-client-protocol 5.0.0 with
// agent-client-protocol-schema 1.15.1, which reads absent params as null.
type paramsCase struct {
	method string
	params string
	err    string
	echo   string
}

// invalidParams renders the reference SDK's -32602 error object for a
// params deserialization failure.
func invalidParams(t *testing.T, detail, echo string) string {
	t.Helper()
	quoted, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	return `{"code":-32602,"message":"Invalid params","data":{"error":` + string(quoted) + `,"json":` + echo + `,"phase":"deserialization"}}`
}

// serveWire starts serve on one end of a pipe and returns the other end,
// read line by line.
func serveWire(t *testing.T, serve func(io.ReadCloser, io.WriteCloser) error) (io.Writer, *bufio.Reader) {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(10 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() { done <- serve(peer, peer) }()
	t.Cleanup(func() {
		_ = local.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("agent runtime did not stop")
		}
	})
	return local, bufio.NewReader(local)
}

// exchange writes one request frame, with params absent when empty, and
// returns the answer line.
func exchange(t *testing.T, w io.Writer, r *bufio.Reader, id int, method, params string) string {
	t.Helper()
	frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q`, id, method)
	if params != "" {
		frame += `,"params":` + params
	}
	if _, err := io.WriteString(w, frame+"}\n"); err != nil {
		t.Fatal(err)
	}
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(line)
}

// requireAnswer checks the answer to request id against the case.
func requireAnswer(t *testing.T, got string, id int, tc paramsCase) {
	t.Helper()
	prefix := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,`, id)
	if tc.err == "" {
		if !strings.HasPrefix(got, prefix+`"result":`) {
			t.Fatalf("%s %s: answer = %s, want a result", tc.method, tc.params, got)
		}
		return
	}
	if want := prefix + `"error":` + invalidParams(t, tc.err, tc.echo) + "}"; got != want {
		t.Fatalf("%s %s:\n got %s\nwant %s", tc.method, tc.params, got, want)
	}
}

// paramsAgent implements and advertises every optional method, so that each
// request reaches its params decode.
type paramsAgent struct{ testAgent }

func (paramsAgent) Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	loadSession := true
	return schema.InitializeResponse{ProtocolVersion: acp.Version, AgentCapabilities: &schema.AgentCapabilities{
		LoadSession: &loadSession,
		SessionCapabilities: &schema.SessionCapabilities{
			Resume: &schema.SessionResumeCapabilities{},
			Close:  &schema.SessionCloseCapabilities{},
			List:   &schema.SessionListCapabilities{},
			Delete: &schema.SessionDeleteCapabilities{},
		},
		Auth: &schema.AgentAuthCapabilities{Logout: &schema.LogoutCapabilities{}},
	}}, nil
}

func (paramsAgent) Authenticate(context.Context, Client, schema.AuthenticateRequest) (schema.AuthenticateResponse, error) {
	return schema.AuthenticateResponse{}, nil
}

func (paramsAgent) LoadSession(context.Context, Client, schema.LoadSessionRequest) (schema.LoadSessionResponse, error) {
	return schema.LoadSessionResponse{}, nil
}

func (paramsAgent) ResumeSession(context.Context, Client, schema.ResumeSessionRequest) (schema.ResumeSessionResponse, error) {
	return schema.ResumeSessionResponse{}, nil
}

func (paramsAgent) CloseSession(context.Context, Client, schema.CloseSessionRequest) (schema.CloseSessionResponse, error) {
	return schema.CloseSessionResponse{}, nil
}

func (paramsAgent) ListSessions(context.Context, Client, schema.ListSessionsRequest) (schema.ListSessionsResponse, error) {
	return schema.ListSessionsResponse{Sessions: []schema.SessionInfo{}}, nil
}

func (paramsAgent) DeleteSession(context.Context, Client, schema.DeleteSessionRequest) (schema.DeleteSessionResponse, error) {
	return schema.DeleteSessionResponse{}, nil
}

func (paramsAgent) SetMode(context.Context, Client, schema.SetSessionModeRequest) (schema.SetSessionModeResponse, error) {
	return schema.SetSessionModeResponse{}, nil
}

func (paramsAgent) SetConfigOption(context.Context, Client, schema.SetSessionConfigOptionRequest) (schema.SetSessionConfigOptionResponse, error) {
	return schema.SetSessionConfigOptionResponse{ConfigOptions: []schema.SessionConfigOption{}}, nil
}

func (paramsAgent) Logout(context.Context, Client, schema.LogoutRequest) (schema.LogoutResponse, error) {
	return schema.LogoutResponse{}, nil
}

// TestRuntimeAnswersMalformedParamsAsTheReferenceSDK sends each runtime
// method absent, null, and malformed params, and requires the exact answer
// the reference SDK sends: absent params decode as null, and a failure is
// json_cast_params' -32602.
func TestRuntimeAnswersMalformedParamsAsTheReferenceSDK(t *testing.T) {
	w, r := serveWire(t, func(in io.ReadCloser, out io.WriteCloser) error {
		return New(paramsAgent{}).Serve(context.Background(), in, out)
	})
	if got := exchange(t, w, r, 0, schema.InitializeMethodName, `{"protocolVersion":1}`); !strings.Contains(got, `"result"`) {
		t.Fatalf("initialize answer = %s", got)
	}
	for i, tc := range []paramsCase{
		{schema.AuthenticateMethodName, "", "invalid type: null, expected struct AuthenticateRequest", `null`},
		{schema.AuthenticateMethodName, `null`, "invalid type: null, expected struct AuthenticateRequest", `null`},
		{schema.AuthenticateMethodName, `{"methodId":5}`, "invalid type: integer `5`, expected a string", `{"methodId":5}`},
		{schema.SessionNewMethodName, "", "invalid type: null, expected struct NewSessionRequest", `null`},
		{schema.SessionNewMethodName, `null`, "invalid type: null, expected struct NewSessionRequest", `null`},
		{schema.SessionNewMethodName, `{"cwd":5}`, "invalid type: integer `5`, expected path string", `{"cwd":5}`},
		{schema.SessionNewMethodName, `{"cwd":"/w","mcpServers":5}`, "invalid type: integer `5`, expected a sequence", `{"cwd":"/w","mcpServers":5}`},
		{schema.SessionNewMethodName, `["s"]`, "invalid length 2, expected struct NewSessionRequest with 4 elements", `["s"]`},
		{schema.SessionLoadMethodName, "", "invalid type: null, expected struct LoadSessionRequest", `null`},
		{schema.SessionLoadMethodName, `null`, "invalid type: null, expected struct LoadSessionRequest", `null`},
		{schema.SessionLoadMethodName, `{"sessionId":5}`, "invalid type: integer `5`, expected a string", `{"sessionId":5}`},
		{schema.SessionResumeMethodName, "", "invalid type: null, expected struct ResumeSessionRequest", `null`},
		{schema.SessionResumeMethodName, `null`, "invalid type: null, expected struct ResumeSessionRequest", `null`},
		{schema.SessionResumeMethodName, `{"sessionId":"s"}`, "missing field `cwd`", `{"sessionId":"s"}`},
		{schema.SessionCloseMethodName, "", "invalid type: null, expected struct CloseSessionRequest", `null`},
		{schema.SessionCloseMethodName, `null`, "invalid type: null, expected struct CloseSessionRequest", `null`},
		{schema.SessionCloseMethodName, `{"sessionId":"a","sessionId":7}`, "invalid type: integer `7`, expected a string", `{"sessionId":7}`},
		{schema.SessionListMethodName, "", "", ""},
		{schema.SessionListMethodName, `null`, "", ""},
		{schema.SessionListMethodName, `{"cursor":5}`, "invalid type: integer `5`, expected a string", `{"cursor":5}`},
		{schema.SessionListMethodName, `[5]`, "invalid type: integer `5`, expected path string", `[5]`},
		{schema.SessionDeleteMethodName, "", "invalid type: null, expected struct DeleteSessionRequest", `null`},
		{schema.SessionDeleteMethodName, `null`, "invalid type: null, expected struct DeleteSessionRequest", `null`},
		{schema.SessionDeleteMethodName, `{"sessionId": 1.50e1 , "x":[1,2.0,-0.0]}`, "invalid type: floating point `15.0`, expected a string", `{"sessionId":15.0,"x":[1,2.0,-0.0]}`},
		{schema.SessionSetModeMethodName, "", "invalid type: null, expected struct SetSessionModeRequest", `null`},
		{schema.SessionSetModeMethodName, `null`, "invalid type: null, expected struct SetSessionModeRequest", `null`},
		{schema.SessionSetModeMethodName, `{"sessionId":"s","modeId":5}`, "invalid type: integer `5`, expected a string", `{"sessionId":"s","modeId":5}`},
		{schema.SessionSetConfigOptionMethodName, "", "invalid type: null, expected struct SetSessionConfigOptionRequest", `null`},
		{schema.SessionSetConfigOptionMethodName, `null`, "invalid type: null, expected struct SetSessionConfigOptionRequest", `null`},
		{schema.SessionSetConfigOptionMethodName, `[]`, "invalid type: sequence, expected struct SetSessionConfigOptionRequest", `[]`},
		{schema.SessionSetConfigOptionMethodName, `{"sessionId":"s","configId":"c","value":{}}`, "data did not match any variant of untagged enum SessionConfigOptionValue", `{"sessionId":"s","configId":"c","value":{}}`},
		{schema.LogoutMethodName, "", "", ""},
		{schema.LogoutMethodName, `null`, "", ""},
		{schema.LogoutMethodName, `[1,2,3,4,5,6,7,8,9]`, "invalid length 9, expected fewer elements in array", `[1,2,3,4,5,6,7,8,9]`},
		{schema.SessionPromptMethodName, "", "invalid type: null, expected struct PromptRequest", `null`},
		{schema.SessionPromptMethodName, `null`, "invalid type: null, expected struct PromptRequest", `null`},
		{schema.SessionPromptMethodName, `{"sessionId":"s","prompt":5}`, "invalid type: integer `5`, expected a sequence", `{"sessionId":"s","prompt":5}`},
		{schema.SessionPromptMethodName, `{"sessionId":"s","prompt":[{"type":"text"}]}`, "missing field `text`", `{"sessionId":"s","prompt":[{"type":"text"}]}`},
	} {
		requireAnswer(t, exchange(t, w, r, i+1, tc.method, tc.params), i+1, tc)
	}
}

// TestHandleClientHostAnswersMalformedParamsAsTheReferenceSDK checks the
// client-side handlers the same way.
func TestHandleClientHostAnswersMalformedParamsAsTheReferenceSDK(t *testing.T) {
	host := testClientHost{}
	handler := HandleClientHost(host, host, host, host)
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
		{schema.ElicitationCreateMethodName, "", "invalid type: null, expected struct CreateElicitationRequest", `null`},
		{schema.ElicitationCreateMethodName, `null`, "invalid type: null, expected struct CreateElicitationRequest", `null`},
		{schema.ElicitationCreateMethodName, `{"elicitationId":5}`, "missing field `message`", `{"elicitationId":5}`},
	} {
		_, err := handler(context.Background(), tc.method, json.RawMessage(tc.params))
		var rpcErr *acp.RPCError
		if !errors.As(err, &rpcErr) {
			t.Fatalf("%s %s: error = %v", tc.method, tc.params, err)
		}
		got, err := json.Marshal(rpcErr)
		if err != nil {
			t.Fatal(err)
		}
		if want := invalidParams(t, tc.err, tc.echo); string(got) != want {
			t.Fatalf("%s %s:\n got %s\nwant %s", tc.method, tc.params, got, want)
		}
	}
}

// methodNotFoundLine renders the reference SDK's answer to request id for a
// method no handler serves, as agent-client-protocol 5.0.0 sends it for both
// roles and protocol versions: -32601 "Method not found" with the method as
// data.
func methodNotFoundLine(t *testing.T, id int, method string) string {
	t.Helper()
	quoted, err := json.Marshal(method)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"Method not found","data":%s}}`, id, quoted)
}

// notify writes one notification frame.
func notify(t *testing.T, w io.Writer, method string) {
	t.Helper()
	if _, err := io.WriteString(w, `{"jsonrpc":"2.0","method":`+fmt.Sprintf("%q", method)+`,"params":{}}`+"\n"); err != nil {
		t.Fatal(err)
	}
}

// requireMethodsNotFound sends each method as a request, after an unknown
// and an extension notification the runtime must ignore without answering,
// and requires the reference SDK's -32601 for each.
func requireMethodsNotFound(t *testing.T, w io.Writer, r *bufio.Reader, methods []string, params string) {
	t.Helper()
	for i, method := range methods {
		notify(t, w, "foo/notify")
		notify(t, w, "_ext/notify")
		id := 100 + i
		if got, want := exchange(t, w, r, id, method, params), methodNotFoundLine(t, id, method); got != want {
			t.Fatalf("%s:\n got %s\nwant %s", method, got, want)
		}
	}
}

// TestRuntimeAnswersUnservedMethodsAsTheReferenceSDK covers unknown methods,
// extension methods, client methods sent the wrong way, and optional methods
// the agent neither implements nor advertises.
func TestRuntimeAnswersUnservedMethodsAsTheReferenceSDK(t *testing.T) {
	w, r := serveWire(t, func(in io.ReadCloser, out io.WriteCloser) error {
		return New(testAgent{}).Serve(context.Background(), in, out)
	})
	if got := exchange(t, w, r, 0, schema.InitializeMethodName, `{"protocolVersion":1}`); !strings.Contains(got, `"result"`) {
		t.Fatalf("initialize answer = %s", got)
	}
	methods := []string{"foo/bar", "_ext/thing", "", schema.SessionLoadMethodName, schema.SessionSetModeMethodName,
		schema.AuthenticateMethodName, schema.SessionListMethodName, schema.FsReadTextFileMethodName, schema.SessionDeleteMethodName,
		schema.SessionResumeMethodName, schema.SessionCloseMethodName, schema.SessionSetConfigOptionMethodName, schema.LogoutMethodName}
	requireMethodsNotFound(t, w, r, methods, `{}`)
	requireMethodsNotFound(t, w, r, []string{"_ext/thing"}, "")
}

// TestHandleClientHostAnswersUnservedMethodsAsTheReferenceSDK covers the
// client-side dispatchers: unknown and extension methods, agent methods, and
// client methods without a configured host.
func TestHandleClientHostAnswersUnservedMethodsAsTheReferenceSDK(t *testing.T) {
	handler := HandleClientHost(nil, nil, nil, nil)
	for _, method := range []string{"foo/bar", "_ext/thing", schema.SessionNewMethodName, schema.FsReadTextFileMethodName,
		schema.TerminalCreateMethodName, schema.SessionRequestPermissionMethodName, schema.ElicitationCreateMethodName} {
		_, err := handler(context.Background(), method, json.RawMessage(`{}`))
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
}
