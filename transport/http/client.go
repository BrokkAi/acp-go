package acphttp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/jsonline"
	"github.com/BrokkAi/acp-go/internal/serdejson"
)

// Client connects one ACP connection to a draft Streamable HTTP endpoint. The
// zero value targets Endpoint with http.DefaultClient.
type Client struct {
	Endpoint string
	HTTP     *http.Client
}

// Dial opens one ACP connection over the draft HTTP binding. The returned
// connection uses the standard transport framing, so callers drive it with the
// usual Initialize, NewSession, and Prompt methods; the HTTP specifics stay
// behind this package. A base URL targets the standard /acp endpoint: an empty
// path becomes /acp, and other paths gain /acp unless they already end in it,
// matching the Rust client. options configure the connection as in
// acp.Connect, for example acp.WithErrorHandler or acp.WithProtocolVersion.
func Dial(ctx context.Context, endpoint string, h acp.Handler, n acp.Notifications, options ...acp.ConnectOption) (*acp.Connection, error) {
	return Client{Endpoint: endpoint}.Dial(ctx, h, n, options...)
}

func (c Client) Dial(ctx context.Context, h acp.Handler, n acp.Notifications, options ...acp.ConnectOption) (*acp.Connection, error) {
	if strings.TrimSpace(c.Endpoint) == "" {
		return nil, errors.New("ACP HTTP endpoint is required")
	}
	endpoint, err := normalizeEndpoint(c.Endpoint)
	if err != nil {
		return nil, err
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithCancel(ctx)
	reader, writer := io.Pipe()
	transport := &httpTransport{endpoint: endpoint, http: client, ctx: ctx, cancel: cancel, feed: writer}
	return acp.Connect(reader, transport, h, n, options...), nil
}

func normalizeEndpoint(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid ACP HTTP endpoint: %w", err)
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case path == "":
		parsed.Path = DefaultPath
	case strings.HasSuffix(path, DefaultPath):
		parsed.Path = path
	default:
		parsed.Path = path + DefaultPath
	}
	return parsed.String(), nil
}

// deleteTimeout bounds the DELETE that Close sends to tear the server
// connection down.
const deleteTimeout = 5 * time.Second

// httpTransport adapts the ACP stream framing to HTTP: writes become POST
// bodies, and agent frames arrive on event streams. Like the Rust client, it
// opens the connection stream after initialize, and a session stream before
// it sends the first message naming a session and when a session/new or
// session/fork result names a new one.
type httpTransport struct {
	endpoint string
	http     *http.Client
	ctx      context.Context
	cancel   context.CancelFunc
	feed     *io.PipeWriter

	mu           sync.Mutex
	connectionID string
	closed       bool
	// failure is the error that ended the transport, such as a stream that
	// failed; later frames fail with it, as the Rust client's transport has
	// stopped.
	failure error
	// streams holds every event stream opened or being opened, keyed by
	// streamKey.
	streams map[string]*eventStream
	// opening holds the ids of session/new and session/fork requests whose
	// results name a session to open.
	opening map[string]struct{}
}

// eventStream is one event stream; ready closes once its GET has an answer.
type eventStream struct {
	ready chan struct{}
	err   error
}

func streamKey(sessionID string, scoped bool) string {
	if !scoped {
		return "connection"
	}
	return "session:" + sessionID
}

