// Package clientrouter selects an explicit ACP v1 or draft-v2 client
// implementation and owns the agent connection required for v2-to-v1 fallback.
package clientrouter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"unicode/utf8"

	"github.com/BrokkAi/acp-go"
	schema1 "github.com/BrokkAi/acp-go/schema"
	schema2 "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
)

const maxFrame = 8 << 20

// V1Client runs an application client over an initialized-capable ACP v1
// transport. Serve must send initialize before any other ACP request.
type V1Client interface {
	RequestHandler() acp.Handler
	NotificationHandler() acp.Notifications
	Serve(context.Context, *acp.Connection) error
}

// V2Client is the draft-v2 counterpart to V1Client.
type V2Client interface {
	RequestHandler() acp.Handler
	NotificationHandler() acp.Notifications
	Serve(context.Context, *acpv2.Connection) error
}

// AgentConnection opens one bidirectional ACP agent transport. It is a factory
// because fallback from an incompatible v2 initialization may require a fresh
// connection.
type AgentConnection func() (io.ReadWriteCloser, error)

type Client struct {
	v1 func() V1Client
	v2 func() V2Client
}

func New() *Client { return &Client{} }

func (c *Client) WithV1(factory func() V1Client) *Client {
	c.v1 = factory
	return c
}

func (c *Client) WithV2(factory func() V2Client) *Client {
	c.v2 = factory
	return c
}

// Connect starts the highest configured client implementation. If a v2 agent
// selects v1 and a v1 client is configured, matching initialize parameters
// reuse the existing agent connection; non-matching parameters reconnect with
// v1. A v2 initialize rejection is surfaced and never silently retried as v1.
func (c *Client) Connect(ctx context.Context, openAgent AgentConnection) error {
	if c.v1 == nil && c.v2 == nil {
		return errors.New("client protocol router has no configured ACP protocol implementations")
	}
	if openAgent == nil {
		return errors.New("agent connection factory is required")
	}
	if c.v2 != nil {
		return c.connectV2(ctx, openAgent)
	}
	_, err := c.connectV1(ctx, openAgent)
	return err
}

type peerConnection struct {
	local *net.Conn
	peer  *net.Conn
}

func openPeer() (*peerConnection, error) {
	local, peer := net.Pipe()
	return &peerConnection{local: &local, peer: &peer}, nil
}

func (p *peerConnection) close() {
	_ = (*p.local).Close()
	_ = (*p.peer).Close()
}

type startedV1 struct {
	client     V1Client
	connection *acp.Connection
	reader     *bufio.Reader
	request    wireFrame
	serve      <-chan error
	peer       *peerConnection
}

func (s *startedV1) close() {
	_ = s.connection.Close()
	s.peer.close()
}

func (s *startedV1) closeClientOnly() {
	_ = s.connection.Close()
}

type startedV2 struct {
	client     V2Client
	connection *acpv2.Connection
	reader     *bufio.Reader
	request    wireFrame
	serve      <-chan error
	peer       *peerConnection
}

func (s *startedV2) close() {
	_ = s.connection.Close()
	s.peer.close()
}

func (s *startedV2) closeClientOnly() {
	_ = s.connection.Close()
}

func (c *Client) startV1(ctx context.Context) (*startedV1, error) {
	transport, err := openPeer()
	if err != nil {
		return nil, err
	}
	client := c.v1()
	if client == nil {
		transport.close()
		return nil, errors.New("v1 client factory returned nil")
	}
	connection := acp.Connect(*transport.local, *transport.local, client.RequestHandler(), client.NotificationHandler())
	done := make(chan error, 1)
	go func() { done <- client.Serve(ctx, connection) }()
	reader := bufio.NewReader(*transport.peer)
	request, err := readInitialize(reader)
	if err != nil {
		_ = connection.Close()
		transport.close()
		return nil, err
	}
	if err := validateV1Initialize(request); err != nil {
		_ = connection.Close()
		transport.close()
		return nil, err
	}
	return &startedV1{
		client: client, connection: connection, reader: reader,
		request: request, serve: done, peer: transport,
	}, nil
}

func (c *Client) startV2(ctx context.Context) (*startedV2, error) {
	transport, err := openPeer()
	if err != nil {
		return nil, err
	}
	client := c.v2()
	if client == nil {
		transport.close()
		return nil, errors.New("v2 client factory returned nil")
	}
	connection := acpv2.Connect(*transport.local, *transport.local, client.RequestHandler(), client.NotificationHandler())
	done := make(chan error, 1)
	go func() { done <- client.Serve(ctx, connection) }()
	reader := bufio.NewReader(*transport.peer)
	request, err := readInitialize(reader)
	if err != nil {
		_ = connection.Close()
		transport.close()
		return nil, err
	}
	if err := validateV2Initialize(request); err != nil {
		_ = connection.Close()
		transport.close()
		return nil, err
	}
	return &startedV2{
		client: client, connection: connection, reader: reader,
		request: request, serve: done, peer: transport,
	}, nil
}

