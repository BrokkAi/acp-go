package cookbook_test

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/BrokkAi/acp-go/acptest"
	schema "github.com/BrokkAi/acp-go/schema/v2"
	acpv2 "github.com/BrokkAi/acp-go/v2"
	agentv2 "github.com/BrokkAi/acp-go/v2/agent"
)

// askAgentV2 sends one draft-v2 prompt. A v2 session/prompt response only
// acknowledges that the agent inserted the user message; the answer is
// complete at the session's next idle update after it reports running.
func askAgentV2(ctx context.Context, in io.ReadCloser, out io.WriteCloser, directory, prompt string) (acpv2.WorkResult, error) {
	// Install the tracker and permission host when the connection is created:
	// v2 updates and interactive requests may arrive before setup responses.
	tracker := acpv2.NewSessionTracker()
	permissions := acpv2.NewCancellablePermissions(cancelPermissionsV2{})
	connection := acpv2.Connect(in, out,
		acpv2.HandlePermissions(nil, permissions),
		tracker.TrackerNotifications(nil),
	)
	defer connection.Close()

	initialization, err := connection.InitializeWithInfo(ctx, acpv2.Capabilities{}, acpv2.ClientInfo{
		Name: "cookbook-client", Version: "0.1.0",
	})
	if err != nil {
		return acpv2.WorkResult{}, err
	}
	session, err := connection.NewSessionWithOptions(ctx, initialization, directory, acpv2.NewSessionOptions{})
	if err != nil {
		return acpv2.WorkResult{}, err
	}
	handle, err := acpv2.NewSessionHandle(connection, initialization, session, acpv2.SessionHandleOptions{
		Tracker:     tracker,
		Permissions: permissions,
	})
	if err != nil {
		return acpv2.WorkResult{}, err
	}
	// Prompt resets the session's work projection before sending, so updates
	// queued from earlier work cannot complete this turn.
	if _, err := handle.Prompt(ctx, prompt); err != nil {
		return acpv2.WorkResult{}, err
	}
	result, err := handle.WaitForIdle(ctx)
	if err != nil {
		return acpv2.WorkResult{}, err
	}
	// A v2 session stays open on the agent until the client closes it.
	return result, handle.Close(ctx)
}

// cancelPermissionsV2 refuses every permission request with the explicit
// cancelled outcome.
type cancelPermissionsV2 struct{}

func (cancelPermissionsV2) RequestPermission(context.Context, schema.RequestPermissionRequest) (schema.RequestPermissionResponse, error) {
	return acpv2.CancelPermission(), nil
}

func Example_v2OneShotPrompt() {
	ctx := context.Background()
	link := acptest.NewPair()
	go agentv2.New(newHistoryAgent(nil)).Serve(ctx, link.B, link.B)

	directory, err := os.Getwd()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	result, err := askAgentV2(ctx, link.A, link.A, directory, "Hello over draft v2.")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(result.Text)
	fmt.Println("stop reason:", *result.StopReason)
	// Output:
	// Echo: Hello over draft v2.
	// stop reason: end_turn
}
