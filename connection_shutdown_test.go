package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BrokkAi/acp-go/internal/rpcctl"
)

// shutdownPeer is the far end of a connection over two io.Pipes. A write to
// the connection's output blocks until the peer reads it, so a test decides
// when queued frames are written.
type shutdownPeer struct {
	t          *testing.T
	connection *Connection
	output     *bufio.Reader // what the connection wrote
	outputPipe *io.PipeReader
	input      *io.PipeWriter // what the connection reads
}

func newShutdownPeer(t *testing.T, h Handler, options ...ConnectOption) *shutdownPeer {
	t.Helper()
	expectNoLeakedGoroutines(t)
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	p := &shutdownPeer{t: t, output: bufio.NewReader(outReader), outputPipe: outReader, input: inWriter}
	p.connection = Connect(inReader, outWriter, h, nil, options...)
	t.Cleanup(func() {
		_ = p.connection.Close()
		_ = inWriter.Close()
		_ = outReader.Close()
	})
	return p
}

// readAll reads every line the connection writes until its output closes.
func (p *shutdownPeer) readAll() []string {
	p.t.Helper()
	var lines []string
	for {
		line, err := p.output.ReadString('\n')
		if err == io.EOF && line == "" {
			return lines
		}
		if err != nil {
			p.t.Fatalf("read connection output: %v", err)
		}
		lines = append(lines, strings.TrimSuffix(line, "\n"))
	}
}

func (p *shutdownPeer) readLine() string {
	p.t.Helper()
	line, err := p.output.ReadString('\n')
	if err != nil {
		p.t.Fatalf("read connection output: %v", err)
	}
	return strings.TrimSuffix(line, "\n")
}

func (p *shutdownPeer) send(line string) {
	p.t.Helper()
	if _, err := io.WriteString(p.input, line+"\n"); err != nil {
		p.t.Fatal(err)
	}
}

// queue queues count notifications without waiting for them to be written,
// returning a function that waits for each.
func (p *shutdownPeer) queue(count int) []func(context.Context) error {
	p.t.Helper()
	waits := make([]func(context.Context) error, count)
	for i := range waits {
		wait, err := p.connection.queueNotification(context.Background(), "test/n", map[string]int{"i": i})
		if err != nil {
			p.t.Fatalf("queue notification %d: %v", i, err)
		}
		waits[i] = wait
	}
	return waits
}

func notificationLines(count int) []string {
	lines := make([]string, count)
	for i := range lines {
		lines[i] = fmt.Sprintf(`{"jsonrpc":"2.0","method":"test/n","params":{"i":%d}}`, i)
	}
	return lines
}

// waitUntil polls condition for up to five seconds.
func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// expectNoLeakedGoroutines fails the test if goroutines it started are still
// running shortly after it ends.
func expectNoLeakedGoroutines(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for runtime.NumGoroutine() > before {
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<20)
				t.Errorf("%d goroutines leaked:\n%s", runtime.NumGoroutine()-before, buf[:runtime.Stack(buf, true)])
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
}

func shutdownOptions() map[string][]ConnectOption {
	passthrough := func(string, json.RawMessage, bool) (json.RawMessage, error) { return nil, nil }
	return map[string][]ConnectOption{
		"unguarded": nil,
		"guarded":   {rpcctl.WithOutgoingGuard(passthrough).(ConnectOption)},
	}
}

func TestShutdownWritesQueuedFramesThenClosesOutput(t *testing.T) {
	for name, options := range shutdownOptions() {
		t.Run(name, func(t *testing.T) {
			p := newShutdownPeer(t, nil, options...)
			waits := p.queue(5)
			done := make(chan error, 1)
			go func() { done <- p.connection.Shutdown(context.Background()) }()
			waitUntil(t, "shutdown to begin", p.connection.shuttingDown.Load)
			if got, want := strings.Join(p.readAll(), "\n"), strings.Join(notificationLines(5), "\n"); got != want {
				t.Fatalf("peer received:\n%s\nwant:\n%s", got, want)
			}
			if err := <-done; err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			for i, wait := range waits {
				if err := wait(context.Background()); err != nil {
					t.Fatalf("notification %d: %v", i, err)
				}
			}
			if !closedChannel(p.connection.Done()) {
				t.Fatal("Done is open after Shutdown")
			}
			if err := p.connection.Err(); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("Err = %v, want io.ErrClosedPipe", err)
			}
			if err := p.connection.Notify(context.Background(), "test/after", nil); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("Notify after Shutdown = %v, want io.ErrClosedPipe", err)
			}
		})
	}
}

