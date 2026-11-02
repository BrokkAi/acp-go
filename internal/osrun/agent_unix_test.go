//go:build unix

package osrun

import (
	"context"
	"errors"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestStopSendsSIGTERMToAgentIgnoringStdinEOF(t *testing.T) {
	h := startHelper(t, "term")
	start := time.Now()
	if err := h.Stop(context.Background(), Shutdown{Exit: 20 * time.Millisecond, Terminate: time.Minute}); err != nil {
		t.Fatalf("Stop = %v, want nil for a requested termination", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("Stop waited %v; the agent should exit on SIGTERM", elapsed)
	}
	if text := h.stderrText(); text != "term\n" {
		t.Fatalf("stderr = %q, want the SIGTERM report", text)
	}
	if signal := waitSignal(t, h.Agent); signal != -1 {
		t.Fatalf("agent ended by %v, want a normal exit after SIGTERM", signal)
	}
}

func TestStopKillsAgentIgnoringSIGTERM(t *testing.T) {
	h := startHelper(t, "stubborn")
	const exit, terminate = 20 * time.Millisecond, 200 * time.Millisecond
	start := time.Now()
	if err := h.Stop(context.Background(), Shutdown{Exit: exit, Terminate: terminate}); err != nil {
		t.Fatalf("Stop = %v, want nil for a requested termination", err)
	}
	if elapsed := time.Since(start); elapsed < exit+terminate {
		t.Fatalf("Stop took %v; SIGKILL must wait for both grace periods", elapsed)
	}
	if text := h.stderrText(); text != "term\n" {
		t.Fatalf("stderr = %q, want SIGTERM before SIGKILL", text)
	}
	if signal := waitSignal(t, h.Agent); signal != syscall.SIGKILL {
		t.Fatalf("agent ended by %v, want SIGKILL", signal)
	}
}

func TestStopKillsProcessGroupAfterLauncherExits(t *testing.T) {
	h := startHelper(t, "group")
	pid, err := strconv.Atoi(h.line(t))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := h.Stop(context.Background(), Shutdown{Exit: time.Minute, Terminate: time.Minute}); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
	// The descendant holds stderr, so Stop returns before the stderr grace
	// only if it killed the group.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("Stop waited %v for the descendant's stderr", elapsed)
	}
	// The descendant is reparented and reaped elsewhere once killed.
	deadline := time.Now().Add(10 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("descendant in the agent's process group outlived Stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitSignal reports the signal that ended the agent, or -1 for an exit.
func waitSignal(t *testing.T, a *Agent) syscall.Signal {
	t.Helper()
	<-a.Exited()
	status := a.Cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !status.Signaled() {
		return -1
	}
	return status.Signal()
}

func TestStopReleasesPipes(t *testing.T) {
	openFiles := func() int {
		entries, err := os.ReadDir("/dev/fd")
		if err != nil {
			t.Skipf("cannot list open files: %v", err)
		}
		return len(entries)
	}
	stop := func() {
		h := startHelper(t, "eof")
		if err := h.Stop(context.Background(), Shutdown{Exit: time.Minute, Terminate: time.Minute}); err != nil {
			t.Fatal(err)
		}
	}
	stop() // Let the runtime open its long-lived descriptors first.
	before := openFiles()
	for range 3 {
		stop()
	}
	if after := openFiles(); after != before {
		t.Fatalf("open files grew from %d to %d over three Stops", before, after)
	}
}