func (c *Client) connectV1(ctx context.Context, openAgent AgentConnection) (*startedV1, error) {
	started, err := c.startV1(ctx)
	if err != nil {
		return nil, err
	}
	agent, err := openAgent()
	if err != nil {
		started.close()
		return nil, err
	}
	agentReader := bufio.NewReader(agent)
	response, err := exchangeInitialize(agentReader, agent, started.request)
	if err != nil {
		_ = agent.Close()
		started.close()
		return nil, err
	}
	if _, err := (*started.peer.peer).Write(append(response, '\n')); err != nil {
		_ = agent.Close()
		started.close()
		return nil, err
	}
	err = pipeUntilDone(ctx, started.serve, started.reader, *started.peer.peer, agentReader, agent, started.close)
	return nil, err
}

func (c *Client) connectV2(ctx context.Context, openAgent AgentConnection) error {
	started, err := c.startV2(ctx)
	if err != nil {
		return err
	}
	agent, err := openAgent()
	if err != nil {
		started.close()
		return err
	}
	agentReader := bufio.NewReader(agent)
	response, err := exchangeInitialize(agentReader, agent, started.request)
	if err != nil {
		_ = agent.Close()
		started.close()
		return err
	}

	version, responseVersion, responseErr := initializeResponseVersion(response)
	if responseErr != nil {
		// Give the v2 client the peer's rejection and surface its result. Do
		// not silently retry with v1.
		if _, err := (*started.peer.peer).Write(append(response, '\n')); err != nil {
			_ = agent.Close()
			started.close()
			return err
		}
		return pipeUntilDone(ctx, started.serve, started.reader, *started.peer.peer, agentReader, agent, started.close)
	}
	if version != 1 || c.v1 == nil {
		if _, err := (*started.peer.peer).Write(append(response, '\n')); err != nil {
			_ = agent.Close()
			started.close()
			return err
		}
		return pipeUntilDone(ctx, started.serve, started.reader, *started.peer.peer, agentReader, agent, started.close)
	}
	_ = responseVersion

	normalized, canReuse, normalizeErr := normalizeV2InitializeForV1(started.request.Params)
	fallback, fallbackErr := c.startV1(ctx)
	if fallbackErr != nil {
		_ = agent.Close()
		started.close()
		if normalizeErr != nil {
			return normalizeErr
		}
		return fallbackErr
	}
	if normalizeErr == nil && canReuse && jsonSemanticEqual(normalized, fallback.request.Params) {
		reusedResponse, err := responseWithID(response, fallback.request.ID)
		if err == nil {
			_, err = (*fallback.peer.peer).Write(append(reusedResponse, '\n'))
		}
		if err != nil {
			_ = agent.Close()
			fallback.close()
			started.close()
			return err
		}
		_ = started.connection.Close()
		started.closeClientOnly()
		<-started.serve
		return pipeUntilDone(ctx, fallback.serve, fallback.reader, *fallback.peer.peer, agentReader, agent, fallback.close)
	}

	// The parameters differ. Close both probes and both transports before
	// constructing a fresh v1 connection.
	started.close()
	<-started.serve
	fallback.close()
	<-fallback.serve
	_ = agent.Close()

	fresh, err := c.connectV1(ctx, openAgent)
	if fresh != nil {
		fresh.close()
	}
	return err
}

type wireFrame struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func readInitialize(reader *bufio.Reader) (wireFrame, error) {
	line, err := readLine(reader)
	if err != nil {
		return wireFrame{}, err
	}
	var frame wireFrame
	if err := json.Unmarshal(line, &frame); err != nil {
		return wireFrame{}, err
	}
	return frame, nil
}

func readLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(line) > maxFrame {
				return nil, errors.New("ACP frame exceeds 8 MiB")
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if len(line) == 0 {
			return nil, io.EOF
		}
		break
	}
	line = bytes.TrimSpace(line)
	if !utf8.Valid(line) {
		return nil, errors.New("ACP frame is not valid UTF-8")
	}
	return line, nil
}

func validateV1Initialize(frame wireFrame) error {
	if frame.Version != "2.0" || frame.Method != "initialize" || len(frame.ID) == 0 {
		return errors.New("ACP protocol version 1 client implementation must send initialize first")
	}
	var request schema1.InitializeRequest
	if err := json.Unmarshal(frame.Params, &request); err != nil {
		return err
	}
	if request.ProtocolVersion != acp.Version {
		return fmt.Errorf("ACP protocol version 1 client implementation requested version %d", request.ProtocolVersion)
	}
	return nil
}