// Write sends one frame the connection wrote, a line, as the Rust client
// sends a frame: a JSON-RPC message serialized again, with the session header
// and stream bookkeeping, and anything else verbatim with neither. The first
// frame must be an initialize request. Errors carry the Rust client's texts.
func (t *httpTransport) Write(p []byte) (int, error) {
	raw := bytes.TrimSuffix(p, []byte("\n"))
	parsed, isMessage := parseMessage(raw)
	t.mu.Lock()
	connectionID, closed, failure := t.connectionID, t.closed, t.failure
	t.mu.Unlock()
	switch {
	case closed:
		return 0, io.ErrClosedPipe
	case failure != nil:
		return 0, failure
	}
	if connectionID == "" {
		if !isMessage || !parsed.isInitialize() {
			return 0, errFirstMessage
		}
		// The Rust client posts the message it holds, serialized again.
		if err := t.initialize(parsed.encode()); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	if !isMessage {
		// A frame that is not one JSON-RPC message is posted verbatim, with
		// no session header and no stream bookkeeping.
		if err := t.post(raw, connectionID, "", false); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	payload := parsed.encode()
	sessionID, scoped := parsed.sessionID()
	if !scoped && parsed.hasMethod() && requiresSessionHeader(parsed.Method) {
		return 0, fmt.Errorf("POST: method `%s` requires sessionId in params", parsed.Method)
	}
	if scoped {
		// Open the session's stream first, so it sees everything the agent
		// sends for the session, including a session/load replay.
		if err := t.ensureStream(connectionID, sessionID, true); err != nil {
			return 0, err
		}
		// A stream that failed while this one opened ends the transport
		// before the frame is posted, as the Rust client's start_sse checks
		// stream failures first. A stream that fails after this check is not
		// seen until the next frame, where Rust may still see it first.
		t.mu.Lock()
		failure := t.failure
		t.mu.Unlock()
		if failure != nil {
			return 0, failure
		}
	}
	openingKey, opening := "", false
	if parsed.isRequest() && effectFor(parsed.Method) == effectOpens {
		if openingKey, opening = idKey(parsed.ID); opening {
			t.mu.Lock()
			if t.opening == nil {
				t.opening = make(map[string]struct{})
			}
			t.opening[openingKey] = struct{}{}
			t.mu.Unlock()
		}
	}
	if err := t.post(payload, connectionID, sessionID, scoped); err != nil {
		// The request never reached the agent, so no result will open a
		// session for it, as the Rust client drops its pending request.
		if opening {
			t.mu.Lock()
			delete(t.opening, openingKey)
			t.mu.Unlock()
		}
		return 0, err
	}
	return len(p), nil
}

// errFirstMessage is the Rust client's error for a first frame that is not an
// initialize request.
var errFirstMessage = errors.New("ACP HTTP transport: first message must be `initialize`")

// post sends one frame after initialize. Like the Rust client, it accepts
// 202 and any other 2xx status, and fails with "POST: " and the reason. A
// scoped frame carries Acp-Session-Id, even when the session id is empty.
func (t *httpTransport) post(payload []byte, connectionID, sessionID string, scoped bool) error {
	request, err := t.request(http.MethodPost, payload, connectionID, sessionID, scoped)
	if err != nil {
		return fmt.Errorf("POST: %w", err)
	}
	response, err := t.http.Do(request)
	if err != nil {
		return fmt.Errorf("POST: %w", err)
	}
	defer response.Body.Close()
	if !successful(response.StatusCode) {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("POST: HTTP %s: %s", statusText(response.StatusCode), body)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return nil
}

func successful(status int) bool { return status >= 200 && status < 300 }

// initialize sends the initialize request and handles its inline response as
// the Rust client does, failing with the Rust client's messages. The
// connection id is stored as soon as the response carries one, so a failure
// afterwards tears the server connection down with DELETE. An error response
// rejects the connection whatever the headers say: it is delivered to the
// caller, the connection is deleted, and the next frame must be initialize
// again. Its own failures start with "initialize: ", as the Rust client's do.
func (t *httpTransport) initialize(payload []byte) error {
	request, err := t.request(http.MethodPost, payload, "", "", false)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	response, err := t.http.Do(request)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	defer response.Body.Close()
	connectionID, hasConnection := headerValue(response.Header, HeaderConnectionID)
	if hasConnection {
		t.mu.Lock()
		t.connectionID = connectionID
		t.mu.Unlock()
	}
	fail := func(err error) error {
		t.abandon()
		return fmt.Errorf("initialize: %w", err)
	}
	body, err := io.ReadAll(response.Body)
	if !successful(response.StatusCode) {
		return fail(fmt.Errorf("HTTP %s: %s", statusText(response.StatusCode), body))
	}
	if err != nil {
		return fail(err)
	}
	// A malformed body, a batch array included, is never an initialize
	// response, as in the Rust client.
	answer, ok := parseMessage(body)
	if !ok {
		return fail(errors.New("invalid initialize response: " + malformedError(body)))
	}
	if answer.isResponse() && answer.Error != nil {
		// A rejected initialize opens no connection.
		err := t.push(answer.encode())
		t.abandon()
		return err
	}
	if !hasConnection {
		return errors.New("initialize: server did not return acp-connection-id header")
	}
	if err := t.push(answer.encode()); err != nil {
		return err
	}
	return t.ensureStream(connectionID, "", false)
}

// malformedError is how the Rust SDK displays the error of a frame it does
// not read as a JSON-RPC message: a parse error carrying the frame for
// invalid JSON, the batch error for an array, and an invalid request
// otherwise.
func malformedError(frame []byte) string {
	value, err := serdejson.Parse(frame)
	switch {
	case err != nil:
		return "Parse error: {\n  \"line\": " + string(serdejson.Encode(stringValue(string(frame)))) + "\n}"
	case value.Kind == serdejson.Array:
		return "Invalid request: " + string(serdejson.Encode(stringValue(errBatch.Error())))
	default:
		return "Invalid request"
	}
}

// statusText is how reqwest displays a status code: the code and its
// canonical reason.
func statusText(code int) string {
	reason := http.StatusText(code)
	if reason == "" {
		reason = "<unknown status code>"
	}
	return fmt.Sprintf("%d %s", code, reason)
}

// abandon deletes the server connection an initialize response named and
// forgets it, as the Rust client closes its connection after a failed or
// rejected initialize.
func (t *httpTransport) abandon() {
	t.mu.Lock()
	connectionID := t.connectionID
	t.connectionID = ""
	t.mu.Unlock()
	if connectionID != "" {
		t.delete(connectionID)
	}
}

// request builds a request to the endpoint. A scoped request carries
// Acp-Session-Id with sessionID, which may be empty, as the Rust client sends
// the header for every session-scoped message and stream.
func (t *httpTransport) request(method string, payload []byte, connectionID, sessionID string, scoped bool) (*http.Request, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(t.ctx, method, t.endpoint, body)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		request.Header.Set("Content-Type", jsonMIME)
		request.Header.Set("Accept", jsonMIME)
	}
	if connectionID != "" {
		request.Header.Set(HeaderConnectionID, connectionID)
	}
	if scoped {
		request.Header.Set(HeaderSessionID, sessionID)
	}
	return request, nil
}

// claim registers a stream before it is opened, so concurrent openers share
// one GET; a second subscriber to a stream would get 409.
func (t *httpTransport) claim(sessionID string, scoped bool) (*eventStream, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, false, io.ErrClosedPipe
	}
	if t.streams == nil {
		t.streams = make(map[string]*eventStream)
	}
	key := streamKey(sessionID, scoped)
	if stream, ok := t.streams[key]; ok {
		return stream, false, nil
	}
	stream := &eventStream{ready: make(chan struct{})}
	t.streams[key] = stream
	return stream, true, nil
}

// ensureStream opens a stream unless it is open or opening, and waits until
// the server has answered its GET.
func (t *httpTransport) ensureStream(connectionID, sessionID string, scoped bool) error {
	stream, created, err := t.claim(sessionID, scoped)
	if err != nil {
		return err
	}
	if created {
		t.establish(stream, connectionID, sessionID, scoped)
	}
	select {
	case <-stream.ready:
		return stream.err
	case <-t.ctx.Done():
		return t.ctx.Err()
	}
}

// establish sends a claimed stream's GET and starts reading it. A stream that
// cannot open fails the transport, as in the Rust client.
func (t *httpTransport) establish(stream *eventStream, connectionID, sessionID string, scoped bool) {
	response, err := t.openStream(connectionID, sessionID, scoped)
	stream.err = err
	close(stream.ready)
	if err != nil {
		t.fail(err)
		return
	}
	go t.readStream(connectionID, streamScope(sessionID, scoped), response)
}

// streamScope names a stream in its errors as the Rust client does: by its
// session id, or "connection".
func streamScope(sessionID string, scoped bool) string {
	if scoped {
		return sessionID
	}
	return "connection"
}

// streamEnded is the Rust client's error for a stream that failed or ended.
func streamEnded(scope string, reason string) error {
	return fmt.Errorf("%s SSE stream ended: %s", scope, reason)
}

func (t *httpTransport) openStream(connectionID, sessionID string, scoped bool) (*http.Response, error) {
	scope := streamScope(sessionID, scoped)
	request, err := t.request(http.MethodGet, nil, connectionID, sessionID, scoped)
	if err != nil {
		return nil, streamEnded(scope, err.Error())
	}
	request.Header.Set("Accept", eventStreamMIME)
	response, err := t.http.Do(request)
	if err != nil {
		return nil, streamEnded(scope, err.Error())
	}
	// Like the Rust client, any 2xx status opens the stream.
	if !successful(response.StatusCode) {
		_ = response.Body.Close()
		return nil, streamEnded(scope, "HTTP "+statusText(response.StatusCode))
	}
	return response, nil
}

// readStream reads an event stream as the Rust client's eventsource-stream
// parser does and hands each event's data to the connection's read loop. Lines
// end at "\r\n", "\r", or "\n"; a leading byte order mark is skipped; a
// "data" field appends its value, without one leading space, and a line feed;
// a blank line dispatches the data without its last line feed. An event with
// empty data is skipped, and an event the stream ends in the middle of is
// lost. The body is read as the parser's UTF-8 stream reads it (see
// utf8Stream). The end of any stream fails the transport with the Rust
// client's error.
func (t *httpTransport) readStream(connectionID, scope string, response *http.Response) {
	defer response.Body.Close()
	scanner := bufio.NewScanner(&utf8Stream{r: response.Body})
	scanner.Buffer(make([]byte, 0, 64*1024), math.MaxInt32)
	scanner.Split(scanSSELine)
	var data bytes.Buffer
	first := true
	for scanner.Scan() {
		line := scanner.Bytes()
		if first {
			line = bytes.TrimPrefix(line, []byte("\uFEFF"))
			first = false
		}
		switch {
		case len(line) == 0:
			if payload := bytes.TrimSuffix(data.Bytes(), []byte("\n")); len(payload) > 0 {
				if t.deliver(connectionID, append([]byte(nil), payload...)) != nil {
					return
				}
			}
			data.Reset()
		case line[0] == ':':
		default:
			name, value, _ := bytes.Cut(line, []byte(":"))
			if string(name) == "data" {
				data.Write(bytes.TrimPrefix(value, []byte(" ")))
				data.WriteByte('\n')
			}
		}
	}
	var reason string
	var invalid *utf8StreamError
	switch err := scanner.Err(); {
	case err == nil:
		reason = "SSE stream closed"
	case errors.As(err, &invalid):
		reason = "UTF8 error: " + invalid.text
	default:
		reason = "Transport error: " + err.Error()
	}
	t.fail(streamEnded(scope, reason))
}

// utf8StreamError is the UTF-8 error the Rust parser reports at the end of a
// stream that held bytes it could not decode.
type utf8StreamError struct{ text string }

func (e *utf8StreamError) Error() string { return e.text }

// utf8Stream reads an event stream as eventsource-stream's Utf8Stream does.
// Bytes pass through up to the first sequence that is not valid UTF-8 so far;
// that sequence and everything after it are held back. A sequence that a later
// read completes passes on, so an invalid one stalls the stream: nothing after
// it is ever parsed. When the body ends with bytes held back, the stream fails
// with Rust's FromUtf8Error text for them.
type utf8Stream struct {
	r     io.Reader
	held  []byte
	ready []byte
	// stuck records an invalid sequence; only its first bytes are kept, which
	// is all the error text depends on.
	stuck bool
	err   error
}

func (u *utf8Stream) Read(p []byte) (int, error) {
	for len(u.ready) == 0 {
		if u.err != nil {
			return 0, u.err
		}
		buf := make([]byte, 32*1024)
		n, err := u.r.Read(buf)
		if !u.stuck {
			u.held = append(u.held, buf[:n]...)
			valid := validUTF8Prefix(u.held)
			u.ready = u.held[:valid]
			u.held = append([]byte(nil), u.held[valid:]...)
			if len(u.held) > 0 && utf8Error(u.held) != "" && !incompleteUTF8(u.held) {
				u.stuck = true
			}
		} else if len(u.held) < utf8.UTFMax {
			u.held = append(u.held, buf[:min(n, utf8.UTFMax-len(u.held))]...)
		}
		if err != nil {
			u.err = err
			if errors.Is(err, io.EOF) && len(u.held) > 0 {
				u.err = &utf8StreamError{text: utf8Error(u.held)}
			}
		}
	}
	n := copy(p, u.ready)
	u.ready = u.ready[n:]
	return n, nil
}

// validUTF8Prefix is the length of the longest valid UTF-8 prefix of data.
func validUTF8Prefix(data []byte) int {
	i := 0
	for i < len(data) {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size <= 1 {
			return i
		}
		i += size
	}
	return i
}

// incompleteUTF8 reports whether data, starting at a sequence that is not
// valid, is only a sequence cut short by the end of data.
func incompleteUTF8(data []byte) bool {
	return strings.HasPrefix(utf8Error(data), "incomplete")
}

// scanSSELine splits an event stream into lines ending at "\r\n", "\r", or
// "\n". A "\r" at the end of the data read so far waits for the next byte, and
// what is left when the stream ends, a "\r" included, is dropped, as the
// streaming parser does.
func scanSSELine(data []byte, atEOF bool) (int, []byte, error) {
	i := bytes.IndexAny(data, "\r\n")
	switch {
	case i < 0:
		return 0, nil, nil
	case data[i] == '\n':
		return i + 1, data[:i], nil
	case i+1 < len(data):
		if data[i+1] == '\n' {
			return i + 2, data[:i], nil
		}
		return i + 1, data[:i], nil
	default:
		return 0, nil, nil
	}
}

// deliver hands an agent frame to the connection, re-serialized when it is a
// JSON-RPC message, as the Rust client holds it, and made one line otherwise.
// A session/new or session/fork result naming a new session claims that
// session's stream before the result is delivered, so a message the caller
// then sends for the session waits for the stream instead of opening a second
// one.
func (t *httpTransport) deliver(connectionID string, payload []byte) error {
	parsed, isMessage := parseMessage(payload)
	switch {
	case !isMessage && bytes.IndexByte(payload, '\n') >= 0 && !jsonline.Valid(payload):
		// Event data of several lines that is not JSON cannot reach the
		// connection as one line, so the transport sends the parse error the
		// connection would answer it with, as the Rust client's connection
		// answers the malformed frame.
		_, err := t.Write(jsonline.ParseError(payload))
		return err
	case !isMessage:
		return t.push(lineValue(payload))
	}
	var opened *eventStream
	sessionID, ok := t.sessionOpenedBy(parsed)
	if ok {
		if stream, created, err := t.claim(sessionID, true); err == nil && created {
			opened = stream
		}
	}
	if err := t.push(parsed.encode()); err != nil {
		return err
	}
	if opened != nil {
		go t.establish(opened, connectionID, sessionID, true)
	}
	return nil
}

// sessionOpenedBy reads the session a result names when it answers a
// session/new or session/fork request.
func (t *httpTransport) sessionOpenedBy(parsed frame) (string, bool) {
	if !parsed.isResponse() {
		return "", false
	}
	key, ok := idKey(parsed.ID)
	if !ok {
		return "", false
	}
	t.mu.Lock()
	_, opening := t.opening[key]
	delete(t.opening, key)
	t.mu.Unlock()
	if !opening || parsed.Error != nil {
		return "", false
	}
	return stringMember(parsed.Result, "sessionId")
}

// push writes one frame, which is one line, to the connection's read loop.
func (t *httpTransport) push(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	_, err := t.feed.Write(append(append([]byte(nil), payload...), '\n'))
	return err
}

func (t *httpTransport) fail(err error) {
	t.mu.Lock()
	closed := t.closed
	if t.failure == nil {
		t.failure = err
	}
	t.mu.Unlock()
	if !closed {
		_ = t.feed.CloseWithError(err)
	}
}

// Close tears the server connection down with DELETE, as the Rust client
// does, then stops every stream. A failed DELETE is ignored.
func (t *httpTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	connectionID := t.connectionID
	t.mu.Unlock()
	if connectionID != "" {
		t.delete(connectionID)
	}
	t.cancel()
	return t.feed.Close()
}

func (t *httpTransport) delete(connectionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.endpoint, nil)
	if err != nil {
		return
	}
	request.Header.Set(HeaderConnectionID, connectionID)
	response, err := t.http.Do(request)
	if err != nil {
		return
	}
	_ = response.Body.Close()
}
