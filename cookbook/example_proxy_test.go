package cookbook_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/proxyrouter"
	"github.com/BrokkAi/acp-go/schema"
	schemav2 "github.com/BrokkAi/acp-go/schema/v2"
)

const (
	proxyInitializeMethod = "_proxy/initialize"
	proxySuccessorMethod  = "_proxy/successor"
)

// successorMessage is the conductor's envelope for traffic between a proxy and
// its successor. The pinned schema has no _proxy/* methods, so acp-go does not
// generate it; this mirrors SuccessorMessage in the Rust SDK 2.2.0. Like the
// reference conductor, the proxy sets no envelope _meta and does not forward
// it; the inner params, including their own _meta, pass through unchanged.
type successorMessage struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Meta   json.RawMessage `json:"_meta,omitempty"`
}

// toolsProxy is a stable-v1 proxy component that adds one MCP server to every
// session/new and session/load and forwards all other traffic unchanged. Its
// Serve method satisfies proxyrouter.Proxy.
type toolsProxy struct {
	server schema.McpServer
}

func (p toolsProxy) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	link := &proxyLink{
		server:      p.server,
		ready:       make(chan struct{}),
		toClient:    newNotifyQueue(),
		toSuccessor: newNotifyQueue(),
	}
	link.connection = acp.Connect(in, out, link.request, link.notification)
	close(link.ready)
	defer link.connection.Close()
	go link.toClient.run(ctx, link.connection)
	go link.toSuccessor.run(ctx, link.connection)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-link.connection.Done():
		// End of input is a normal shutdown; anything else is reported.
		if err := link.connection.Err(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			return err
		}
		return nil
	}
}

// proxyLink is one conductor connection. The conductor delivers client traffic
// unwrapped and successor traffic inside _proxy/successor envelopes; the proxy
// answers in kind.
type proxyLink struct {
	server      schema.McpServer
	connection  *acp.Connection
	ready       chan struct{}
	toClient    *notifyQueue
	toSuccessor *notifyQueue
}

