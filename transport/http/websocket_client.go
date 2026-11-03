package acphttp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/internal/jsonline"
)

// dialWebSocket opens the WebSocket binding as the Rust client does: the
// handshake is a GET of the http or https form of the endpoint, sent over
// HTTP/1.1 without following redirects, and the 101 response is checked
// before any ACP message is sent. The binding negotiates no subprotocol or
// extension, so a response that selects one is refused. A failed handshake
// is returned with the Rust client's text, "WebSocket connect failed: " and
// the reason. ctx bounds the handshake and the connection, as it bounds the
// HTTP binding's requests.
func dialWebSocket(ctx context.Context, endpoint string, base *http.Client, h acp.Handler, n acp.Notifications, options ...acp.ConnectOption) (*acp.Connection, error) {
	ws, err := websocketHandshake(ctx, endpoint, base)
	if err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	transport := &wsTransport{ws: ws, feed: writer}
	transport.stopWatch = context.AfterFunc(ctx, func() {
		transport.fail(ctx.Err())
	})
	go transport.read()
	return acp.Connect(reader, transport, h, n, options...), nil
}

func websocketConnectFailed(reason string) error {
	return errors.New("WebSocket connect failed: " + reason)
}

func websocketHandshake(ctx context.Context, endpoint string, base *http.Client) (*wsConn, error) {
	target, err := url.Parse(endpoint)
	if err != nil {
		return nil, websocketConnectFailed(err.Error())
	}
	switch target.Scheme {
	case "ws":
		target.Scheme = "http"
	case "wss":
		target.Scheme = "https"
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, websocketConnectFailed(err.Error())
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	client, owned, timeout := websocketHTTPClient(base)
	// The client's timeout covers the handshake only, as reqwest's request
	// timeout covers the opening handshake and not the upgraded socket.
	handshakeCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		handshakeCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(handshakeCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, websocketConnectFailed(err.Error())
	}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", key)
	// reqwest's defaults: Accept */*, no User-Agent, and no compression.
	request.Header.Set("Accept", "*/*")
	request.Header.Set("User-Agent", "")
	response, err := client.Do(request)
	if err != nil {
		return nil, websocketConnectFailed(err.Error())
	}
	// A refused handshake drops its connection, as the Rust client drops
	// the reqwest client it built for the handshake.
	refuse := func(reason string) error {
		_ = response.Body.Close()
		if owned != nil {
			owned.CloseIdleConnections()
		}
		return websocketConnectFailed(reason)
	}
	if err := validateWebSocketResponse(response, wsAccept(key)); err != nil {
		return nil, refuse(err.Error())
	}
	socket, ok := response.Body.(io.ReadWriteCloser)
	if !ok {
		return nil, refuse("the HTTP client did not hand over the upgraded connection")
	}
	return newWSConn(socket, bufio.NewReader(socket), wsClientRole), nil
}

// websocketHTTPClient is base set up for the handshake as the Rust client
// sets up reqwest after the caller's configuration: no redirects, so a
// redirect fails at the original endpoint, and HTTP/1.1 only, for a
// standard Transport, which is cloned, set not to request compression, and
// returned as owned. Other round trippers are used as they are, and a
// response over another HTTP version fails the handshake. The client's
// Timeout is returned for the handshake alone: as an http.Client timeout it
// would also bound the upgraded socket's lifetime.
func websocketHTTPClient(base *http.Client) (*http.Client, *http.Transport, time.Duration) {
	client := *base
	timeout := client.Timeout
	client.Timeout = 0
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	roundTripper := client.Transport
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	var owned *http.Transport
	if transport, ok := roundTripper.(*http.Transport); ok {
		owned = transport.Clone()
		protocols := new(http.Protocols)
		protocols.SetHTTP1(true)
		owned.Protocols = protocols
		owned.DisableCompression = true
		roundTripper = owned
	}
	client.Transport = roundTripper
	return &client, owned, timeout
}

// validateWebSocketResponse checks the handshake response as the Rust client
// does, in the same order and with the same reasons.
func validateWebSocketResponse(response *http.Response, accept string) error {
	if response.ProtoMajor != 1 || response.ProtoMinor != 1 {
		return fmt.Errorf("expected HTTP/1.1, received %s; preconfigured TLS must use HTTP/1.1 ALPN", httpVersionDebug(response.ProtoMajor, response.ProtoMinor))
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		return errors.New("unexpected status " + statusText(response.StatusCode))
	}
	if upgrades := response.Header.Values("Upgrade"); len(upgrades) != 1 || !asciiEqualFold(upgrades[0], "websocket") {
		return errors.New("invalid upgrade header")
	}
	if !connectionUpgrade(response.Header.Values("Connection")) {
		return errors.New("invalid connection header")
	}
	if accepts := response.Header.Values("Sec-Websocket-Accept"); len(accepts) != 1 || accepts[0] != accept {
		return errors.New("invalid Sec-WebSocket-Accept")
	}
	for _, name := range []string{"sec-websocket-protocol", "sec-websocket-extensions"} {
		if _, ok := response.Header[http.CanonicalHeaderKey(name)]; ok {
			return errors.New("unsupported " + name)
		}
	}
	return nil
}

// connectionUpgrade reports whether a Connection header holds the token
// "upgrade", in any case, in a comma-separated list. A value the http crate
// cannot read as a string is skipped.
func connectionUpgrade(values []string) bool {
	for _, value := range values {
		if _, ok := headerValue(http.Header{"V": {value}}, "V"); !ok {
			continue
		}
		for _, part := range strings.Split(value, ",") {
			if asciiEqualFold(strings.Trim(part, " \t"), "upgrade") {
				return true
			}
		}
	}
	return false
}

// httpVersionDebug is how the http crate debug-prints a Version.
func httpVersionDebug(major, minor int) string {
	switch {
	case major == 0 && minor == 9:
		return "HTTP/0.9"
	case major == 1 && minor == 0:
		return "HTTP/1.0"
	case major == 2:
		return "HTTP/2.0"
	case major == 3:
		return "HTTP/3.0"
	default:
		return fmt.Sprintf("HTTP/%d.%d", major, minor)
	}
}

// wsTransport adapts the ACP stream framing to one WebSocket, as the Rust
// client's WebSocket driver does: every frame the connection writes is one
// text frame, a JSON-RPC message serialized again and anything else
// verbatim, and every text frame received is one frame for the connection.
// Binary frames and pongs are ignored and pings answered. A close frame from
// the server, a protocol violation, or the socket ending fails the
// transport with the Rust client's error, and later writes fail with it.
// Close sends a close frame without a payload and closes the socket without
// waiting for an answer.
type wsTransport struct {
	ws        *wsConn
	feed      *io.PipeWriter
	stopWatch func() bool

	mu      sync.Mutex
	closed  bool
	failure error
}

func (t *wsTransport) Write(p []byte) (int, error) {
	t.mu.Lock()
	closed, failure := t.closed, t.failure
	t.mu.Unlock()
	switch {
	case closed:
		return 0, io.ErrClosedPipe
	case failure != nil:
		return 0, failure
	}
	payload := bytes.TrimSuffix(p, []byte("\n"))
	if parsed, ok := parseMessage(payload); ok {
		payload = parsed.encode()
	}
	if err := t.ws.writeMessage(opText, payload); err != nil {
		err = errors.New("ws send: " + err.Error())
		t.fail(err)
		return 0, err
	}
	return len(p), nil
}

// read hands every text frame to the connection until the socket fails.
func (t *wsTransport) read() {
	for {
		message, err := t.ws.readMessage()
		if err != nil {
			t.fail(errors.New("ws recv: " + err.Error()))
			return
		}
		switch message.kind {
		case wsClose:
			t.fail(errors.New("WebSocket closed by peer: " + closeFrameDebug(message.close)))
			return
		case wsText:
			if err := t.deliver(message.payload); err != nil {
				return
			}
		}
	}
}

// deliver hands an agent frame to the connection as the HTTP binding's
// client does: re-serialized when it is a JSON-RPC message and made one line
// otherwise. A frame of several lines that is not JSON cannot reach the
// connection as one line, so the transport sends the parse error the
// connection would answer it with.
func (t *wsTransport) deliver(payload []byte) error {
	parsed, isMessage := parseMessage(payload)
	switch {
	case isMessage:
		payload = parsed.encode()
	case bytes.IndexByte(payload, '\n') >= 0 && !jsonline.Valid(payload):
		_, err := t.Write(jsonline.ParseError(payload))
		return err
	default:
		payload = lineValue(payload)
	}
	_, err := t.feed.Write(append(append([]byte(nil), payload...), '\n'))
	return err
}

// fail ends the transport with err, the first failure winning, and closes
// the socket, as the Rust client drops it.
func (t *wsTransport) fail(err error) {
	t.mu.Lock()
	closed := t.closed
	if t.failure == nil {
		t.failure = err
	}
	err = t.failure
	t.mu.Unlock()
	_ = t.ws.Close()
	if !closed {
		_ = t.feed.CloseWithError(err)
	}
}

// Close sends a close frame without a payload, as the Rust client does when
// its connection finishes, and closes the socket.
func (t *wsTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	failure := t.failure
	t.mu.Unlock()
	t.stopWatch()
	if failure == nil {
		_ = t.ws.writeMessage(opClose, nil)
	}
	_ = t.ws.Close()
	return t.feed.Close()
}
