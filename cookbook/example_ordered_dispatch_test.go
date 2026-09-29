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

// applicationEvent is one entry in the application's single FIFO. Exactly one
// field is set. SDK callbacks only enqueue events; the consumer goroutine is
// the only code that touches application state.
type applicationEvent struct {
	Update  *acpv2.Update
	Resumed *resumeResult
	Closed  bool
}

type resumeResult struct {
	Response schema.ResumeSessionResponse
	Err      error
}

// observeSession resumes one session with replay and hands every event to
// apply, in order, on the calling goroutine, until the connection closes.
// apply must not wait for later traffic on this connection.
func observeSession(ctx context.Context, in io.ReadCloser, out io.WriteCloser, sessionID acpv2.SessionID, directory string, apply func(applicationEvent)) error {
	events := make(chan applicationEvent, 64)
	stopped := make(chan struct{})
	enqueue := func(event applicationEvent) {
		// Block for queue space rather than drop, but never outlive the
		// consumer. A production queue also needs an explicit memory policy.
		select {
		case events <- event:
		case <-stopped:
		}
	}

	connection := acpv2.Connect(in, out,
		acpv2.HandlePermissions(nil, cancelPermissionsV2{}),
		acpv2.SessionUpdates(func(update acpv2.Update) error {
			// This runs on the connection's reader in wire order. Enqueue and
			// return: do not call back into the connection or wait for apply.
			enqueue(applicationEvent{Update: &update})
			return nil
		}),
	)
	defer connection.Close()
	// Deferred calls run in reverse: release blocked callbacks before Close
	// joins the reader.
	defer close(stopped)
	go func() {
		// On EOF the reader delivers every notification before Done closes,
		// so Closed is queued behind all updates.
		<-connection.Done()
		enqueue(applicationEvent{Closed: true})
	}()

	initialization, err := connection.InitializeWithInfo(ctx, acpv2.Capabilities{}, acpv2.ClientInfo{
		Name: "cookbook-observer", Version: "0.1.0",
	})
	if err != nil {
		return err
	}
	go func() {
		response, err := connection.ResumeSessionFromStart(ctx, initialization, sessionID, directory, nil)
		// Replay precedes the resume response on the wire, and every
		// notification before a response has been handled when the call
		// returns. This marker therefore lands behind the whole replay.
		enqueue(applicationEvent{Resumed: &resumeResult{Response: response, Err: err}})
	}()

	for event := range events {
		if event.Update != nil && event.Update.SessionID != sessionID {
			continue
		}
		apply(event)
		if event.Closed {
			return nil
		}
	}
	return nil
}

func Example_orderedApplicationDispatch() {
	ctx := context.Background()
	notes := acpv2.SessionID("release-notes")
	fixture := newHistoryAgent(map[schema.SessionId][]schema.SessionUpdate{
		notes: {
			agentText("m1", "Drafted the release notes."),
			agentText("m2", "Added the upgrade section."),
		},
	})
	link := acptest.NewPair()
	agentCtx, stopAgent := context.WithCancel(ctx)
	defer stopAgent()
	go agentv2.New(fixture).Serve(agentCtx, link.B, link.B)

	directory, err := os.Getwd()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	// The projection is application state: only apply touches it.
	projection := acpv2.NewUpdateProjection()
	replayed := 0
	err = observeSession(ctx, link.A, link.A, notes, directory, func(event applicationEvent) {
		switch {
		case event.Update != nil:
			projection.Apply(event.Update.Update)
			replayed++
		case event.Resumed != nil:
			if event.Resumed.Err != nil {
				fmt.Println("resume failed:", event.Resumed.Err)
			} else {
				// The response is exposed only now, after its replay was applied.
				fmt.Printf("resumed after %d replayed updates: %q\n", replayed, projection.Text())
			}
			// Either way this observer is done: the agent process exits, and
			// the client reads EOF next.
			stopAgent()
		case event.Closed:
			fmt.Println("connection closed")
		}
	})
	if err != nil {
		fmt.Println("error:", err)
	}
	// Output:
	// resumed after 2 replayed updates: "Drafted the release notes.Added the upgrade section."
	// connection closed
}