func (l *proxyLink) request(ctx context.Context, method string, params json.RawMessage) (any, error) {
	select {
	case <-l.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	switch method {
	case proxyInitializeMethod:
		// The conductor initializes a proxy with _proxy/initialize. The proxy
		// initializes its successor with a plain initialize, which the
		// conductor turns back into _proxy/initialize if the successor is
		// itself a proxy.
		return l.forwardToSuccessor(ctx, schema.InitializeMethodName, params)
	case proxySuccessorMethod:
		// A successor request, such as session/request_permission, goes to
		// the client unchanged, behind the updates that preceded it.
		var message successorMessage
		if err := json.Unmarshal(params, &message); err != nil {
			return nil, &acp.RPCError{Code: -32602, Message: "invalid _proxy/successor envelope"}
		}
		l.toClient.flush(ctx)
		var result json.RawMessage
		err := l.connection.Call(ctx, message.Method, message.Params, &result)
		// Client notifications sent before the client's answer, such as a
		// session/cancel before a cancelled permission outcome, stay ahead of
		// it on the way back to the successor.
		l.toSuccessor.flush(ctx)
		if err != nil {
			return nil, err
		}
		return result, nil
	case schema.SessionNewMethodName, schema.SessionLoadMethodName:
		withServer, err := addMCPServer(params, l.server)
		if err != nil {
			return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
		}
		return l.forwardToSuccessor(ctx, method, withServer)
	default:
		return l.forwardToSuccessor(ctx, method, params)
	}
}

func (l *proxyLink) forwardToSuccessor(ctx context.Context, method string, params json.RawMessage) (any, error) {
	// Client notifications queued before this request stay ahead of it.
	l.toSuccessor.flush(ctx)
	var result json.RawMessage
	err := l.connection.Call(ctx, proxySuccessorMethod, successorMessage{Method: method, Params: params}, &result)
	// Updates the successor sent before its response were queued while Call
	// waited. Deliver them first: a v1 client expects every session/update of
	// a prompt turn before the turn's response.
	l.toClient.flush(ctx)
	if err != nil {
		// An *acp.RPCError keeps the successor's code, message, and data.
		return nil, err
	}
	return result, nil
}

// notification runs on the connection's reader, so it only queues: a
// notification callback must not write to its own connection.
func (l *proxyLink) notification(method string, params json.RawMessage) error {
	if method == proxySuccessorMethod {
		var message successorMessage
		if err := json.Unmarshal(params, &message); err != nil {
			return fmt.Errorf("invalid _proxy/successor envelope: %w", err)
		}
		l.toClient.push(queuedNotification{method: message.Method, params: message.Params})
		return nil
	}
	// Client notifications, such as session/cancel, go to the successor.
	l.toSuccessor.push(queuedNotification{
		method: proxySuccessorMethod,
		params: successorMessage{Method: method, Params: params},
	})
	return nil
}

// addMCPServer appends server to a session setup request's mcpServers and
// preserves every other field, including ones this proxy does not model.
func addMCPServer(params json.RawMessage, server schema.McpServer) (json.RawMessage, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, errors.New("session setup params must be a JSON object")
	}
	var servers []json.RawMessage
	if raw, ok := request["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(server)
	if err != nil {
		return nil, err
	}
	if request["mcpServers"], err = json.Marshal(append(servers, encoded)); err != nil {
		return nil, err
	}
	return json.Marshal(request)
}

// notifyQueue sends notifications from its own goroutine in the order they
// were queued. flush is the barrier that keeps queued notifications ahead of
// a response or request forwarded afterwards.
type notifyQueue struct {
	items chan queuedNotification
	done  chan struct{}
}

type queuedNotification struct {
	method  string
	params  any
	flushed chan struct{} // set only for a flush marker
}

func newNotifyQueue() *notifyQueue {
	return &notifyQueue{items: make(chan queuedNotification, 64), done: make(chan struct{})}
}

func (q *notifyQueue) run(ctx context.Context, connection *acp.Connection) {
	defer close(q.done)
	for {
		select {
		case item := <-q.items:
			if item.flushed != nil {
				close(item.flushed)
				continue
			}
			// A failed send means the connection is already failing and has
			// recorded its own error.
			_ = connection.Notify(ctx, item.method, item.params)
		case <-ctx.Done():
			return
		}
	}
}

// push waits for queue space, which applies backpressure to the reader.
func (q *notifyQueue) push(item queuedNotification) bool {
	select {
	case q.items <- item:
		return true
	case <-q.done:
		return false
	}
}

// flush returns once every notification queued before it has been sent.
func (q *notifyQueue) flush(ctx context.Context) {
	flushed := make(chan struct{})
	if !q.push(queuedNotification{flushed: flushed}) {
		return
	}
	select {
	case <-flushed:
	case <-ctx.Done():
	case <-q.done:
	}
}

// testConductor stands in for the reference conductor so this recipe runs
// under go test: it plays the client toward the proxy and relays the proxy's
// _proxy/successor traffic to and from one agent. It is test scaffolding, not
// a conductor; see #24.
type testConductor struct {
	proxy *acp.Connection
	agent *acp.Connection

	mu   sync.Mutex
	text strings.Builder
}

func (c *testConductor) fromProxy(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if method != proxySuccessorMethod {
		return nil, &acp.RPCError{Code: -32601, Message: "client method not supported: " + method}
	}
	var message successorMessage
	if err := json.Unmarshal(params, &message); err != nil {
		return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
	}
	var result json.RawMessage
	if err := c.agent.Call(ctx, message.Method, message.Params, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *testConductor) proxyNotification(method string, params json.RawMessage) error {
	if method == proxySuccessorMethod {
		var message successorMessage
		if err := json.Unmarshal(params, &message); err != nil {
			return err
		}
		return c.agent.Notify(context.Background(), message.Method, message.Params)
	}
	return acp.SessionUpdates(func(update acp.Update) error {
		if chunk := update.Update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.text.WriteString(chunk.Content.Text.Text)
		}
		return nil
	})(method, params)
}

func (c *testConductor) fromAgent(ctx context.Context, method string, params json.RawMessage) (any, error) {
	var result json.RawMessage
	if err := c.proxy.Call(ctx, proxySuccessorMethod, successorMessage{Method: method, Params: params}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// agentNotification wraps agent updates synchronously, so they reach the
// proxy before the agent's response is relayed.
func (c *testConductor) agentNotification(method string, params json.RawMessage) error {
	return c.proxy.Notify(context.Background(), proxySuccessorMethod, successorMessage{Method: method, Params: params})
}

// toolsRecordingAgent is the test agent, reporting the MCP servers each
// session/new declares.
type toolsRecordingAgent struct {
	acptest.TestAgent
	declared chan []schema.McpServer
}

func (a *toolsRecordingAgent) NewSession(ctx context.Context, client agent.Client, request schema.NewSessionRequest) (schema.NewSessionResponse, error) {
	a.declared <- request.MCPServers
	return a.TestAgent.NewSession(ctx, client, request)
}

func Example_proxyComponent() {
	ctx := context.Background()
	// The router hands a connection to the proxy only for an exact v1
	// _proxy/initialize; it never downgrades or converts versions.
	router := proxyrouter.New().WithV1(toolsProxy{
		server: acp.NewStdioMCPServer("docs", "/usr/local/bin/docs-mcp", []string{"--stdio"}, nil),
	})

	proxyPipe := acptest.NewPair()
	go router.Serve(ctx, proxyPipe.B, proxyPipe.B)
	agentPipe := acptest.NewPair()
	recorder := &toolsRecordingAgent{declared: make(chan []schema.McpServer, 1)}
	go agent.New(recorder).Serve(ctx, agentPipe.B, agentPipe.B)
	conductor := &testConductor{}
	conductor.agent = agentPipe.A.Connect(conductor.fromAgent, conductor.agentNotification)
	conductor.proxy = proxyPipe.A.Connect(conductor.fromProxy, conductor.proxyNotification)
	defer conductor.agent.Close()
	defer conductor.proxy.Close()

	directory, err := os.Getwd()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	var initialized schema.InitializeResponse
	if err := conductor.proxy.Call(ctx, proxyInitializeMethod, schema.InitializeRequest{
		ProtocolVersion: acp.Version,
		ClientInfo:      &acp.ClientInfo{Name: "cookbook-conductor", Version: "0.1.0"},
	}, &initialized); err != nil {
		fmt.Println("error:", err)
		return
	}
	var session schema.NewSessionResponse
	if err := conductor.proxy.Call(ctx, schema.SessionNewMethodName, schema.NewSessionRequest{
		Cwd: directory, MCPServers: []schema.McpServer{},
	}, &session); err != nil {
		fmt.Println("error:", err)
		return
	}
	for _, server := range <-recorder.declared {
		fmt.Println("agent received MCP server:", server.Stdio.Name)
	}
	var turn schema.PromptResponse
	if err := conductor.proxy.Call(ctx, schema.SessionPromptMethodName, schema.PromptRequest{
		SessionID: session.SessionID,
		Prompt:    []schema.ContentBlock{acp.NewTextContent("Hello through the proxy.")},
	}, &turn); err != nil {
		fmt.Println("error:", err)
		return
	}
	conductor.mu.Lock()
	fmt.Printf("client received %q, then %s\n", conductor.text.String(), turn.StopReason)
	conductor.mu.Unlock()

	// A v2 conductor gets an explicit rejection from this v1-only router.
	v2Pipe := acptest.NewPair()
	go router.Serve(ctx, v2Pipe.B, v2Pipe.B)
	v2Conductor := v2Pipe.A.Connect(nil, nil)
	defer v2Conductor.Close()
	err = v2Conductor.Call(ctx, proxyInitializeMethod, schemav2.InitializeRequest{
		ProtocolVersion: 2,
		Info:            schemav2.Implementation{Name: "cookbook-conductor", Version: "0.1.0"},
	}, nil)
	fmt.Println("v2 conductor:", err)
	// Output:
	// agent received MCP server: docs
	// client received "Hello through the proxy.", then end_turn
	// v2 conductor: ACP error -32600: ACP protocol version 2 is not configured
}