func TestCloseDropsQueuedFrames(t *testing.T) {
	p := newShutdownPeer(t, nil)
	waits := p.queue(3)
	// The write loop holds the first frame in a blocked Write.
	waitUntil(t, "the first frame to reach the writer", func() bool { return len(p.connection.writes) == 2 })
	start := time.Now()
	if err := p.connection.Close(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v", elapsed)
	}
	if lines := p.readAll(); len(lines) != 0 {
		t.Fatalf("peer received %q after Close", lines)
	}
	for i, wait := range waits {
		if err := wait(context.Background()); err == nil {
			t.Fatalf("notification %d reported written", i)
		}
	}
}

func TestShutdownIsBoundedByContext(t *testing.T) {
	p := newShutdownPeer(t, nil)
	waits := p.queue(3)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	// Nobody reads, so the writer stays blocked until the deadline aborts it.
	if err := p.connection.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Shutdown took %v past a 50ms deadline", elapsed)
	}
	if lines := p.readAll(); len(lines) != 0 {
		t.Fatalf("peer received %q after an aborted Shutdown", lines)
	}
	for i, wait := range waits {
		if err := wait(context.Background()); err == nil {
			t.Fatalf("notification %d reported written", i)
		}
	}
}

func TestShutdownWithEndedContextAborts(t *testing.T) {
	p := newShutdownPeer(t, nil)
	p.queue(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.connection.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown = %v, want context.Canceled", err)
	}
	if lines := p.readAll(); len(lines) != 0 {
		t.Fatalf("peer received %q", lines)
	}
}

func TestCloseAbortsShutdownDrain(t *testing.T) {
	p := newShutdownPeer(t, nil)
	p.queue(2)
	done := make(chan error, 1)
	go func() { done <- p.connection.Shutdown(context.Background()) }()
	waitUntil(t, "shutdown to begin", p.connection.shuttingDown.Load)
	if err := p.connection.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("Shutdown = %v, want io.ErrClosedPipe", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return after Close")
	}
}

func TestShutdownSendsNothingNew(t *testing.T) {
	handlerStarted := make(chan struct{})
	handlerDone := make(chan error, 1)
	handler := func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
		close(handlerStarted)
		<-ctx.Done()
		handlerDone <- ctx.Err()
		return nil, ctx.Err()
	}
	p := newShutdownPeer(t, handler)

	// A call the peer has received but not answered.
	callDone := make(chan error, 1)
	go func() { callDone <- p.connection.Call(context.Background(), "test/call", nil, nil) }()
	if got, want := p.readLine(), `{"jsonrpc":"2.0","id":1,"method":"test/call"}`; got != want {
		t.Fatalf("call frame = %s, want %s", got, want)
	}
	// An incoming request whose handler is running.
	p.send(`{"jsonrpc":"2.0","id":"in","method":"test/slow"}`)
	<-handlerStarted
	// A frame still queued when Shutdown begins.
	waits := p.queue(1)

	done := make(chan error, 1)
	go func() { done <- p.connection.Shutdown(context.Background()) }()
	waitUntil(t, "shutdown to begin", p.connection.shuttingDown.Load)
	if err := <-handlerDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("handler context = %v, want context.Canceled", err)
	}
	// Delivery has stopped: this response completes no call, and these
	// frames are not answered.
	p.send(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	p.send(`{"jsonrpc":"2.0","id":"late","method":"test/slow"}`)
	p.send(`not json`)
	if err := p.connection.Notify(context.Background(), "test/after", nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Notify during Shutdown = %v, want io.ErrClosedPipe", err)
	}
	if err := p.connection.Call(context.Background(), "test/after", nil, nil); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Call during Shutdown = %v, want io.ErrClosedPipe", err)
	}
	if err := p.connection.CancelRequest(context.Background(), 1); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("CancelRequest during Shutdown = %v, want io.ErrClosedPipe", err)
	}

	if got, want := strings.Join(p.readAll(), "\n"), notificationLines(1)[0]; got != want {
		t.Fatalf("peer received:\n%s\nwant:\n%s", got, want)
	}
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := waits[0](context.Background()); err != nil {
		t.Fatalf("queued notification: %v", err)
	}
	if err := <-callDone; !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("pending call = %v, want io.ErrClosedPipe", err)
	}
}