func validateV2Initialize(frame wireFrame) error {
	if frame.Version != "2.0" || frame.Method != "initialize" || len(frame.ID) == 0 {
		return errors.New("ACP protocol version 2 client implementation must send initialize first")
	}
	var request schema2.InitializeRequest
	if err := json.Unmarshal(frame.Params, &request); err != nil {
		return err
	}
	if request.ProtocolVersion != acpv2.Version {
		return fmt.Errorf("ACP protocol version 2 client implementation requested version %d", request.ProtocolVersion)
	}
	return nil
}

func exchangeInitialize(reader *bufio.Reader, agent io.Writer, request wireFrame) ([]byte, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if _, err := agent.Write(append(encoded, '\n')); err != nil {
		return nil, err
	}
	return readLine(reader)
}

func initializeResponseVersion(response []byte) (uint16, uint16, error) {
	var envelope struct {
		Result *struct {
			ProtocolVersion uint16 `json:"protocolVersion"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return 0, 0, err
	}
	if envelope.Error != nil {
		return 0, 0, errors.New("initialize request rejected")
	}
	if envelope.Result == nil {
		return 0, 0, errors.New("agent closed before initialize response")
	}
	return envelope.Result.ProtocolVersion, envelope.Result.ProtocolVersion, nil
}

func responseWithID(response []byte, id json.RawMessage) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response, &envelope); err != nil {
		return nil, err
	}
	envelope["id"] = id
	return json.Marshal(envelope)
}

func normalizeV2InitializeForV1(params []byte) ([]byte, bool, error) {
	var raw any
	if err := json.Unmarshal(params, &raw); err != nil {
		return nil, false, err
	}
	var request schema2.InitializeRequest
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, false, err
	}
	// Unknown fields and noncanonical tolerant values make the request
	// non-lossless for connection reuse.
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, false, err
	}
	if !jsonSemanticEqual(encoded, params) {
		return nil, false, nil
	}
	v1Request, err := v1InitializeRequest(request)
	if err != nil {
		return nil, false, err
	}
	normalized, err := json.Marshal(v1Request)
	if err != nil {
		return nil, false, err
	}
	return normalized, true, nil
}

func v1InitializeRequest(request schema2.InitializeRequest) (schema1.InitializeRequest, error) {
	info := schema1.Implementation{
		Name:    request.Info.Name,
		Title:   nullableString(request.Info.Title),
		Version: request.Info.Version,
	}
	result := schema1.InitializeRequest{
		ClientInfo:      &info,
		ProtocolVersion: acp.Version,
	}
	if request.Capabilities != nil {
		capabilities := schema1.ClientCapabilities{}
		if request.Capabilities.Elicitation != nil {
			capabilities.Elicitation = &schema1.ElicitationCapabilities{
				Form: &schema1.ElicitationFormCapabilities{},
				URL:  &schema1.ElicitationUrlCapabilities{},
			}
		}
		if auth := request.Capabilities.Auth; auth != nil && auth.Terminal != nil {
			if auth.Terminal.Meta.Set {
				return schema1.InitializeRequest{}, errors.New("v2 terminal authentication metadata is not representable in v1")
			}
			terminal := true
			capabilities.Auth = &schema1.AuthCapabilities{Terminal: &terminal}
		}
		capabilities.Session = &schema1.ClientSessionCapabilities{
			ConfigOptions: &schema1.SessionConfigOptionsCapabilities{
				Boolean: &schema1.BooleanConfigOptionCapabilities{},
			},
		}
		result.ClientCapabilities = &capabilities
	}
	return result, nil
}

func nullableString(value schema2.Nullable[string]) *string {
	if !value.Set || value.Null {
		return nil
	}
	return &value.Value
}

func jsonSemanticEqual(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	encodedLeft, _ := json.Marshal(leftValue)
	encodedRight, _ := json.Marshal(rightValue)
	return bytes.Equal(encodedLeft, encodedRight)
}

func pipeUntilDone(ctx context.Context, serve <-chan error, clientReader io.Reader, clientWriter io.Writer, agentReader io.Reader, agentWriter io.WriteCloser, closePeer func()) error {
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			if closePeer != nil {
				closePeer()
			}
			_ = agentWriter.Close()
		})
	}
	defer closeAll()

	outbound := make(chan error, 1)
	inbound := make(chan error, 1)
	go func() { outbound <- copyFrames(agentWriter, clientReader) }()
	go func() { inbound <- copyFrames(clientWriter, agentReader) }()

	select {
	case err := <-serve:
		if err != nil {
			return err
		}
		return nil
	case err := <-outbound:
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			return err
		}
		return <-serve
	case err := <-inbound:
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			return err
		}
		return <-serve
	case <-ctx.Done():
		return ctx.Err()
	}
}

func copyFrames(dst io.Writer, src io.Reader) error {
	_, err := io.Copy(dst, src)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
