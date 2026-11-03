package unstable_test

import (
	"context"
	"testing"

	schema "github.com/BrokkAi/acp-go/schema/unstable"
	acpunstable "github.com/BrokkAi/acp-go/unstable"
)

// Only the session/load response marks the boundary: an earlier call send
// makes with the same context does not, and a second Restore while one is
// in progress still sends its request.
func TestSubagentTreeRestoreBoundaryIsTheRestoreResponse(t *testing.T) {
	tree := acpunstable.NewSubagentTree()
	connection, agentSide := connectPeer(t, tree.Notifications())
	observeWire(t, tree, `{"sessionId":"root","update":{"sessionUpdate":"subagent_update","sessionId":"child"}}`)

	done := make(chan error, 1)
	go func() {
		done <- tree.Restore(context.Background(), "root", true, func(ctx context.Context) error {
			if err := connection.Call(ctx, "_vendor/prepare", map[string]any{}, nil); err != nil {
				return err
			}
			return connection.Call(ctx, schema.SessionLoadMethodName, map[string]any{"sessionId": "root"}, nil)
		})
	}()
	prepare := agentSide.next(t)
	agentSide.send(t, `{"jsonrpc":"2.0","id":`+string(prepare["id"])+`,"result":{}}`)
	load := agentSide.next(t)
	if string(load["method"]) != `"session/load"` {
		t.Fatalf("second request = %v", load)
	}
	// Still replay: this capability is history.
	agentSide.send(t, update("root", `{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}}}`))
	agentSide.send(t, `{"jsonrpc":"2.0","id":`+string(load["id"])+`,"result":{}}`)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if child := mustSubagent(t, tree, "child"); child.CanCancel() {
		t.Fatal("a capability sent before the load response, after an earlier call's response, authorized cancellation")
	}

	first := make(chan error, 1)
	go func() {
		first <- tree.Restore(context.Background(), "root", false, func(ctx context.Context) error {
			return connection.Call(ctx, schema.SessionResumeMethodName, map[string]any{"sessionId": "root"}, nil)
		})
	}()
	firstResume := agentSide.next(t)
	second := make(chan error, 1)
	go func() {
		second <- tree.Restore(context.Background(), "root", false, func(ctx context.Context) error {
			return connection.Call(ctx, schema.SessionResumeMethodName, map[string]any{"sessionId": "root"}, nil)
		})
	}()
	secondResume := agentSide.next(t)
	if string(secondResume["method"]) != `"session/resume"` {
		t.Fatalf("a Restore in progress suppressed the next request: %v", secondResume)
	}
	agentSide.send(t, `{"jsonrpc":"2.0","id":`+string(firstResume["id"])+`,"result":{}}`)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	agentSide.send(t, update("root", `{"sessionUpdate":"subagent_update","sessionId":"child","capabilities":{"cancel":{}}}`))
	agentSide.send(t, `{"jsonrpc":"2.0","id":`+string(secondResume["id"])+`,"result":{}}`)
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if child := mustSubagent(t, tree, "child"); !child.CanCancel() {
		t.Fatalf("the latest attempt's current capability was lost: %+v", child)
	}
}
