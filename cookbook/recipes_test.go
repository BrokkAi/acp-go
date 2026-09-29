package cookbook_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/acptest"
	"github.com/BrokkAi/acp-go/agent"
	"github.com/BrokkAi/acp-go/proxyrouter"
	"github.com/BrokkAi/acp-go/schema"
	agentv2 "github.com/BrokkAi/acp-go/v2/agent"
)

// These tests pin the edge cases of the recipes that the examples' happy
// paths do not reach.

// cancellingConductor answers a permission request the way a client does when
// its user stops the turn: session/cancel first, then the cancelled outcome
// the protocol requires. It records the tool call statuses it receives.
type cancellingConductor struct {
	*testConductor
	mu       sync.Mutex
	statuses []schema.ToolCallStatus
}

func (c *cancellingConductor) fromProxy(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if method != schema.SessionRequestPermissionMethodName {
		return c.testConductor.fromProxy(ctx, method, params)
	}
	var request schema.RequestPermissionRequest
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, err
	}
	// A backlog of notifications the agent ignores makes the proxy's queue
	// busy, so an answer forwarded without a flush would overtake the cancel.
	for i := 0; i < 200; i++ {
		if err := c.proxy.Notify(ctx, "_cookbook/progress", map[string]int{"step": i}); err != nil {
			return nil, err
		}
	}
	if err := c.proxy.Notify(ctx, schema.SessionCancelMethodName, schema.CancelNotification{SessionID: request.SessionID}); err != nil {
		return nil, err
	}
	return schema.RequestPermissionResponse{Outcome: schema.RequestPermissionOutcome{
		Cancelled: &schema.RequestPermissionOutcomeCancelled{},
	}}, nil
}

func (c *cancellingConductor) proxyNotification(method string, params json.RawMessage) error {
	if method == schema.SessionUpdateMethodName {
		var update acp.Update
		if err := json.Unmarshal(params, &update); err != nil {
			return err
		}
		if change := update.Update.ToolCallUpdate; change != nil && change.Status != nil {
			c.mu.Lock()
			c.statuses = append(c.statuses, *change.Status)
			c.mu.Unlock()
		}
	}
	return c.testConductor.proxyNotification(method, params)
}

