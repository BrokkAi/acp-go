package osrun

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

const helperEnv = "ACP_GO_OSRUN_TEST_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		runHelper(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// runHelper is the agent process the shutdown tests launch. It reports the
// signals it handles on stderr and writes "ready" to stdout once its handlers
// are installed.
func runHelper(mode string) {
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	ready := func() { fmt.Println("ready") }
	switch mode {
	case "eof", "eof-fail":
		ready()
		_, _ = io.Copy(io.Discard, os.Stdin)
		fmt.Fprintln(os.Stderr, "eof")
		if mode == "eof-fail" {
			os.Exit(3)
		}
	case "term":
		ready()
		<-terms
		fmt.Fprintln(os.Stderr, "term")
	case "stubborn":
		ready()
		for range terms {
			fmt.Fprintln(os.Stderr, "term")
		}
	case "group":
		// A launcher whose real agent ignores SIGTERM and stdin EOF and
		// holds the launcher's stderr open.
		child := startStubborn(io.Discard)
		ready()
		fmt.Println(child.Process.Pid)
		_, _ = io.Copy(io.Discard, os.Stdin)
	case "crash":
		// An agent that fails mid-protocol while a descendant holds its
		// stdout open.
		startStubborn(os.Stdout)
		ready()
		fmt.Fprintln(os.Stderr, "crash")
		os.Exit(5)
	case "quit":
		ready()
	default:
		panic("unknown helper mode " + mode)
	}
}

// startStubborn starts a descendant in the helper's process group that ignores
// stdin EOF and SIGTERM and inherits the helper's stderr.
func startStubborn(stdout io.Writer) *exec.Cmd {
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), helperEnv+"=stubborn")
	child.Stdout = stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		panic(err)
	}
	return child
}

type helper struct {
	*Agent
	stderr *Tail
	stdout *bufio.Reader
}

func startHelper(t *testing.T, mode string) helper {
	t.Helper()
	stderr := &Tail{Capacity: 4096}
	// The race detector otherwise sleeps a second before a clean exit.
	env := map[string]string{helperEnv: mode, "GORACE": "atexit_sleep_ms=0"}
	agent, err := StartAgent("", []string{os.Args[0]}, env, stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Kill(DefaultShutdown) })
	h := helper{Agent: agent, stderr: stderr, stdout: bufio.NewReader(agent.Stdout)}
	if line := h.line(t); line != "ready" {
		t.Fatalf("helper wrote %q before it was ready", line)
	}
	return h
}

func (h helper) line(t *testing.T) string {
	t.Helper()
	line, err := h.stdout.ReadString('\n')
	if err != nil {
		t.Fatalf("helper stdout: %v", err)
	}
	return strings.TrimSpace(line)
}

func (h helper) stderrText() string {
	text, _ := h.stderr.Text()
	return text
}

func TestStopLetsAgentExitOnStdinEOF(t *testing.T) {
	for _, test := range []struct {
		mode string
		code int
	}{{"eof", 0}, {"eof-fail", 3}} {
		t.Run(test.mode, func(t *testing.T) {
			h := startHelper(t, test.mode)
			// Generous bounds: a well-behaved agent never waits for them.
			start := time.Now()
			err := h.Stop(context.Background(), Shutdown{Exit: time.Minute, Terminate: time.Minute})
			if elapsed := time.Since(start); elapsed > 30*time.Second {
				t.Fatalf("Stop waited %v for an agent exiting on stdin EOF", elapsed)
			}
			var exit *exec.ExitError
			switch {
			case test.code == 0 && err != nil:
				t.Fatalf("Stop = %v, want nil", err)
			case test.code != 0 && (!errors.As(err, &exit) || exit.ExitCode() != test.code):
				t.Fatalf("Stop = %v, want exit status %d", err, test.code)
			}
			if text := h.stderrText(); text != "eof\n" {
				t.Fatalf("stderr = %q, want only the EOF report and no signal", text)
			}
		})
	}
}

func TestKillDoesNotWaitForStdinEOF(t *testing.T) {
	h := startHelper(t, "stubborn")
	h.Kill(Shutdown{Exit: time.Minute, Terminate: time.Minute})
	select {
	case <-h.Exited():
	default:
		t.Fatal("Kill returned before the agent exited")
	}
	if text := h.stderrText(); text != "" {
		t.Fatalf("stderr = %q, want no SIGTERM before the kill", text)
	}
}

func TestStopKillsAtOnceWhenCancelled(t *testing.T) {
	h := startHelper(t, "stubborn")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	if err := h.Stop(ctx, Shutdown{Exit: time.Minute, Terminate: time.Minute}); err != nil {
		t.Fatalf("Stop = %v, want nil for a requested termination", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("cancelled Stop waited %v", elapsed)
	}
	if text := h.stderrText(); text != "" {
		t.Fatalf("stderr = %q, want no SIGTERM before the kill", text)
	}
}

// fakeProtocol stands in for the connection over the agent's stdio.
type fakeProtocol struct {
	done   chan struct{}
	closed chan struct{}
}

func newFakeProtocol() *fakeProtocol {
	return &fakeProtocol{done: make(chan struct{}), closed: make(chan struct{})}
}
func (p *fakeProtocol) Done() <-chan struct{} { return p.done }
func (p *fakeProtocol) Close() error {
	close(p.closed)
	return nil
}

func TestSuperviseStopsProtocolWhenAgentFails(t *testing.T) {
	h := startHelper(t, "crash")
	protocol := newFakeProtocol()
	release := h.Supervise(protocol, Shutdown{Exit: time.Minute, Terminate: time.Minute})
	// The descendant holding stdout and stderr dies with the group, so the
	// protocol is closed once stderr reaches EOF, well before the grace.
	select {
	case <-protocol.closed:
	case <-time.After(30 * time.Second):
		t.Fatal("protocol still running after the agent failed")
	}
	var exit *exec.ExitError
	if err := release(); !errors.As(err, &exit) || exit.ExitCode() != 5 {
		t.Fatalf("release = %v, want exit status 5", err)
	}
	if text := h.stderrText(); text != "crash\n" {
		t.Fatalf("stderr = %q, want the crash report", text)
	}
}

func TestSuperviseGivesProtocolGraceAfterAgentSucceeds(t *testing.T) {
	h := startHelper(t, "quit")
	protocol := newFakeProtocol()
	const grace = 50 * time.Millisecond
	start := time.Now()
	release := h.Supervise(protocol, Shutdown{Exit: grace, Terminate: time.Minute})
	select {
	case <-protocol.closed:
	case <-time.After(30 * time.Second):
		t.Fatal("protocol still running after the agent exited")
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Fatalf("protocol closed after %v, before its %v grace", elapsed, grace)
	}
	if err := release(); err != nil {
		t.Fatalf("release = %v, want nil for a successful exit", err)
	}
}

func TestReleaseEndsSupervisionOfRunningAgent(t *testing.T) {
	h := startHelper(t, "eof")
	protocol := newFakeProtocol()
	release := h.Supervise(protocol, Shutdown{Exit: time.Minute, Terminate: time.Minute})
	if err := release(); err != nil {
		t.Fatalf("release = %v, want nil for a running agent", err)
	}
	if err := h.Stop(context.Background(), Shutdown{Exit: time.Minute, Terminate: time.Minute}); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	select {
	case <-protocol.closed:
		t.Fatal("released supervision closed the protocol")
	default:
	}
}
