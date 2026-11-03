package agentrouter

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"

	"github.com/BrokkAi/acp-go/acptest"
	schema1 "github.com/BrokkAi/acp-go/schema"
	schema2 "github.com/BrokkAi/acp-go/schema/v2"
)

// startRouter serves router over the shared in-memory pipe harness.
func startRouter(t *testing.T, router *Agent) *bufio.ReadWriter {
	t.Helper()
	return acptest.Serve(t, router.Serve)
}

func TestRouterRoutesFutureVersionToV2(t *testing.T) {
	requests := make(chan schema2.InitializeRequest, 1)
	rw := startRouter(t, New().
		WithV1(&acptest.V1Agent{Requests: nil}).
		WithV2(&acptest.V2Agent{Requests: requests}))
	acptest.WriteInitialize(t, rw, map[string]any{
		"protocolVersion":        3,
		"info":                   map[string]any{"name": "future-client", "version": "3.0"},
		"_futureInitializeField": map[string]any{"future": true},
	})
	var response struct {
		Result struct {
			ProtocolVersion uint16 `json:"protocolVersion"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rw).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil {
		t.Fatalf("router error: %s", response.Error.Message)
	}
	if response.Result.ProtocolVersion != 2 {
		t.Fatalf("selected protocol = %d", response.Result.ProtocolVersion)
	}
	select {
	case request := <-requests:
		if request.ProtocolVersion != 2 || request.Info.Name != "future-client" {
			t.Fatalf("canonical v2 request = %+v", request)
		}
	default:
		t.Fatal("v2 implementation was not called")
	}
}

func TestRouterCanonicalizesV2ToConfiguredV1(t *testing.T) {
	requests := make(chan schema1.InitializeRequest, 1)
	rw := startRouter(t, New().WithV1(&acptest.V1Agent{Requests: requests}))
	acptest.WriteInitialize(t, rw, map[string]any{
		"protocolVersion": 2,
		"capabilities": map[string]any{
			"auth":        map[string]any{"terminal": map[string]any{}},
			"elicitation": map[string]any{"form": map[string]any{}, "url": map[string]any{}},
		},
		"info": map[string]any{"name": "v2-client", "version": "2.0"},
	})
	var response struct {
		Result struct {
			ProtocolVersion uint16 `json:"protocolVersion"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rw).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil {
		t.Fatalf("router error: %s", response.Error.Message)
	}
	if response.Result.ProtocolVersion != 1 {
		t.Fatalf("selected protocol = %d", response.Result.ProtocolVersion)
	}
	select {
	case request := <-requests:
		if request.ProtocolVersion != 1 || request.ClientInfo == nil || request.ClientInfo.Name != "v2-client" {
			t.Fatalf("canonical v1 request = %+v", request)
		}
		caps := request.ClientCapabilities
		if caps == nil || caps.Auth == nil || caps.Auth.Terminal == nil || !*caps.Auth.Terminal ||
			caps.Elicitation == nil || caps.Session == nil || caps.Session.ConfigOptions == nil ||
			caps.Session.ConfigOptions.Boolean == nil {
			t.Fatalf("canonical v1 capabilities = %+v", caps)
		}
	default:
		t.Fatal("v1 implementation was not called")
	}
}

func TestRouterPreservesFramesBufferedAfterInitialize(t *testing.T) {
	requests := make(chan schema2.InitializeRequest, 1)
	rw := startRouter(t, New().WithV2(&acptest.V2Agent{Requests: requests}))
	initialize, err := json.Marshal(map[string]any{
		"protocolVersion": 2,
		"info":            map[string]any{"name": "client", "version": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": json.RawMessage(initialize)}
	second := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/list", "params": map[string]any{}}
	for _, frame := range []map[string]any{first, second} {
		encoded, err := json.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rw.Write(append(encoded, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for range 2 {
		var response struct {
			ID     int `json:"id"`
			Result any `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.NewDecoder(rw).Decode(&response); err != nil {
			t.Fatal(err)
		}
		seen[response.ID] = true
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("response IDs = %+v", seen)
	}
}

// exchange writes one line and returns the next response line, without its
// terminator.
func exchange(t *testing.T, rw *bufio.ReadWriter, line string) string {
	t.Helper()
	if _, err := rw.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	response, err := rw.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(response, "\n")
}

func selectedVersion(t *testing.T, line string) uint16 {
	t.Helper()
	var response struct {
		Result *struct {
			ProtocolVersion uint16 `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &response); err != nil || response.Result == nil {
		t.Fatalf("initialize response = %s", line)
	}
	return response.Result.ProtocolVersion
}

const (
	v2Initialize       = `{"jsonrpc":"2.0","id":9,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"client","version":"1"}}}`
	invalidVersionData = `"initialize.protocolVersion must be a valid ACP protocol version"`
)

// TestRouterRejectionsKeepReading covers the answers a v2-only router gives
// to first frames that select nothing, each with the request's id, in the
// reference SDK's exact shape. After each one the connection is still
// uninitialized, so a later v2 initialize is routed.
func TestRouterRejectionsKeepReading(t *testing.T) {
	requests := make(chan schema2.InitializeRequest, 1)
	rw := startRouter(t, New().WithV2(&acptest.V2Agent{Requests: requests}))
	for _, frame := range []struct{ wire, answer string }{
		{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":1,"supported":[2]}}}`,
		},
		{
			`{"jsonrpc":"2.0","id":"zero","method":"initialize","params":{"protocolVersion":0}}`,
			`{"jsonrpc":"2.0","id":"zero","error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":0,"supported":[2]}}}`,
		},
		{
			`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":100000,"info":{"name":"client","version":"1"}}}`,
			`{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"Invalid params","data":` + invalidVersionData + `}}`,
		},
		{
			`{"jsonrpc":"2.0","id":3,"method":"initialize","params":{"info":{"name":"client","version":"1"}}}`,
			`{"jsonrpc":"2.0","id":3,"error":{"code":-32602,"message":"Invalid params","data":` + invalidVersionData + `}}`,
		},
		{
			`{"jsonrpc":"2.0","id":4,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"client"}}}`,
			`{"jsonrpc":"2.0","id":4,"error":{"code":-32602,"message":"Invalid params","data":"invalid initialize params: missing field ` + "`version`" + `"}}`,
		},
		{
			`{"jsonrpc":"2.0","id":null,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"client","version":"1"}}}`,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"ACP protocol v2 request ids must be a string or an integer, not null"}}`,
		},
		{
			`{"jsonrpc":"2.0","id":5,"method":"session/new","params":{}}`,
			`{"jsonrpc":"2.0","id":5,"error":{"code":-32600,"message":"Invalid request","data":"first ACP request must be ` + "`initialize`" + `"}}`,
		},
		{
			`{"jsonrpc":"1.0","id":6,"method":"initialize","params":{"protocolVersion":2}}`,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request"}}`,
		},
		{`7`, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request"}}`},
	} {
		if got := exchange(t, rw, frame.wire); got != frame.answer {
			t.Fatalf("frame %s response = %s, want %s", frame.wire, got, frame.answer)
		}
	}
	// Responses and notifications before initialization are ignored: the
	// next line read is the answer to the initialize after them.
	for _, ignored := range []string{
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":"m"}}`,
		`{"result":{}}`,
		`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`,
		`{"jsonrpc":"2.0","method":"initialize","params":{"protocolVersion":2}}`,
	} {
		if _, err := rw.WriteString(ignored + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	answer := exchange(t, rw, v2Initialize)
	if !strings.Contains(answer, `"id":9`) || selectedVersion(t, answer) != 2 {
		t.Fatalf("initialize after rejections = %s", answer)
	}
	if len(requests) != 1 {
		t.Fatalf("v2 Initialize calls = %d, want 1", len(requests))
	}
}

func TestRouterRoutesEverythingNotSelectingV2ToV1(t *testing.T) {
	for _, test := range []struct {
		name  string
		first string
		check func(*testing.T, string, chan schema1.InitializeRequest)
	}{
		{
			name:  "older version",
			first: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":0,"clientInfo":{"name":"old","version":"0"}}}`,
			check: func(t *testing.T, answer string, requests chan schema1.InitializeRequest) {
				if selectedVersion(t, answer) != 1 {
					t.Fatalf("answer = %s", answer)
				}
				request := <-requests
				if request.ProtocolVersion != 1 || request.ClientInfo == nil || request.ClientInfo.Name != "old" {
					t.Fatalf("v1 request = %+v", request)
				}
			},
		},
		{
			name:  "malformed initialize",
			first: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"one"}}`,
			check: func(t *testing.T, answer string, requests chan schema1.InitializeRequest) {
				// The v1 implementation answers it, exactly as without the router.
				want := `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid params","data":` + invalidVersionData + `}}`
				if answer != want || len(requests) != 0 {
					t.Fatalf("answer = %s, want %s", answer, want)
				}
			},
		},
		{
			name:  "other request",
			first: `{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/","mcpServers":[]}}`,
			check: func(t *testing.T, answer string, requests chan schema1.InitializeRequest) {
				want := `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"Invalid request","data":"ACP initialization must complete before ` + "`session/new`" + ` can be used"}}`
				if answer != want || len(requests) != 0 {
					t.Fatalf("answer = %s, want %s", answer, want)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan schema1.InitializeRequest, 1)
			rw := startRouter(t, New().
				WithV1(&acptest.V1Agent{Requests: requests}).
				WithV2(&acptest.V2Agent{}))
			test.check(t, exchange(t, rw, test.first), requests)
		})
	}
}

func TestRouterNormalizesNewerRequestForV2(t *testing.T) {
	requests := make(chan schema2.InitializeRequest, 1)
	rw := startRouter(t, New().WithV2(&acptest.V2Agent{Requests: requests}))
	answer := exchange(t, rw, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":4,"capabilities":{"auth":7}}}`)
	if selectedVersion(t, answer) != 2 {
		t.Fatalf("answer = %s", answer)
	}
	request := <-requests
	if request.ProtocolVersion != 2 || request.Info.Name != "unknown" || request.Info.Version != "unknown" ||
		(request.Capabilities != nil && request.Capabilities.Auth != nil) {
		t.Fatalf("v2 request = %+v", request)
	}
}

// TestRouterRepeatIsNegotiatedBySelectedImplementation checks that a
// repeated initialize is never re-routed: on a v2 route, a repeat requesting
// v1 gets -32022 listing only v2, and the v2 initialization stays in effect.
func TestRouterRepeatIsNegotiatedBySelectedImplementation(t *testing.T) {
	rw := startRouter(t, New().WithV1(&acptest.V1Agent{}).WithV2(&acptest.V2Agent{}))
	if selectedVersion(t, exchange(t, rw, v2Initialize)) != 2 {
		t.Fatal("v2 was not selected")
	}
	answer := exchange(t, rw, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}`)
	want := `{"jsonrpc":"2.0","id":2,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"requested":1,"supported":[2]}}}`
	if answer != want {
		t.Fatalf("v1 repeat = %s, want %s", answer, want)
	}
	if answer := exchange(t, rw, `{"jsonrpc":"2.0","id":3,"method":"session/list","params":{}}`); !strings.Contains(answer, `"result"`) {
		t.Fatalf("session/list after refused repeat = %s", answer)
	}
}

// batchRejection is the exact line the reference SDK sends for any JSON-RPC
// batch array.
const batchRejection = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request","data":"JSON-RPC batches are not supported; send each message individually"}}`

// parseErrorLine is the exact line the reference SDK sends for a line that is
// not valid JSON; quoted is the line, without its terminator, as a JSON string.
func parseErrorLine(quoted string) string {
	return `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error","data":{"line":` + quoted + `}}}`
}

// unreadableFirstFrames are first frames that select no protocol: each gets
// exactly answer, and the router keeps reading.
var unreadableFirstFrames = []struct{ wire, answer string }{
	{"[]\n", batchRejection},
	{`[{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"batch","version":"1"}}}]` + "\n", batchRejection},
	{`[{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}},{"jsonrpc":"2.0","method":"_future/notification","params":{}}]` + "\n", batchRejection},
	{`[{"jsonrpc":"2.0","method":"_future/notification","params":{}}]` + "\n", batchRejection},
	{`[{"jsonrpc":"2.0","id":1,"result":{}}]` + "\n", batchRejection},
	{"[\n", parseErrorLine(`"["`)},
	{"[1,\r\n", parseErrorLine(`"[1,"`)},
	{"\n", parseErrorLine(`""`)},
	{"not-json\n", parseErrorLine(`"not-json"`)},
	{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}` + "\n", parseErrorLine(`"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":1}"`)},
	// serde_json, unlike encoding/json, rejects a float64 overflow, so this
	// is a parse error rather than a batch.
	{"[1e400]\n", parseErrorLine(`"[1e400]"`)},
}

func TestRouterAnswersUnreadableFirstFramesAndKeepsReading(t *testing.T) {
	for _, tc := range []struct {
		name       string
		v1, v2     bool
		initialize uint16
	}{
		{"v1 only", true, false, 1},
		{"v2 only", false, true, 2},
		{"v1 and v2 selecting v1", true, true, 1},
		{"v1 and v2 selecting v2", true, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v1Requests := make(chan schema1.InitializeRequest, 1)
			v2Requests := make(chan schema2.InitializeRequest, 1)
			router := New()
			if tc.v1 {
				router.WithV1(&acptest.V1Agent{Requests: v1Requests})
			}
			if tc.v2 {
				router.WithV2(&acptest.V2Agent{Requests: v2Requests})
			}
			rw := startRouter(t, router)
			for _, frame := range unreadableFirstFrames {
				if _, err := rw.WriteString(frame.wire); err != nil {
					t.Fatal(err)
				}
				if err := rw.Flush(); err != nil {
					t.Fatal(err)
				}
				line, err := rw.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if got := strings.TrimSuffix(line, "\n"); got != frame.answer {
					t.Fatalf("frame %q response = %s, want %s", frame.wire, got, frame.answer)
				}
			}
			if len(v1Requests) != 0 || len(v2Requests) != 0 {
				t.Fatal("an initialize handler ran for an unreadable frame")
			}

			// A single initialize afterwards is routed normally.
			acptest.WriteInitialize(t, rw, map[string]any{
				"protocolVersion": tc.initialize,
				"info":            map[string]any{"name": "client", "version": "1"},
			})
			line, err := rw.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				ID     int `json:"id"`
				Result *struct {
					ProtocolVersion uint16 `json:"protocolVersion"`
				} `json:"result"`
			}
			if err := json.Unmarshal([]byte(line), &response); err != nil {
				t.Fatal(err)
			}
			if response.ID != 1 || response.Result == nil || response.Result.ProtocolVersion != tc.initialize {
				t.Fatalf("initialize afterwards = %s", line)
			}
			if tc.initialize == 1 && (len(v1Requests) != 1 || len(v2Requests) != 0) ||
				tc.initialize == 2 && (len(v2Requests) != 1 || len(v1Requests) != 0) {
				t.Fatalf("initialize handler calls: v1=%d v2=%d", len(v1Requests), len(v2Requests))
			}
		})
	}
}
