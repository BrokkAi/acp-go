package acphttp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/internal/serdejson"
)

// probeStream is one scripted GET answer: a status, raw body bytes, and
// whether the stream ends after them.
type probeStream struct {
	status int
	body   string
	close  bool
}

// The expected output of each scenario is the Rust client's: an
// agent-client-protocol-http 5.0.0 HttpClient driven by a Lines client
// against the same fake server, recording the requests the server got, the
// lines the client was handed, and how the transport ended. A Go error is
// shown as the Rust AcpError the client fails with: its text is the error's
// data, under "Internal error", or "Invalid request" for a first frame that
// is not initialize.
var rustClientScenarios = []struct {
	name    string
	lines   []string
	streams map[string]probeStream
	want    string
}{
	{
		name:    `malformed-posts`,
		lines:   []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`, `not json`, `[1,2]`, `{"x":1}`, `  {"jsonrpc":"2.0","method":"x/y","zz":1}  `, `{"jsonrpc":"2.0","id":7,"result":{}`, `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"S"},"x":1`, ``},
		streams: map[string]probeStream{},
		want:    "  POST conn=\"\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"method\\\":\\\"initialize\\\",\\\"params\\\":{\\\"protocolVersion\\\":1}}\"\n  POST conn=\"conn\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"not json\"\n  POST conn=\"conn\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"[1,2]\"\n  POST conn=\"conn\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"x\\\":1}\"\n  POST conn=\"conn\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"method\\\":\\\"x/y\\\"}\"\n  POST conn=\"conn\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":7,\\\"result\\\":{}\"\n  POST conn=\"conn\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"method\\\":\\\"session/update\\\",\\\"params\\\":{\\\"sessionId\\\":\\\"S\\\"},\\\"x\\\":1\"\n  POST conn=\"conn\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"\"\n  DELETE conn=\"conn\"\n  GET conn=\"conn\" sess=\"\" accept=\"text/event-stream\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"result\\\":{\\\"protocolVersion\\\":1}}\"\n  RESULT Ok after close\n",
	},
	{
		name:    `first-malformed`,
		lines:   []string{`not json`},
		streams: map[string]probeStream{},
		want:    "  RESULT Err Invalid request: \"ACP HTTP transport: first message must be `initialize`\"\n",
	},
	{
		name:    `first-not-initialize`,
		lines:   []string{`{"jsonrpc":"2.0","method":"x/y"}`},
		streams: map[string]probeStream{},
		want:    "  RESULT Err Invalid request: \"ACP HTTP transport: first message must be `initialize`\"\n",
	},
	{
		name:    `missing-session`,
		lines:   []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`, `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{}}`},
		streams: map[string]probeStream{},
		want:    "  POST conn=\"\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"method\\\":\\\"initialize\\\",\\\"params\\\":{\\\"protocolVersion\\\":1}}\"\n  DELETE conn=\"conn\"\n  GET conn=\"conn\" sess=\"\" accept=\"text/event-stream\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"result\\\":{\\\"protocolVersion\\\":1}}\"\n  RESULT Err Internal error: \"POST: method `session/prompt` requires sessionId in params\"\n",
	},
	{
		name:    `conn-stream-500`,
		lines:   []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`},
		streams: map[string]probeStream{``: {500, `nope`, true}},
		want:    "  POST conn=\"\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"method\\\":\\\"initialize\\\",\\\"params\\\":{\\\"protocolVersion\\\":1}}\"\n  DELETE conn=\"conn\"\n  GET conn=\"conn\" sess=\"\" accept=\"text/event-stream\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"result\\\":{\\\"protocolVersion\\\":1}}\"\n  RESULT Err Internal error: \"connection SSE stream ended: HTTP 500 Internal Server Error\"\n",
	},
	{
		name:    `conn-stream-204`,
		lines:   []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`, `{"jsonrpc":"2.0","method":"x/y"}`},
		streams: map[string]probeStream{``: {204, ``, true}},
		want:    "  POST conn=\"\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"method\\\":\\\"initialize\\\",\\\"params\\\":{\\\"protocolVersion\\\":1}}\"\n  DELETE conn=\"conn\"\n  GET conn=\"conn\" sess=\"\" accept=\"text/event-stream\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"result\\\":{\\\"protocolVersion\\\":1}}\"\n  RESULT Err Internal error: \"connection SSE stream ended: SSE stream closed\"\n",
	},
	{
		name:    `conn-stream-closed`,
		lines:   []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`},
		streams: map[string]probeStream{``: {200, "data: {\"jsonrpc\":\"2.0\",\"method\":\"n/a\"}\n\n", true}},
		want:    "  POST conn=\"\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"method\\\":\\\"initialize\\\",\\\"params\\\":{\\\"protocolVersion\\\":1}}\"\n  DELETE conn=\"conn\"\n  GET conn=\"conn\" sess=\"\" accept=\"text/event-stream\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"result\\\":{\\\"protocolVersion\\\":1}}\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"method\\\":\\\"n/a\\\"}\"\n  RESULT Err Internal error: \"connection SSE stream ended: SSE stream closed\"\n",
	},
	{
		name:    `session-stream-404`,
		lines:   []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`, `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"S"}}`},
		streams: map[string]probeStream{`S`: {404, ``, true}},
		want:    "  POST conn=\"\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"method\\\":\\\"initialize\\\",\\\"params\\\":{\\\"protocolVersion\\\":1}}\"\n  DELETE conn=\"conn\"\n  GET conn=\"conn\" sess=\"\" accept=\"text/event-stream\"\n  GET conn=\"conn\" sess=\"S\" accept=\"text/event-stream\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"result\\\":{\\\"protocolVersion\\\":1}}\"\n  RESULT Err Internal error: \"S SSE stream ended: HTTP 404 Not Found\"\n",
	},
	{
		name:    `utf8-invalid-open`,
		lines:   []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`},
		streams: map[string]probeStream{``: {200, "data: {\"jsonrpc\":\"2.0\",\"method\":\"n/a\"}\x0a\x0adata: {\"jsonrpc\":\"2.0\",\"method\":\"n/b\"}\xff\x0a\x0adata: {\"jsonrpc\":\"2.0\",\"method\":\"n/c\"}\x0a\x0a", false}},
		want:    "  POST conn=\"\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"method\\\":\\\"initialize\\\",\\\"params\\\":{\\\"protocolVersion\\\":1}}\"\n  DELETE conn=\"conn\"\n  GET conn=\"conn\" sess=\"\" accept=\"text/event-stream\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"result\\\":{\\\"protocolVersion\\\":1}}\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"method\\\":\\\"n/a\\\"}\"\n  RESULT Ok after close\n",
	},
	{
		name:    `utf8-invalid-closed`,
		lines:   []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`},
		streams: map[string]probeStream{``: {200, "data: {\"jsonrpc\":\"2.0\",\"method\":\"n/a\"}\x0a\x0adata: {\"jsonrpc\":\"2.0\",\"method\":\"n/b\"}\xff\x0a\x0adata: {\"jsonrpc\":\"2.0\",\"method\":\"n/c\"}\x0a\x0a", true}},
		want:    "  POST conn=\"\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"method\\\":\\\"initialize\\\",\\\"params\\\":{\\\"protocolVersion\\\":1}}\"\n  DELETE conn=\"conn\"\n  GET conn=\"conn\" sess=\"\" accept=\"text/event-stream\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"result\\\":{\\\"protocolVersion\\\":1}}\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"method\\\":\\\"n/a\\\"}\"\n  RESULT Err Internal error: \"connection SSE stream ended: UTF8 error: invalid utf-8 sequence of 1 bytes from index 0\"\n",
	},
	{
		name:    `utf8-incomplete-closed`,
		lines:   []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`},
		streams: map[string]probeStream{``: {200, "data: {\"jsonrpc\":\"2.0\",\"method\":\"n/a\"}\x0a\x0adata: \xe2\x82", true}},
		want:    "  POST conn=\"\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"method\\\":\\\"initialize\\\",\\\"params\\\":{\\\"protocolVersion\\\":1}}\"\n  DELETE conn=\"conn\"\n  GET conn=\"conn\" sess=\"\" accept=\"text/event-stream\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"result\\\":{\\\"protocolVersion\\\":1}}\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"method\\\":\\\"n/a\\\"}\"\n  RESULT Err Internal error: \"connection SSE stream ended: UTF8 error: incomplete utf-8 byte sequence from index 0\"\n",
	},
	{
		name:    `utf8-surrogate-closed`,
		lines:   []string{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}`},
		streams: map[string]probeStream{``: {200, "data: x\xed\xa0\x80\x0a\x0a", true}},
		want:    "  POST conn=\"\" sess=\"\" ct=\"application/json\" accept=\"application/json\" body=\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"method\\\":\\\"initialize\\\",\\\"params\\\":{\\\"protocolVersion\\\":1}}\"\n  DELETE conn=\"conn\"\n  GET conn=\"conn\" sess=\"\" accept=\"text/event-stream\"\n  DELIVERED \"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"id\\\":1,\\\"result\\\":{\\\"protocolVersion\\\":1}}\"\n  RESULT Err Internal error: \"connection SSE stream ended: UTF8 error: invalid utf-8 sequence of 1 bytes from index 0\"\n",
	},
}

