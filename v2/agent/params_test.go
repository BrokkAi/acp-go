package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	schema "github.com/BrokkAi/acp-go/schema/v2"
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
type paramsAgent struct{ *echoAgent }

func (paramsAgent) Initialize(context.Context, Client, schema.InitializeRequest) (schema.InitializeResponse, error) {
	return schema.InitializeResponse{
		ProtocolVersion: version,
		Info:            schema.Implementation{Name: "params", Version: "test"},
		Capabilities:    &schema.AgentCapabilities{Session: &schema.SessionCapabilities{Delete: &schema.SessionDeleteCapabilities{}}},
		AuthMethods:     []schema.AuthMethod{{Agent: &schema.AuthMethodAgent{MethodID: "key", Name: "Key"}}},
	}, nil
}

func (paramsAgent) AuthLogin(context.Context, Client, schema.LoginAuthRequest) (schema.LoginAuthResponse, error) {
	return schema.LoginAuthResponse{}, nil
}

func (paramsAgent) AuthLogout(context.Context, Client, schema.LogoutAuthRequest) (schema.LogoutAuthResponse, error) {
	return schema.LogoutAuthResponse{}, nil
}

func (paramsAgent) DeleteSession(context.Context, Client, schema.DeleteSessionRequest) (schema.DeleteSessionResponse, error) {
	return schema.DeleteSessionResponse{}, nil
}

func (paramsAgent) SetConfigOption(context.Context, Client, schema.SetSessionConfigOptionRequest) (schema.SetSessionConfigOptionResponse, error) {
	return schema.SetSessionConfigOptionResponse{ConfigOptions: []schema.SessionConfigOption{}}, nil
}

// TestRuntimeAnswersMalformedParamsAsTheReferenceSDK sends each runtime
// method absent, null, and malformed params, and requires the exact answer
// the reference SDK sends: absent params decode as null, and a failure is
// json_cast_params' -32602.
func TestRuntimeAnswersMalformedParamsAsTheReferenceSDK(t *testing.T) {
	w, r := serveWire(t, func(in io.ReadCloser, out io.WriteCloser) error {
		return New(paramsAgent{&echoAgent{}}).Serve(context.Background(), in, out)
	})
	if got := exchange(t, w, r, 0, schema.InitializeMethodName, `{"protocolVersion":2,"info":{"name":"c","version":"1"}}`); !strings.Contains(got, `"result"`) {
		t.Fatalf("initialize answer = %s", got)
	}
	for i, tc := range []paramsCase{
		{schema.AuthLoginMethodName, "", "invalid type: null, expected struct LoginAuthRequest", `null`},
		{schema.AuthLoginMethodName, `null`, "invalid type: null, expected struct LoginAuthRequest", `null`},
		{schema.AuthLoginMethodName, `[5]`, "invalid type: integer `5`, expected a string", `[5]`},
		{schema.AuthLogoutMethodName, "", "", ""},
		{schema.AuthLogoutMethodName, `null`, "", ""},
		{schema.AuthLogoutMethodName, `[1,2,3,4,5,6,7,8,9]`, "invalid length 9, expected fewer elements in array", `[1,2,3,4,5,6,7,8,9]`},
		{schema.SessionNewMethodName, "", "invalid type: null, expected struct NewSessionRequest", `null`},
		{schema.SessionNewMethodName, `null`, "invalid type: null, expected struct NewSessionRequest", `null`},
		{schema.SessionNewMethodName, `{"cwd":5}`, "invalid type: integer `5`, expected path string", `{"cwd":5}`},
		{schema.SessionNewMethodName, `{"cwd":"/w","mcpServers":5}`, "invalid type: integer `5`, expected a sequence", `{"cwd":"/w","mcpServers":5}`},
		{schema.SessionResumeMethodName, "", "invalid type: null, expected struct ResumeSessionRequest", `null`},
		{schema.SessionResumeMethodName, `null`, "invalid type: null, expected struct ResumeSessionRequest", `null`},
		{schema.SessionResumeMethodName, `{"sessionId":"s"}`, "missing field `cwd`", `{"sessionId":"s"}`},
		{schema.SessionCloseMethodName, "", "invalid type: null, expected struct CloseSessionRequest", `null`},
		{schema.SessionCloseMethodName, `null`, "invalid type: null, expected struct CloseSessionRequest", `null`},
		{schema.SessionCloseMethodName, `{"sessionId":"a","sessionId":7}`, "invalid type: integer `7`, expected a string", `{"sessionId":7}`},
		{schema.SessionListMethodName, "", "", ""},
		{schema.SessionListMethodName, `null`, "", ""},
		{schema.SessionListMethodName, `{"cursor":5}`, "invalid type: integer `5`, expected a string", `{"cursor":5}`},
		{schema.SessionDeleteMethodName, "", "invalid type: null, expected struct DeleteSessionRequest", `null`},
		{schema.SessionDeleteMethodName, `null`, "invalid type: null, expected struct DeleteSessionRequest", `null`},
		{schema.SessionDeleteMethodName, `{"sessionId": 1.50e1 , "x":[1,2.0,-0.0]}`, "invalid type: floating point `15.0`, expected a string", `{"sessionId":15.0,"x":[1,2.0,-0.0]}`},
		{schema.SessionSetConfigOptionMethodName, "", "invalid type: null, expected struct SetSessionConfigOptionRequest", `null`},
		{schema.SessionSetConfigOptionMethodName, `null`, "invalid type: null, expected struct SetSessionConfigOptionRequest", `null`},
		{schema.SessionSetConfigOptionMethodName, `{"sessionId":"s","configId":"c","value":{}}`, "missing field `type`", `{"sessionId":"s","configId":"c","value":{}}`},
		{schema.SessionPromptMethodName, "", "invalid type: null, expected struct PromptRequest", `null`},
		{schema.SessionPromptMethodName, `null`, "invalid type: null, expected struct PromptRequest", `null`},
		{schema.SessionPromptMethodName, `{"sessionId":"s","prompt":5}`, "invalid type: integer `5`, expected a sequence", `{"sessionId":"s","prompt":5}`},
	} {
		requireAnswer(t, exchange(t, w, r, i+1, tc.method, tc.params), i+1, tc)
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
// extension methods, client and v1 methods, and the methods an agent serves
// only when it implements and advertises them.
func TestRuntimeAnswersUnservedMethodsAsTheReferenceSDK(t *testing.T) {
	for _, tc := range []struct {
		agent   Agent
		methods []string
	}{
		{&echoAgent{}, []string{"foo/bar", "_ext/thing", "", "session/load", "session/set_mode", "authenticate",
			schema.SessionRequestPermissionMethodName, schema.AuthLoginMethodName, schema.AuthLogoutMethodName,
			schema.SessionDeleteMethodName, schema.SessionSetConfigOptionMethodName}},
	} {
		w, r := serveWire(t, func(in io.ReadCloser, out io.WriteCloser) error {
			return New(tc.agent).Serve(context.Background(), in, out)
		})
		if got := exchange(t, w, r, 0, schema.InitializeMethodName, `{"protocolVersion":2,"info":{"name":"c","version":"1"}}`); !strings.Contains(got, `"result"`) {
			t.Fatalf("initialize answer = %s", got)
		}
		requireMethodsNotFound(t, w, r, tc.methods, `{}`)
		requireMethodsNotFound(t, w, r, []string{"_ext/thing"}, "")
	}
}
