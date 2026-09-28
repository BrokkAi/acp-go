package acphttp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/BrokkAi/acp-go"
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
// matching the Rust client.
func Dial(ctx context.Context, endpoint string, h acp.Handler, n acp.Notifications) (*acp.Connection, error) {
	return Client{Endpoint: endpoint}.Dial(ctx, h, n)
}

func (c Client) Dial(ctx context.Context, h acp.Handler, n acp.Notifications) (*acp.Connection, error) {
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
	return acp.Connect(reader, transport, h, n), nil
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

// httpTransport adapts the ACP stream framing to HTTP: writes become POST
// bodies, and the server-to-client stream is a single event-stream request.
type httpTransport struct {
	endpoint string
	http     *http.Client
	ctx      context.Context
	cancel   context.CancelFunc
	feed     *io.PipeWriter

	mu           sync.Mutex
	connectionID string
	streaming    bool
	closed       bool
}

func (t *httpTransport) Write(p []byte) (int, error) {
	payload := bytes.TrimSpace(p)
	if len(payload) == 0 {
		return len(p), nil
	}
	parsed, err := parseFrame(payload)
	if err != nil {
		return 0, err
	}
	t.mu.Lock()
	connectionID, closed := t.connectionID, t.closed
	t.mu.Unlock()
	if closed {
		return 0, io.ErrClosedPipe
	}
	if connectionID == "" {
		if !parsed.isInitialize() {
			return 0, errors.New("the first ACP HTTP frame must be initialize")
		}
		if err := t.initialize(payload); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	request, err := t.post(payload, connectionID, parsed.sessionID())
	if err != nil {
		return 0, err
	}
	response, err := t.http.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxFrame))
	if err != nil {
		return 0, err
	}
	if response.StatusCode != http.StatusAccepted {
		return 0, fmt.Errorf("ACP HTTP POST returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return len(p), nil
}

func (t *httpTransport) initialize(payload []byte) error {
	request, err := t.post(payload, "", "")
	if err != nil {
		return err
	}
	response, err := t.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxFrame))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("ACP HTTP initialize returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	connectionID := strings.TrimSpace(response.Header.Get(HeaderConnectionID))
	if connectionID == "" {
		return errors.New("ACP HTTP initialize response is missing the connection id header")
	}
	t.mu.Lock()
	t.connectionID = connectionID
	t.mu.Unlock()
	if err := t.push(body); err != nil {
		return err
	}
	t.startStream(connectionID)
	return nil
}

func (t *httpTransport) post(payload []byte, connectionID, sessionID string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(t.ctx, http.MethodPost, t.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", jsonMIME)
	if connectionID != "" {
		request.Header.Set(HeaderConnectionID, connectionID)
	}
	if sessionID != "" {
		request.Header.Set(HeaderSessionID, sessionID)
	}
	return request, nil
}

func (t *httpTransport) startStream(connectionID string) {
	t.mu.Lock()
	if t.streaming || t.closed {
		t.mu.Unlock()
		return
	}
	t.streaming = true
	t.mu.Unlock()
	go t.readStream(connectionID)
}

// readStream turns event-stream data lines into newline-delimited frames for
// the connection's read loop.
func (t *httpTransport) readStream(connectionID string) {
	request, err := http.NewRequestWithContext(t.ctx, http.MethodGet, t.endpoint, nil)
	if err != nil {
		t.fail(err)
		return
	}
	request.Header.Set("Accept", eventStreamMIME)
	request.Header.Set(HeaderConnectionID, connectionID)
	response, err := t.http.Do(request)
	if err != nil {
		t.fail(err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.fail(fmt.Errorf("ACP HTTP stream returned %s", response.Status))
		return
	}
	reader := bufio.NewReader(response.Body)
	var event bytes.Buffer
	for {
		line, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimRight(line, "\r\n")
		switch {
		case len(trimmed) == 0:
			if event.Len() > 0 {
				payload := append([]byte(nil), event.Bytes()...)
				event.Reset()
				if perr := t.push(payload); perr != nil {
					return
				}
			}
		case bytes.HasPrefix(trimmed, []byte("data:")):
			event.Write(bytes.TrimSpace(trimmed[len("data:"):]))
		}
		if err != nil {
			if len(bytes.TrimSpace(event.Bytes())) > 0 {
				_ = t.push(bytes.TrimSpace(event.Bytes()))
			}
			t.fail(err)
			return
		}
	}
}

func (t *httpTransport) push(payload []byte) error {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil
	}
	_, err := t.feed.Write(append(payload, '\n'))
	return err
}

func (t *httpTransport) fail(err error) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if !closed {
		_ = t.feed.CloseWithError(err)
	}
}

func (t *httpTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	t.cancel()
	return t.feed.Close()
}
