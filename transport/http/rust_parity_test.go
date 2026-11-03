package acphttp_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/transport/http"
)

// The expected values in this file are the Rust crate's own output: the
// agent-client-protocol-http 5.0.0 AcpHttpServer serving a Lines agent was
// sent the same bytes, and what its agent read, what it answered, and what
// its event streams wrote were recorded.

// recordingAgent records every line the server writes to it and answers each
// initialize request.
type recordingAgent struct {
	lines chan string
}

func (a *recordingAgent) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	reader := bufio.NewReader(in)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil
		}
		line = strings.TrimSuffix(line, "\n")
		a.lines <- line
		var request struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		if json.Unmarshal([]byte(line), &request) == nil && request.Method == "initialize" {
			_, _ = io.WriteString(out, `{"jsonrpc":"2.0","id":`+string(request.ID)+`,"result":{"protocolVersion":1}}`+"\n")
		}
	}
}

func rustProbePost(t *testing.T, url, connectionID, sessionID string, body []byte) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if connectionID != "" {
		request.Header.Set(acphttp.HeaderConnectionID, connectionID)
	}
	if sessionID != "" {
		request.Header.Set(acphttp.HeaderSessionID, sessionID)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, string(answer)
}

// Client frames reach the agent exactly as the Rust crate forwards them: a
// JSON-RPC message re-serialized as RawJsonRpcMessage (members in order,
// other envelope members and a null params dropped, escapes decoded, the last
// of duplicate members kept, numbers written as serde_json writes them, and
// the session header appended to params), a malformed single-line frame
// verbatim, and a malformed multi-line frame re-encoded as JSON or as a JSON
// string.
func TestServerForwardsClientFramesAsRust(t *testing.T) {
	agent := &recordingAgent{lines: make(chan string, 16)}
	server := httptest.NewServer(&acphttp.Server{Entry: agent})
	t.Cleanup(server.Close)
	url := server.URL + acphttp.DefaultPath
	response, _ := rustProbePost(t, url, "", "", []byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}`))
	connectionID := response.Header.Get(acphttp.HeaderConnectionID)
	if connectionID == "" {
		t.Fatal("initialize opened no connection")
	}
	<-agent.lines
	const sync = `{"jsonrpc":"2.0","method":"_probe/sync"}`
	for _, test := range []struct {
		name, session, body string
		status              int
		contentType, answer string
		agent               []string
	}{
		{`multiline-valid-rpc`, ``, "{\n \"jsonrpc\": \"2.0\",\n \"method\": \"x/y\",\n \"params\": {\"b\":1,\"a\":2}\n}", 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"b":1,"a":2}}`}},
		{`extra-members-single`, ``, `{"zz":1,"jsonrpc":"2.0","method":"x/y","params":{"b":1},"extra":true}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"b":1}}`}},
		{`extra-members-multi`, ``, "{\"zz\":1,\n\"jsonrpc\":\"2.0\",\"method\":\"x/y\",\"params\":{\"b\":1},\"extra\":true}", 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"b":1}}`}},
		{`escapes-single`, ``, `{"jsonrpc":"2.0","method":"x/y","params":{"a":"\u00e9\/\u0001\ud83d\ude00\u003c","b":"é<"}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"a":"é/\u0001😀<","b":"é<"}}`}},
		{`order-single`, ``, `{"params":{"z":1,"a":2},"method":"x/y","id":5,"jsonrpc":"2.0"}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":5,"method":"x/y","params":{"z":1,"a":2}}`}},
		{`dupkey-single`, ``, `{"jsonrpc":"2.0","method":"x/y","params":{"a":1,"a":2}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"a":2}}`}},
		{`params-null`, ``, `{"jsonrpc":"2.0","method":"x/y","params":null}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y"}`}},
		{`ws-single`, ``, "  {\"jsonrpc\":\"2.0\",\"method\":\"x/y\"}  \t", 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y"}`}},
		{`invalid-json-single`, ``, `{not json`, 202, ``, ``, []string{`{not json`}},
		{`invalid-json-multi`, ``, "{not\njson", 202, ``, ``, []string{`"{not\njson"`}},
		{`nonmsg-single`, ``, `{"hello":"world","n":1.0}`, 202, ``, ``, []string{`{"hello":"world","n":1.0}`}},
		{`nonmsg-multi`, ``, "{\"hello\":\n\"world\",\"n\":1.0,\"e\":\"\\u00e9\"}", 202, ``, ``, []string{`{"hello":"world","n":1.0,"e":"é"}`}},
		{`empty`, ``, ``, 202, ``, ``, []string{``}},
		{`ws-only`, ``, `   `, 202, ``, ``, []string{`   `}},
		{`trailing-garbage`, ``, `{"jsonrpc":"2.0","method":"x/y"} xx`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y"} xx`}},
		{`two-values`, ``, "{\"jsonrpc\":\"2.0\",\"method\":\"x/y\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"x/z\"}", 202, ``, ``, []string{`"{\"jsonrpc\":\"2.0\",\"method\":\"x/y\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"x/z\"}"`}},
		{`num-1.0`, ``, `{"jsonrpc":"2.0","method":"x/y","params":{"a":1.0}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"a":1.0}}`}},
		{`num-1e2`, ``, `{"jsonrpc":"2.0","method":"x/y","params":{"a":1e2}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"a":100.0}}`}},
		{`num-big`, ``, `{"jsonrpc":"2.0","method":"x/y","params":{"a":12345678901234567890123}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"a":1.2345678901234568e+22}}`}},
		{`num-neg0`, ``, `{"jsonrpc":"2.0","method":"x/y","params":{"a":-0}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"a":-0.0}}`}},
		{`num-1.50`, ``, `{"jsonrpc":"2.0","method":"x/y","params":{"a":1.50,"b":0.1,"c":1e-7,"d":123456789012.5}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"a":1.5,"b":0.1,"c":1e-7,"d":123456789012.5}}`}},
		{`num-1E400`, ``, `{"jsonrpc":"2.0","method":"x/y","params":{"a":1E400}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"a":1E400}}`}},
		{`num-1E400-multi`, ``, "{\"jsonrpc\":\"2.0\",\n\"method\":\"x/y\",\"params\":{\"a\":1E400}}", 202, ``, ``, []string{`"{\"jsonrpc\":\"2.0\",\n\"method\":\"x/y\",\"params\":{\"a\":1E400}}"`}},
		{`nonmsg-num-multi`, ``, "{\"n\":\n1.0,\"m\":1e2,\"o\":-0}", 202, ``, ``, []string{`{"n":1.0,"m":100.0,"o":-0.0}`}},
		{`nonmsg-dup-multi`, ``, "{\"n\":\n1,\"n\":2}", 202, ``, ``, []string{`{"n":2}`}},
		{`resp-numid-result`, ``, `{"jsonrpc":"2.0","id":7,"result":{"k":1.0,"e":"\u00e9"},"x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":7,"result":{"k":1.0,"e":"é"}}`}},
		{`req-id-string-escape`, ``, `{"jsonrpc":"2.0","id":"\u0061","method":"x/y"}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":"a","method":"x/y"}`}},
		{`method-escape`, ``, `{"jsonrpc":"2.0","method":"x\/y"}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y"}`}},
		{`hdr-session-insert`, `s1`, `{"jsonrpc":"2.0","method":"x/y","params":{"z":1,"a":2}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"z":1,"a":2,"sessionId":"s1"}}`}},
		{`hdr-session-noparams`, `s1`, `{"jsonrpc":"2.0","method":"x/y"}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"sessionId":"s1"}}`}},
		{`hdr-session-nullparams`, `s1`, `{"jsonrpc":"2.0","method":"x/y","params":null}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"sessionId":"s1"}}`}},
		{`hdr-session-array`, `s1`, `{"jsonrpc":"2.0","method":"x/y","params":[1]}`, 400, `text/plain; charset=utf-8`, `Acp-Session-Id header requires object params`, nil},
		{`hdr-session-mismatch`, `s1`, `{"jsonrpc":"2.0","method":"x/y","params":{"sessionId":"s2"}}`, 400, `text/plain; charset=utf-8`, `Acp-Session-Id header does not match params.sessionId`, nil},
		{`hdr-session-nonstring`, `s1`, `{"jsonrpc":"2.0","method":"x/y","params":{"sessionId":5}}`, 400, `text/plain; charset=utf-8`, `params.sessionId must be a string`, nil},
		{`hdr-session-escape`, `s1`, `{"jsonrpc":"2.0","method":"x/y","params":{"e":"\u00e9"},"x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"e":"é","sessionId":"s1"}}`}},
		{`hdr-session-match`, `s1`, `{"jsonrpc":"2.0","method":"x/y","params":{"sessionId":"s1","e":"\u00e9"},"x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"sessionId":"s1","e":"é"}}`}},
		{`hdr-session-response`, `s1`, `{"jsonrpc":"2.0","id":9,"result":{}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":9,"result":{}}`}},
		{`hdr-session-nonmsg`, `s1`, `{"a":1}`, 202, ``, ``, []string{`{"a":1}`}},
		{`prompt-noses`, ``, `{"jsonrpc":"2.0","id":11,"method":"session/prompt","params":{}}`, 400, `text/plain; charset=utf-8`, `Acp-Session-Id header required`, nil},
		{`prompt-sesnull`, ``, `{"jsonrpc":"2.0","id":11,"method":"session/prompt","params":{"sessionId":null}}`, 400, `text/plain; charset=utf-8`, `Acp-Session-Id header required`, nil},
		{`resp-error-extra`, ``, `{"jsonrpc":"2.0","id":7,"error":{"code":1,"message":"m","x":1,"data":null}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":7,"error":{"code":1,"message":"m","data":null,"x":1}}`}},
		{`resp-error-data`, ``, `{"jsonrpc":"2.0","id":7,"error":{"data":{"e":"\u00e9"},"message":"m","code":-1}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":7,"error":{"code":-1,"message":"m","data":{"e":"é"}}}`}},
		{`resp-error-bigcode`, ``, `{"jsonrpc":"2.0","id":7,"error":{"code":9999999999,"message":"m"},"x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":7,"error":{"code":9999999999,"message":"m"},"x":1}`}},
		{`resp-error-floatcode`, ``, `{"jsonrpc":"2.0","id":7,"error":{"code":1.0,"message":"m"},"x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":7,"error":{"code":1.0,"message":"m"},"x":1}`}},
		{`resp-error-nomsg`, ``, `{"jsonrpc":"2.0","id":7,"error":{"code":1},"x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":7,"error":{"code":1},"x":1}`}},
		{`id-exp`, ``, `{"jsonrpc":"2.0","id":1e2,"method":"x/y","x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":1e2,"method":"x/y","x":1}`}},
		{`id-neg0`, ``, `{"jsonrpc":"2.0","id":-0,"method":"x/y","x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":-0,"method":"x/y","x":1}`}},
		{`id-i64min`, ``, `{"jsonrpc":"2.0","id":-9223372036854775808,"method":"x/y","x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":-9223372036854775808,"method":"x/y"}`}},
		{`id-i64max1`, ``, `{"jsonrpc":"2.0","id":9223372036854775808,"method":"x/y","x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":9223372036854775808,"method":"x/y","x":1}`}},
		{`version-1`, ``, `{"jsonrpc":"1.0","method":"x/y","x":1}`, 202, ``, ``, []string{`{"jsonrpc":"1.0","method":"x/y","x":1}`}},
		{`version-missing`, ``, `{"method":"x/y","x":1}`, 202, ``, ``, []string{`{"method":"x/y","x":1}`}},
		{`params-string`, ``, `{"jsonrpc":"2.0","method":"x/y","params":"s","x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":"s","x":1}`}},
		{`method-null`, ``, `{"jsonrpc":"2.0","method":null,"x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":null,"x":1}`}},
		{`notif-result-null`, ``, `{"jsonrpc":"2.0","method":"x/y","result":null,"x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","result":null,"x":1}`}},
		{`resp-null-id`, ``, `{"jsonrpc":"2.0","id":null,"error":{"code":1,"message":"m"},"x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":null,"error":{"code":1,"message":"m"}}`}},
		{`resp-result-null`, ``, `{"jsonrpc":"2.0","id":7,"result":null,"x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":7,"result":null}`}},
		{`notif-id-null`, ``, `{"jsonrpc":"2.0","id":null,"method":"x/y","x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":null,"method":"x/y"}`}},
		{`method-empty`, ``, `{"jsonrpc":"2.0","method":"","x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":""}`}},
		{`method-empty-id`, ``, `{"jsonrpc":"2.0","id":3,"method":"","x":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":3,"method":""}`}},
		{`bom`, ``, "\ufeff{\"jsonrpc\":\"2.0\",\"method\":\"x/y\"}", 202, ``, ``, []string{"\ufeff{\"jsonrpc\":\"2.0\",\"method\":\"x/y\"}"}},
		{`invalid-utf8`, ``, "{\"a\":\"\xff\"}", 400, `text/plain; charset=utf-8`, `Invalid JSON-RPC: invalid utf-8 sequence of 1 bytes from index 6`, nil},
		{`utf8-trunc`, ``, "{\"a\":\"\xe2\x82", 400, `text/plain; charset=utf-8`, `Invalid JSON-RPC: incomplete utf-8 byte sequence from index 6`, nil},
		{`surrogate-utf8`, ``, "{\"a\":\"\xed\xa0\x80\"}", 400, `text/plain; charset=utf-8`, `Invalid JSON-RPC: invalid utf-8 sequence of 1 bytes from index 6`, nil},
		{`lone-surrogate-escape`, ``, `{"jsonrpc":"2.0","method":"x/y","params":{"a":"\ud800"}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"a":"\ud800"}}`}},
		{`ctrl-in-string`, ``, "{\"jsonrpc\":\"2.0\",\"method\":\"x/y\",\"params\":{\"a\":\"\x01\"}}", 202, ``, ``, []string{"{\"jsonrpc\":\"2.0\",\"method\":\"x/y\",\"params\":{\"a\":\"\x01\"}}"}},
		{`deep-cancel`, `s1`, `{"jsonrpc":"2.0","method":"proxy/successor","params":{"method":"$/cancel_request","params":{"requestId":1}}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"proxy/successor","params":{"method":"$/cancel_request","params":{"requestId":1}}}`}},
		{`init-multiline`, ``, "{\"jsonrpc\":\"2.0\",\n\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":1},\"x\":1}", 200, `application/json`, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1}}`, []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`}},
		{`ver-map`, ``, `{"jsonrpc":{"2.0":null},"method":"x/y"}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y"}`}},
		{`err-array`, ``, `{"jsonrpc":"2.0","id":7,"error":[1,"m"]}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":7,"error":[1,"m"]}`}},
		{`err-code-i32max1`, ``, `{"jsonrpc":"2.0","id":7,"error":{"code":2147483648,"message":"m"}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":7,"error":{"code":2147483648,"message":"m"}}`}},
		{`err-code-i32min`, ``, `{"jsonrpc":"2.0","id":7,"error":{"code":-2147483648,"message":"m"}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":7,"error":{"code":-2147483648,"message":"m"}}`}},
		{`err-msg-null`, ``, `{"jsonrpc":"2.0","id":7,"error":{"code":1,"message":null}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":7,"error":{"code":1,"message":null}}`}},
		{`err-dup-code`, ``, `{"jsonrpc":"2.0","id":7,"error":{"code":1,"message":"m","code":2}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":7,"error":{"code":2,"message":"m"}}`}},
		{`err-extra-order`, ``, `{"jsonrpc":"2.0","id":7,"error":{"b":1,"code":1,"a":2,"message":"m","data":3,"c":{"z":1,"y":2}}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":7,"error":{"code":1,"message":"m","data":3,"b":1,"a":2,"c":{"z":1,"y":2}}}`}},
		{`err-nodata`, ``, `{"jsonrpc":"2.0","id":"x","error":{"message":"m","code":1}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":"x","error":{"code":1,"message":"m"}}`}},
		{`result-and-method`, ``, `{"jsonrpc":"2.0","id":1,"method":"m","result":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":1,"method":"m","result":1}`}},
		{`both-result-error`, ``, `{"jsonrpc":"2.0","id":1,"result":1,"error":{"code":1,"message":"m"}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":1,"result":1,"error":{"code":1,"message":"m"}}`}},
		{`dup-method`, ``, `{"jsonrpc":"2.0","method":"a","method":"b"}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"b"}`}},
		{`dup-id`, ``, `{"jsonrpc":"2.0","id":1,"method":"a","id":"z"}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":"z","method":"a"}`}},
		{`dup-params`, ``, `{"jsonrpc":"2.0","method":"a","params":{"q":1},"params":[2]}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"a","params":[2]}`}},
		{`dup-jsonrpc`, ``, `{"jsonrpc":"1.0","method":"a","jsonrpc":"2.0"}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"a"}`}},
		{`params-array`, ``, `{"jsonrpc":"2.0","method":"a","params":[1,1.0,"\u00e9",null,true,{}]}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"a","params":[1,1.0,"é",null,true,{}]}`}},
		{`id-u64`, ``, `{"jsonrpc":"2.0","id":18446744073709551615,"method":"a"}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":18446744073709551615,"method":"a"}`}},
		{`id-bool`, ``, `{"jsonrpc":"2.0","id":true,"method":"a"}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":true,"method":"a"}`}},
		{`id-obj`, ``, `{"jsonrpc":"2.0","id":{},"method":"a"}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":{},"method":"a"}`}},
		{`resp-noid`, ``, `{"jsonrpc":"2.0","result":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","result":1}`}},
		{`hdr-session-dup`, `s1`, `{"jsonrpc":"2.0","method":"x/y","params":{"sessionId":"s2","sessionId":"s1"}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"sessionId":"s1"}}`}},
		{`hdr-session-arrayparams-nonmsg`, `s1`, `{"jsonrpc":"2.0","method":"x/y","params":"s"}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":"s"}`}},
		{`hdr-session-empty-method`, `s1`, `{"jsonrpc":"2.0","method":"","params":{"a":1}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"","params":{"a":1,"sessionId":"s1"}}`}},
		{`hdr-session-multi`, `s1`, "{\"jsonrpc\":\"2.0\",\n\"method\":\"x/y\",\"params\":{\"z\":1}}", 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"z":1,"sessionId":"s1"}}`}},
		{`cancel-hdr`, `s1`, `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`}},
		{`crlf-multi`, ``, "{\"jsonrpc\":\"2.0\",\r\n\"method\":\"x/y\"}", 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y"}`}},
		{`cr-only`, ``, "{\"jsonrpc\":\"2.0\",\r\"method\":\"x/y\"}", 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y"}`}},
		{`big-float`, ``, `{"jsonrpc":"2.0","method":"x/y","params":{"a":1e15,"b":1e16,"c":1e17,"d":0.0001,"e":0.00001,"f":123e-20,"g":-1.5e300}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","method":"x/y","params":{"a":1000000000000000.0,"b":1e+16,"c":1e+17,"d":0.0001,"e":0.00001,"f":1.23e-18,"g":-1.5e+300}}`}},
		{`unicode-keys`, ``, `{"jsonrpc":"2.0","method":"x/y","params":{"\u00e9":"\u2028\u007f\u001f"}}`, 202, ``, ``, []string{"{\"jsonrpc\":\"2.0\",\"method\":\"x/y\",\"params\":{\"é\":\"\u2028\x7f\\u001f\"}}"}},
		{`v1`, ``, `{"jsonrpc":{"2.0":{}},"method":"x/y"}`, 202, ``, ``, []string{`{"jsonrpc":{"2.0":{}},"method":"x/y"}`}},
		{`v2`, ``, `{"jsonrpc":{"2.0":[]},"method":"x/y"}`, 202, ``, ``, []string{`{"jsonrpc":{"2.0":[]},"method":"x/y"}`}},
		{`v3`, ``, `{"jsonrpc":{"2.0":1},"method":"x/y"}`, 202, ``, ``, []string{`{"jsonrpc":{"2.0":1},"method":"x/y"}`}},
		{`v4`, ``, `{"jsonrpc":{"2.0":null,"x":1},"method":"x/y"}`, 202, ``, ``, []string{`{"jsonrpc":{"2.0":null,"x":1},"method":"x/y"}`}},
		{`v5`, ``, `{"jsonrpc":{"2.0":null},"id":1,"result":2}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":1,"result":2}`}},
		{`v6`, ``, `{"jsonrpc":["2.0"],"method":"x/y"}`, 202, ``, ``, []string{`{"jsonrpc":["2.0"],"method":"x/y"}`}},
		{`v7`, ``, `{"jsonrpc":{"1.0":null},"method":"x/y"}`, 202, ``, ``, []string{`{"jsonrpc":{"1.0":null},"method":"x/y"}`}},
		{`v8`, ``, `{"jsonrpc":{"2.0":false},"method":"x/y"}`, 202, ``, ``, []string{`{"jsonrpc":{"2.0":false},"method":"x/y"}`}},
		{`v9`, ``, `{"jsonrpc":{"2.0":""},"method":"x/y"}`, 202, ``, ``, []string{`{"jsonrpc":{"2.0":""},"method":"x/y"}`}},
		{`v10`, ``, `{"jsonrpc":{"2.0":null},"id":1,"error":{"code":1,"message":"m"}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":"m"}}`}},
		{`m1`, ``, `{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":"m","data":null,"data":2}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":"m","data":2}}`}},
		{`e1`, ``, `{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":"m"},"result":null}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":"m"},"result":null}`}},
		{`idnull-v`, ``, `{"jsonrpc":"2.0","id":null,"result":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":null,"result":1}`}},
		{`id-emptyobj-resp`, ``, `{"jsonrpc":"2.0","id":{},"result":1}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":{},"result":1}`}},
		{`id-emptyarr`, ``, `{"jsonrpc":"2.0","id":[],"method":"a"}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":[],"method":"a"}`}},
		{`code-neg0`, ``, `{"jsonrpc":"2.0","id":1,"error":{"code":-0,"message":"m"}}`, 202, ``, ``, []string{`{"jsonrpc":"2.0","id":1,"error":{"code":-0,"message":"m"}}`}},
	} {
		response, answer := rustProbePost(t, url, connectionID, test.session, []byte(test.body))
		if response.StatusCode != test.status || response.Header.Get("Content-Type") != test.contentType || answer != test.answer {
			t.Errorf("%s: answered %d %q %q, want %d %q %q", test.name, response.StatusCode, response.Header.Get("Content-Type"), answer, test.status, test.contentType, test.answer)
		}
		rustProbePost(t, url, connectionID, "", []byte(sync))
		var got []string
		for line := range agent.lines {
			if line == sync {
				break
			}
			got = append(got, line)
		}
		if strings.Join(got, "\n") != strings.Join(test.agent, "\n") || len(got) != len(test.agent) {
			t.Errorf("%s: agent read %q, want %q", test.name, got, test.agent)
		}
	}
}