func TestProxyKeepsSessionCancelAheadOfPermissionAnswer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	proxyPipe := acptest.NewPairWithDeadline(deadline)
	agentPipe := acptest.NewPairWithDeadline(deadline)
	served := make(chan error, 1)
	go func() {
		served <- proxyrouter.New().WithV1(toolsProxy{
			server: acp.NewStdioMCPServer("docs", "/usr/local/bin/docs-mcp", nil, nil),
		}).Serve(ctx, proxyPipe.B, proxyPipe.B)
	}()
	go agent.New(&releaseAgent{}).Serve(ctx, agentPipe.B, agentPipe.B)
	conductor := &cancellingConductor{testConductor: &testConductor{}}
	conductor.agent = agentPipe.A.Connect(conductor.fromAgent, conductor.agentNotification)
	conductor.proxy = proxyPipe.A.Connect(conductor.fromProxy, conductor.proxyNotification)
	defer conductor.agent.Close()

	if err := conductor.proxy.Call(ctx, proxyInitializeMethod, schema.InitializeRequest{
		ProtocolVersion: acp.Version,
		ClientInfo:      &acp.ClientInfo{Name: "test", Version: "1"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	var session schema.NewSessionResponse
	if err := conductor.proxy.Call(ctx, schema.SessionNewMethodName, schema.NewSessionRequest{
		Cwd: t.TempDir(), MCPServers: []schema.McpServer{},
	}, &session); err != nil {
		t.Fatal(err)
	}
	var turn schema.PromptResponse
	if err := conductor.proxy.Call(ctx, schema.SessionPromptMethodName, schema.PromptRequest{
		SessionID: session.SessionID,
		Prompt:    []schema.ContentBlock{acp.NewTextContent("Cut the release.")},
	}, &turn); err != nil {
		t.Fatal(err)
	}
	// The agent sees session/cancel before the cancelled permission outcome,
	// so it ends the turn as cancelled rather than skipping the tool.
	if turn.StopReason != schema.StopReasonCancelled {
		t.Fatalf("stop reason = %s, want cancelled", turn.StopReason)
	}
	conductor.mu.Lock()
	statuses := append([]schema.ToolCallStatus(nil), conductor.statuses...)
	conductor.mu.Unlock()
	if len(statuses) != 1 || statuses[0] != schema.ToolCallStatusFailed {
		t.Fatalf("tool call statuses = %v, want [failed]", statuses)
	}

	// Closing the conductor's side is a normal shutdown for the proxy.
	_ = conductor.proxy.Close()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve after EOF = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("proxy did not stop")
	}
}

func TestProxyServeReportsProtocolErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	pipe := acptest.NewPairWithDeadline(deadline)
	served := make(chan error, 1)
	go func() { served <- (toolsProxy{}).Serve(ctx, pipe.B, pipe.B) }()
	conductor := pipe.A.Connect(nil, nil)
	defer conductor.Close()

	// The proxy stops as soon as it reads the frame, so the sender may see
	// EOF before its own write completes.
	if err := conductor.Notify(ctx, proxySuccessorMethod, "not an envelope"); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if err == nil || !strings.Contains(err.Error(), "invalid _proxy/successor envelope") {
			t.Fatalf("Serve = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("proxy did not stop")
	}
}

func TestAddMCPServerPreservesFieldsAndRejectsNonObjects(t *testing.T) {
	server := acp.NewStdioMCPServer("docs", "/usr/local/bin/docs-mcp", nil, nil)
	for _, params := range []string{`null`, `[]`, `"session"`} {
		if _, err := addMCPServer(json.RawMessage(params), server); err == nil {
			t.Errorf("addMCPServer(%s) succeeded", params)
		}
	}
	for _, params := range []string{
		`{"cwd":"/work","future":true}`,
		`{"cwd":"/work","future":true,"mcpServers":null}`,
		`{"cwd":"/work","future":true,"mcpServers":[{"type":"http","name":"tickets","url":"https://example.com","headers":[]}]}`,
	} {
		encoded, err := addMCPServer(json.RawMessage(params), server)
		if err != nil {
			t.Fatalf("addMCPServer(%s) = %v", params, err)
		}
		var request map[string]json.RawMessage
		if err := json.Unmarshal(encoded, &request); err != nil {
			t.Fatal(err)
		}
		if string(request["future"]) != "true" {
			t.Errorf("unmodeled field dropped: %s", encoded)
		}
		var servers []schema.McpServer
		if err := json.Unmarshal(request["mcpServers"], &servers); err != nil {
			t.Fatal(err)
		}
		if last := servers[len(servers)-1]; last.Stdio == nil || last.Stdio.Name != "docs" {
			t.Errorf("mcpServers = %s", request["mcpServers"])
		}
	}
}

func TestFixtureAgentProcessIsAbsolute(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		command, _ := fixtureAgentProcess(version)
		if !filepath.IsAbs(command[0]) {
			t.Errorf("%s fixture command %q is relative", version, command[0])
		}
	}
}

func TestObserveSessionReportsResumeFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	link := acptest.NewPairWithDeadline(deadline)
	agentCtx, stopAgent := context.WithCancel(ctx)
	defer stopAgent()
	go agentv2.New(newHistoryAgent(nil)).Serve(agentCtx, link.B, link.B)

	var resumeErr error
	closed := false
	err := observeSession(ctx, link.A, link.A, "missing-session", t.TempDir(), func(event applicationEvent) {
		switch {
		case event.Resumed != nil:
			resumeErr = event.Resumed.Err
			stopAgent()
		case event.Closed:
			closed = true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if resumeErr == nil || !strings.Contains(resumeErr.Error(), "unknown session") {
		t.Fatalf("resume error = %v", resumeErr)
	}
	if !closed {
		t.Fatal("observer returned before the connection closed")
	}
}
