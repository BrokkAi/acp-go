// Package acptest is the standard-library-only test harness for ACP clients,
// agents, and the protocol routers. It deliberately does not import the router
// packages, so router tests can use these fixtures without creating an import
// cycle.
//
// The harness covers three layers:
//
//   - Pipe/Endpoint: one in-memory duplex transport that any v1 or draft-v2
//     connection can use, with the release's newline-delimited framing.
//   - Serve/WriteRequest/WriteInitialize/ReadResponse: framing helpers for
//     tests that drive a router or agent over that transport.
//   - V1Agent/V2Agent/AgentHandler/ProxyFunc and Command/TestAgent: fake
//     implementations, including deterministic typed prompt commands.
package acptest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/agent"
	schema1 "github.com/BrokkAi/acp-go/schema"
	schema2 "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	agent2 "github.com/BrokkAi/acp-go/v2/agent"
)

// ServeFunc is the connection entry point shared by the agent, proxy, and
// client routers.
type ServeFunc func(context.Context, io.ReadCloser, io.WriteCloser) error

// Serve runs serve over one end of an in-memory pipe and returns the peer side
// as a buffered JSON-RPC stream. The router is stopped during test cleanup.
func Serve(t testing.TB, serve ServeFunc) *bufio.ReadWriter {
	t.Helper()
	local, peer := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = local.SetDeadline(deadline)
	_ = peer.SetDeadline(deadline)
	done := make(chan error, 1)
	go func() { done <- serve(context.Background(), local, local) }()
	t.Cleanup(func() {
		_ = peer.Close()
		_ = local.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("router did not stop")
		}
	})
	return bufio.NewReadWriter(bufio.NewReader(peer), bufio.NewWriter(peer))
}

// WriteRequest encodes and flushes one JSON-RPC request frame.
func WriteRequest(t testing.TB, rw *bufio.ReadWriter, id int, method string, params any) {
	t.Helper()
	encoded, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	frame := struct {
		Version string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}{Version: "2.0", ID: id, Method: method, Params: encoded}
	if err := json.NewEncoder(rw).Encode(frame); err != nil {
		t.Fatal(err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
}

// WriteInitialize is WriteRequest for the initialize method with request ID 1.
func WriteInitialize(t testing.TB, rw *bufio.ReadWriter, params any) {
	t.Helper()
	WriteRequest(t, rw, 1, schema1.InitializeMethodName, params)
}

// ReadResponse decodes one JSON-RPC frame from the router's peer stream.
func ReadResponse(t testing.TB, rw *bufio.ReadWriter, value any) {
	t.Helper()
	if err := json.NewDecoder(rw).Decode(value); err != nil {
		t.Fatal(err)
	}
}

// V1Agent is a minimal v1 agent.Agent for router fixtures. Requests, when
// non-nil, must be buffered: it receives every initialize request.
type V1Agent struct {
	Requests chan schema1.InitializeRequest
}

func (a *V1Agent) Initialize(_ context.Context, _ agent.Client, request schema1.InitializeRequest) (schema1.InitializeResponse, error) {
	if a.Requests != nil {
		a.Requests <- request
	}
	return schema1.InitializeResponse{
		ProtocolVersion: acp.Version,
		AgentInfo:       &schema1.Implementation{Name: "v1-router-agent", Version: "test"},
	}, nil
}

func (a *V1Agent) NewSession(context.Context, agent.Client, schema1.NewSessionRequest) (schema1.NewSessionResponse, error) {
	return schema1.NewSessionResponse{SessionID: "v1"}, nil
}

func (a *V1Agent) Prompt(context.Context, agent.Client, schema1.PromptRequest, agent.SessionUpdater) (schema1.PromptResponse, error) {
	return schema1.PromptResponse{StopReason: schema1.StopReasonEndTurn}, nil
}

// V2Agent is the draft-v2 counterpart to V1Agent. Requests has the same
// buffered requirement.
type V2Agent struct {
	Requests chan schema2.InitializeRequest
}

func (a *V2Agent) Initialize(_ context.Context, _ agent2.Client, request schema2.InitializeRequest) (schema2.InitializeResponse, error) {
	if a.Requests != nil {
		a.Requests <- request
	}
	return schema2.InitializeResponse{
		ProtocolVersion: acpv2.Version,
		Info:            schema2.Implementation{Name: "v2-router-agent", Version: "test"},
		Capabilities:    &schema2.AgentCapabilities{Session: &schema2.SessionCapabilities{}},
	}, nil
}

func (a *V2Agent) NewSession(context.Context, agent2.Client, schema2.NewSessionRequest) (schema2.NewSessionResponse, error) {
	return schema2.NewSessionResponse{SessionID: "v2"}, nil
}

func (a *V2Agent) Prompt(context.Context, agent2.Client, schema2.PromptRequest, agent2.SessionUpdater) (schema2.PromptResponse, error) {
	return schema2.PromptResponse{}, nil
}

func (a *V2Agent) ListSessions(context.Context, agent2.Client, schema2.ListSessionsRequest) (schema2.ListSessionsResponse, error) {
	return schema2.ListSessionsResponse{}, nil
}

func (a *V2Agent) ResumeSession(context.Context, agent2.Client, schema2.ResumeSessionRequest) (schema2.ResumeSessionResponse, error) {
	return schema2.ResumeSessionResponse{}, nil
}

func (a *V2Agent) CloseSession(context.Context, agent2.Client, schema2.CloseSessionRequest) (schema2.CloseSessionResponse, error) {
	return schema2.CloseSessionResponse{}, nil
}

func (a *V2Agent) CancelSession(schema2.CancelSessionNotification) error { return nil }

// AgentHandler returns a raw ACP handler that answers initialize and
// session/new for one protocol version. A v1 handler accepts version 1 and any
// newer request (which it canonicalizes to v1); a v2 handler accepts only
// version 2. The client router uses it as its fake agent.
func AgentHandler(protocol uint16) acp.Handler {
	return func(_ context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case schema1.InitializeMethodName:
			var params struct {
				ProtocolVersion uint16 `json:"protocolVersion"`
			}
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, &acp.RPCError{Code: -32602, Message: err.Error()}
			}
			if protocol == 1 {
				if params.ProtocolVersion < 1 {
					return nil, &acp.RPCError{Code: -32600, Message: fmt.Sprintf("unexpected protocol %d", params.ProtocolVersion)}
				}
				return schema1.InitializeResponse{
					ProtocolVersion: acp.Version,
					AgentInfo:       &schema1.Implementation{Name: "fixture-agent", Version: "1"},
				}, nil
			}
			if params.ProtocolVersion != protocol {
				return nil, &acp.RPCError{Code: -32600, Message: fmt.Sprintf("unexpected protocol %d", params.ProtocolVersion)}
			}
			return schema2.InitializeResponse{
				ProtocolVersion: acpv2.Version,
				Info:            schema2.Implementation{Name: "fixture-agent", Version: "1"},
				Capabilities:    &schema2.AgentCapabilities{Session: &schema2.SessionCapabilities{}},
			}, nil
		case schema1.SessionNewMethodName:
			if protocol == 1 {
				return schema1.NewSessionResponse{SessionID: "v1-session"}, nil
			}
			return schema2.NewSessionResponse{SessionID: "v2-session"}, nil
		default:
			return nil, &acp.RPCError{Code: -32601}
		}
	}
}

// ProxyFunc adapts a function to the proxy router's Serve signature.
type ProxyFunc func(context.Context, io.ReadCloser, io.WriteCloser) error

func (f ProxyFunc) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	return f(ctx, in, out)
}