// linesAgent writes its lines after reading the initialize request.
type linesAgent struct{ lines []string }

func (a linesAgent) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	reader := bufio.NewReader(in)
	if _, err := reader.ReadString('\n'); err != nil {
		return nil
	}
	for _, line := range a.lines {
		if _, err := io.WriteString(out, line+"\n"); err != nil {
			return nil
		}
	}
	<-ctx.Done()
	return nil
}

func readStreamBytes(t *testing.T, url, connectionID, sessionID string, n int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set(acphttp.HeaderConnectionID, connectionID)
	if sessionID != "" {
		request.Header.Set(acphttp.HeaderSessionID, sessionID)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got := make([]byte, n)
	read, err := io.ReadFull(response.Body, got)
	if err != nil {
		t.Errorf("stream %q: read %q: %v", sessionID, got[:read], err)
	}
	return string(got[:read])
}

// Agent lines reach the client as the Rust crate sends them: a JSON-RPC
// message re-serialized and routed, and anything else, an empty or blank line,
// a batch, or a response the Rust SDK cannot read included, verbatim on the
// connection stream, as axum writes SSE data. Only a valid response to the
// initialize request answers the initialize POST.
func TestServerSendsAgentLinesAsRust(t *testing.T) {
	agent := linesAgent{lines: []string{
		`{"x":1,"method":"n/a","jsonrpc":"2.0","params":{"s":"\u003c\u00e9","n":1e2}}`,
		`{"jsonrpc":"2.0","method":"n/b","params":{"sessionId":"S"},"extra":1}`,
		`{"jsonrpc":"2.0","method":"n/c","params":null}`,
		`{"method":"n/d","params":{"sessionId":"S"}}`,
		`{"jsonrpc":"2.0","method":"n/e","params":{"big":12345678901234567890123}}`,
		`not json at all`,
		`{"jsonrpc":"2.0","id":42,"method":"c/req","params":{}, "x": 2}`,
		``,
		`   `,
		"\t",
		"a\rb",
		`{"jsonrpc":"2.0","id":0,"result":1,"x":`,
		`{"id":0,"result":{}}`,
		`{"jsonrpc":"2.0","id":"0","result":{}}`,
		`{"jsonrpc":"2.0","id":0,"error":{"code":1.5,"message":"m"}}`,
		`{"jsonrpc":"2.0","method":"","params":{"sessionId":"S"}}`,
		`{"jsonrpc":"2.0","method":"n/f","params":{"a":1,"a":2,"e":"\u00e9\/"},"zz":0}`,
		`{"jsonrpc":"2.0","id":7,"error":{"x":1,"data":null,"message":"m","code":5}}`,
		`  {"jsonrpc":"2.0","method":"n/g"}  `,
		`[{"jsonrpc":"2.0","method":"n/h"}]`,
		`{"jsonrpc":{"2.0":null},"method":"n/i","params":{"sessionId":"S"}}`,
		`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1,"s":"\u003c\/"},"x":1}`,
	}}
	server := httptest.NewServer(&acphttp.Server{Entry: agent})
	t.Cleanup(server.Close)
	url := server.URL + acphttp.DefaultPath
	response, answer := rustProbePost(t, url, "", "", []byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}`))
	connectionID := response.Header.Get(acphttp.HeaderConnectionID)
	if want := `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1,"s":"</"}}`; response.StatusCode != http.StatusOK || connectionID == "" || answer != want {
		t.Fatalf("initialize answered %d %q %q, want %q", response.StatusCode, connectionID, answer, want)
	}
	wantConnection := "data: {\"jsonrpc\":\"2.0\",\"method\":\"n/a\",\"params\":{\"s\":\"<é\",\"n\":100.0}}\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"n/c\"}\n\ndata: {\"method\":\"n/d\",\"params\":{\"sessionId\":\"S\"}}\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"n/e\",\"params\":{\"big\":1.2345678901234568e+22}}\n\ndata: not json at all\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":42,\"method\":\"c/req\",\"params\":{}}\n\n\ndata:    \n\ndata: \t\n\ndata: a\rdata: b\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":0,\"result\":1,\"x\":\n\ndata: {\"id\":0,\"result\":{}}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"0\",\"result\":{}}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":0,\"error\":{\"code\":1.5,\"message\":\"m\"}}\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"n/f\",\"params\":{\"a\":2,\"e\":\"é/\"}}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"error\":{\"code\":5,\"message\":\"m\",\"data\":null,\"x\":1}}\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"n/g\"}\n\ndata: [{\"jsonrpc\":\"2.0\",\"method\":\"n/h\"}]\n\n"
	if got := readStreamBytes(t, url, connectionID, "", len(wantConnection)); got != wantConnection {
		t.Errorf("connection stream = %q, want %q", got, wantConnection)
	}
	wantSession := "data: {\"jsonrpc\":\"2.0\",\"method\":\"n/b\",\"params\":{\"sessionId\":\"S\"}}\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"\",\"params\":{\"sessionId\":\"S\"}}\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"n/i\",\"params\":{\"sessionId\":\"S\"}}\n\n"
	if got := readStreamBytes(t, url, connectionID, "S", len(wantSession)); got != wantSession {
		t.Errorf("session stream = %q, want %q", got, wantSession)
	}
}

// A malformed line with the initialize id does not answer initialize; the
// error response after it rejects the connection, re-serialized.
func TestServerInitializeIgnoresMalformedResponse(t *testing.T) {
	agent := linesAgent{lines: []string{
		`{"jsonrpc":"2.0","id":0,"result":1,"x":`,
		`{"jsonrpc":"2.0","id":0,"error":{"data":{"e":"\u00e9"},"code":-32603,"message":"boom","q":1},"x":2}`,
	}}
	server := httptest.NewServer(&acphttp.Server{Entry: agent})
	t.Cleanup(server.Close)
	response, answer := rustProbePost(t, server.URL+acphttp.DefaultPath, "", "", []byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}`))
	if want := `{"jsonrpc":"2.0","id":0,"error":{"code":-32603,"message":"boom","data":{"e":"é"},"q":1}}`; response.StatusCode != http.StatusOK || response.Header.Get(acphttp.HeaderConnectionID) != "" || answer != want {
		t.Fatalf("initialize answered %d %q %q, want %q", response.StatusCode, response.Header.Get(acphttp.HeaderConnectionID), answer, want)
	}
}