func debugString(s string) string { return serdejson.DebugString(s) }

func runClientScenario(t *testing.T, lines []string, streams map[string]probeStream) string {
	t.Helper()
	var mu sync.Mutex
	var log []string
	add := func(s string) { mu.Lock(); log = append(log, s); mu.Unlock() }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, sess := r.Header.Get(HeaderConnectionID), r.Header.Get(HeaderSessionID)
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			add(fmt.Sprintf("POST conn=%s sess=%s ct=%s accept=%s body=%s", debugString(conn), debugString(sess), debugString(r.Header.Get("Content-Type")), debugString(r.Header.Get("Accept")), debugString(string(body))))
			if conn == "" {
				var request struct {
					ID json.RawMessage `json:"id"`
				}
				_ = json.Unmarshal(body, &request)
				w.Header().Set("Content-Type", jsonMIME)
				w.Header().Set(HeaderConnectionID, "conn")
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(request.ID)+`,"result":{"protocolVersion":1}}`)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		case http.MethodGet:
			add(fmt.Sprintf("GET conn=%s sess=%s accept=%s", debugString(conn), debugString(sess), debugString(r.Header.Get("Accept"))))
			stream, ok := streams[sess]
			if !ok {
				stream = probeStream{status: http.StatusOK}
			}
			w.Header().Set("Content-Type", eventStreamMIME)
			w.WriteHeader(stream.status)
			_, _ = io.WriteString(w, stream.body)
			w.(http.Flusher).Flush()
			if !stream.close {
				<-r.Context().Done()
			}
		default:
			add(fmt.Sprintf("%s conn=%s", r.Method, debugString(conn)))
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	endpoint, err := normalizeEndpoint(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &httpTransport{endpoint: endpoint, http: server.Client(), ctx: ctx, cancel: cancel, feed: writer}
	var delivered []string
	var readErr error
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		lines := bufio.NewReader(reader)
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				if !errors.Is(err, io.EOF) {
					readErr = err
				}
				return
			}
			delivered = append(delivered, strings.TrimSuffix(line, "\n"))
		}
	}()
	var failure error
	for _, line := range lines {
		if _, err := transport.Write([]byte(line + "\n")); err != nil {
			failure = err
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if failure == nil {
		select {
		case <-readDone:
			failure = readErr
		default:
		}
	}
	result := "Ok after close"
	if failure != nil {
		kind := "Internal error"
		if errors.Is(failure, errFirstMessage) {
			kind = "Invalid request"
		}
		result = "Err " + kind + ": " + string(serdejson.Encode(stringValue(failure.Error())))
	}
	_ = transport.Close()
	<-readDone
	var out strings.Builder
	mu.Lock()
	var posts, others []string
	for _, entry := range log {
		if strings.HasPrefix(entry, "POST") {
			posts = append(posts, entry)
		} else {
			others = append(others, entry)
		}
	}
	mu.Unlock()
	sort.Strings(others)
	for _, entry := range append(posts, others...) {
		fmt.Fprintf(&out, "  %s\n", entry)
	}
	for _, line := range delivered {
		fmt.Fprintf(&out, "  DELIVERED %s\n", debugString(line))
	}
	fmt.Fprintf(&out, "  RESULT %s\n", result)
	return out.String()
}

// The client sends, delivers, and fails as the Rust client does: a frame
// that is not one JSON-RPC message is posted verbatim without bookkeeping, a
// message is posted serialized again, a first frame that is not initialize and
// a session method without a session fail with the Rust texts, a stream opens
// on any 2xx status and fails with "<scope> SSE stream ended: ..." when its
// status is not 2xx or it ends, and bytes that are not UTF-8 stall the stream
// and fail it with the UTF-8 error when it ends.
func TestHTTPClientMatchesRustClient(t *testing.T) {
	for _, scenario := range rustClientScenarios {
		t.Run(scenario.name, func(t *testing.T) {
			if got := runClientScenario(t, scenario.lines, scenario.streams); got != scenario.want {
				t.Errorf("got\n%s\nwant\n%s", got, scenario.want)
			}
		})
	}
}