func TestShutdownKeepsCallFrameWhoseCallerCancels(t *testing.T) {
	p := newShutdownPeer(t, nil)
	// The write loop holds this frame in a blocked Write.
	p.queue(1)
	waitUntil(t, "the first frame to reach the writer", func() bool { return len(p.connection.writes) == 0 })
	ctx, cancel := context.WithCancel(context.Background())
	callDone := make(chan error, 1)
	go func() { callDone <- p.connection.Call(ctx, "test/call", nil, nil) }()
	waitUntil(t, "the call to be queued", func() bool { return len(p.connection.writes) == 1 })

	done := make(chan error, 1)
	go func() { done <- p.connection.Shutdown(context.Background()) }()
	waitUntil(t, "shutdown to begin", p.connection.shuttingDown.Load)
	cancel()
	if err := <-callDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Call = %v, want context.Canceled", err)
	}
	// The call gave up after the 250ms a cancelled caller otherwise allows a
	// queued frame before stopping the connection; the drain still owns it.
	want := notificationLines(1)[0] + "\n" + `{"jsonrpc":"2.0","id":1,"method":"test/call"}`
	if got := strings.Join(p.readAll(), "\n"); got != want {
		t.Fatalf("peer received:\n%s\nwant:\n%s", got, want)
	}
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestShutdownSurvivesInputEOF(t *testing.T) {
	p := newShutdownPeer(t, nil)
	p.queue(2)
	done := make(chan error, 1)
	go func() { done <- p.connection.Shutdown(context.Background()) }()
	waitUntil(t, "shutdown to begin", p.connection.shuttingDown.Load)
	if err := p.input.Close(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "input to close", func() bool { return closedChannel(p.connection.InputClosed()) })
	if got, want := strings.Join(p.readAll(), "\n"), strings.Join(notificationLines(2), "\n"); got != want {
		t.Fatalf("peer received:\n%s\nwant:\n%s", got, want)
	}
	if err := <-done; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

func TestShutdownStopsOnReadFailure(t *testing.T) {
	p := newShutdownPeer(t, nil)
	p.queue(2)
	done := make(chan error, 1)
	go func() { done <- p.connection.Shutdown(context.Background()) }()
	waitUntil(t, "shutdown to begin", p.connection.shuttingDown.Load)
	failure := errors.New("read failed")
	_ = p.input.CloseWithError(failure)
	select {
	case err := <-done:
		if !errors.Is(err, failure) {
			t.Fatalf("Shutdown = %v, want %v", err, failure)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a read failure did not stop the drain")
	}
}

func TestShutdownReportsWriteFailure(t *testing.T) {
	p := newShutdownPeer(t, nil)
	p.queue(2)
	done := make(chan error, 1)
	go func() { done <- p.connection.Shutdown(context.Background()) }()
	waitUntil(t, "shutdown to begin", p.connection.shuttingDown.Load)
	failure := errors.New("peer gone")
	_ = p.outputPipe.CloseWithError(failure)
	if err := <-done; !errors.Is(err, failure) {
		t.Fatalf("Shutdown = %v, want %v", err, failure)
	}
}

func TestShutdownRepeatedAndAfterStop(t *testing.T) {
	t.Run("concurrent", func(t *testing.T) {
		p := newShutdownPeer(t, nil)
		p.queue(2)
		results := make(chan error, 3)
		for range 3 {
			go func() { results <- p.connection.Shutdown(context.Background()) }()
		}
		waitUntil(t, "shutdown to begin", p.connection.shuttingDown.Load)
		if got := len(p.readAll()); got != 2 {
			t.Fatalf("peer received %d frames, want 2", got)
		}
		for range 3 {
			if err := <-results; err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
		}
		if err := p.connection.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown after Shutdown: %v", err)
		}
		if err := p.connection.Close(); err != nil {
			t.Fatalf("Close after Shutdown: %v", err)
		}
	})
	t.Run("after close", func(t *testing.T) {
		p := newShutdownPeer(t, nil)
		_ = p.connection.Close()
		if err := p.connection.Shutdown(context.Background()); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("Shutdown after Close = %v, want io.ErrClosedPipe", err)
		}
	})
	t.Run("after peer EOF", func(t *testing.T) {
		p := newShutdownPeer(t, nil)
		_ = p.input.Close()
		<-p.connection.Done()
		if err := p.connection.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown after EOF = %v, want nil", err)
		}
	})
}