// batchesAgent writes batch k of its lines after it reads its k-th line. An
// entry "\xff" is a line that is not UTF-8.
type batchesAgent struct{ batches [][]string }

func (a batchesAgent) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	reader := bufio.NewReader(in)
	for k := 0; ; k++ {
		if _, err := reader.ReadString('\n'); err != nil {
			return nil
		}
		if k >= len(a.batches) {
			continue
		}
		for _, line := range a.batches[k] {
			if _, err := io.WriteString(out, line+"\n"); err != nil {
				return nil
			}
		}
	}
}

func readStreamToEnd(t *testing.T, url, connectionID, sessionID string) chan string {
	t.Helper()
	streamed := make(chan string, 1)
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set(acphttp.HeaderConnectionID, connectionID)
	if sessionID != "" {
		request.Header.Set(acphttp.HeaderSessionID, sessionID)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			streamed <- "error: " + err.Error()
			return
		}
		streamed <- string(body)
	}()
	return streamed
}

// An agent line that is not UTF-8 ends the agent's transport as the Rust line
// reader's error does: the frames before it are delivered and the streams
// end, the line and everything after it are dropped, and the connection is
// gone, so later requests get 404.
func TestServerEndsConnectionOnAgentLineNotUTF8(t *testing.T) {
	agent := batchesAgent{batches: [][]string{
		{`{"jsonrpc":"2.0","method":"n/a"}`, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1}}`},
		{`{"jsonrpc":"2.0","method":"n/b","params":{"sessionId":"S"}}`, `{"jsonrpc":"2.0","method":"n/c"}`, "\xff", `{"jsonrpc":"2.0","method":"n/d"}`},
	}}
	server := httptest.NewServer(&acphttp.Server{Entry: agent})
	t.Cleanup(server.Close)
	url := server.URL + acphttp.DefaultPath
	response, answer := rustProbePost(t, url, "", "", []byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}`))
	connectionID := response.Header.Get(acphttp.HeaderConnectionID)
	if want := `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1}}`; response.StatusCode != http.StatusOK || connectionID == "" || answer != want {
		t.Fatalf("initialize answered %d %q %q", response.StatusCode, connectionID, answer)
	}
	connection := readStreamToEnd(t, url, connectionID, "")
	session := readStreamToEnd(t, url, connectionID, "S")
	if response, _ := rustProbePost(t, url, connectionID, "", []byte(`{"jsonrpc":"2.0","method":"x/trigger"}`)); response.StatusCode != http.StatusAccepted {
		t.Fatalf("trigger answered %d", response.StatusCode)
	}
	if got, want := <-connection, "data: {\"jsonrpc\":\"2.0\",\"method\":\"n/a\"}\n\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"n/c\"}\n\n"; got != want {
		t.Errorf("connection stream = %q, want %q", got, want)
	}
	if got, want := <-session, "data: {\"jsonrpc\":\"2.0\",\"method\":\"n/b\",\"params\":{\"sessionId\":\"S\"}}\n\n"; got != want {
		t.Errorf("session stream = %q, want %q", got, want)
	}
	if response, answer := rustProbePost(t, url, connectionID, "", []byte(`{"jsonrpc":"2.0","method":"x/after"}`)); response.StatusCode != http.StatusNotFound || answer != "" {
		t.Errorf("POST after the line answered %d %q, want an empty 404", response.StatusCode, answer)
	}
	request, _ := http.NewRequest(http.MethodDelete, url, nil)
	request.Header.Set(acphttp.HeaderConnectionID, connectionID)
	deleted, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = deleted.Body.Close()
	if deleted.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE answered %d, want 404", deleted.StatusCode)
	}
}

// Before the initialize response, such a line ends the agent the same way, so
// the initialize POST gets the Rust crate's 500.
func TestServerInitializeFailsOnAgentLineNotUTF8(t *testing.T) {
	agent := batchesAgent{batches: [][]string{
		{`{"jsonrpc":"2.0","method":"n/a"}`, "\xff", `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":1}}`},
	}}
	server := httptest.NewServer(&acphttp.Server{Entry: agent})
	t.Cleanup(server.Close)
	response, answer := rustProbePost(t, server.URL+acphttp.DefaultPath, "", "", []byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":1}}`))
	if response.StatusCode != http.StatusInternalServerError || response.Header.Get(acphttp.HeaderConnectionID) != "" || answer != "agent closed before initialize response" {
		t.Fatalf("initialize answered %d %q %q", response.StatusCode, response.Header.Get(acphttp.HeaderConnectionID), answer)
	}
}