func TestPeerEOFWritesQueuedFrames(t *testing.T) {
	for name, options := range shutdownOptions() {
		t.Run(name, func(t *testing.T) {
			p := newShutdownPeer(t, nil, options...)
			waits := p.queue(5)
			if err := p.input.Close(); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, "input to close", func() bool { return closedChannel(p.connection.InputClosed()) })
			if closedChannel(p.connection.Done()) {
				t.Fatal("Done closed before the queued frames were written")
			}
			if got, want := strings.Join(p.readAll(), "\n"), strings.Join(notificationLines(5), "\n"); got != want {
				t.Fatalf("peer received:\n%s\nwant:\n%s", got, want)
			}
			<-p.connection.Done()
			if err := p.connection.Err(); !errors.Is(err, io.EOF) {
				t.Fatalf("Err = %v, want io.EOF", err)
			}
			for i, wait := range waits {
				if err := wait(context.Background()); err != nil {
					t.Fatalf("notification %d: %v", i, err)
				}
			}
			if err := p.connection.Shutdown(context.Background()); err != nil {
				t.Fatalf("Shutdown after EOF: %v", err)
			}
			if err := p.connection.Notify(context.Background(), "test/after", nil); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("Notify after the EOF flush = %v, want io.ErrClosedPipe", err)
			}
		})
	}
}

func TestPeerEOFFlushReportsWriteFailure(t *testing.T) {
	p := newShutdownPeer(t, nil)
	p.queue(3)
	if err := p.input.Close(); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "input to close", func() bool { return closedChannel(p.connection.InputClosed()) })
	failure := errors.New("peer gone")
	_ = p.outputPipe.CloseWithError(failure)
	<-p.connection.Done()
	if err := p.connection.Err(); !errors.Is(err, failure) {
		t.Fatalf("Err = %v, want %v", err, failure)
	}
	if err := p.connection.Shutdown(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("Shutdown = %v, want %v", err, failure)
	}
}

func TestShutdownDuringEOFFlushJoinsIt(t *testing.T) {
	for name, options := range shutdownOptions() {
		t.Run(name, func(t *testing.T) {
			p := newShutdownPeer(t, nil, options...)
			waits := p.queue(3)
			// The peer is not reading, so the EOF flush stays in progress.
			if err := p.input.Close(); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, "the EOF flush to seal the queue", p.connection.shuttingDown.Load)
			// Nothing is accepted behind the flush's marker.
			if err := p.connection.Notify(context.Background(), "test/after", nil); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("Notify during the EOF flush = %v, want io.ErrClosedPipe", err)
			}
			if _, err := p.connection.queueNotification(context.Background(), "test/after", nil); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("queued notification during the EOF flush = %v, want io.ErrClosedPipe", err)
			}
			done := make(chan error, 1)
			go func() { done <- p.connection.Shutdown(context.Background()) }()
			if got, want := strings.Join(p.readAll(), "\n"), strings.Join(notificationLines(3), "\n"); got != want {
				t.Fatalf("peer received:\n%s\nwant:\n%s", got, want)
			}
			if err := <-done; err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			for i, wait := range waits {
				if err := wait(context.Background()); err != nil {
					t.Fatalf("notification %d: %v", i, err)
				}
			}
		})
	}
}

func TestDrainMarkerWaitsForAdmittedSender(t *testing.T) {
	for _, trigger := range []string{"shutdown", "eof"} {
		t.Run(trigger, func(t *testing.T) {
			p := newShutdownPeer(t, nil)
			// A sender that passed the seal check but has not queued yet.
			if !p.connection.admitFrame() {
				t.Fatal("admitFrame refused before the seal")
			}
			done := make(chan error, 1)
			if trigger == "shutdown" {
				go func() { done <- p.connection.Shutdown(context.Background()) }()
			} else if err := p.input.Close(); err != nil {
				t.Fatal(err)
			}
			waitUntil(t, "the queue to be sealed", p.connection.shuttingDown.Load)
			// Give a marker queued too early the chance to overtake the frame.
			time.Sleep(20 * time.Millisecond)
			if n := len(p.connection.writes); n != 0 {
				t.Fatalf("%d entries queued while a sender was admitted", n)
			}
			frame := notificationLines(1)[0]
			w := outgoing{data: []byte(frame + "\n"), sent: make(chan error, 1)}
			p.connection.writes <- w
			p.connection.enqueuing.Done()
			if got := strings.Join(p.readAll(), "\n"); got != frame {
				t.Fatalf("peer received %q, want %q", got, frame)
			}
			if err := <-w.sent; err != nil {
				t.Fatalf("admitted frame: %v", err)
			}
			if trigger == "shutdown" {
				if err := <-done; err != nil {
					t.Fatalf("Shutdown: %v", err)
				}
			} else {
				<-p.connection.Done()
				if err := p.connection.Err(); !errors.Is(err, io.EOF) {
					t.Fatalf("Err = %v, want io.EOF", err)
				}
			}
		})
	}
}
