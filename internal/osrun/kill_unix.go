//go:build unix

package osrun

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func processGroup() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// Kill terminates the command's whole process group. Like the Rust SDK's
// ChildGuard, it also kills the command directly, so a leader that moved to
// another process group still dies.
func Kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	groupErr := signalGroup(cmd, syscall.SIGKILL)
	if cmd.Process.Kill() == nil {
		return nil
	}
	return groupErr
}

// terminate sends SIGTERM to the command's process group and waits up to grace,
// or until ctx is done, for the command to exit. The caller sends SIGKILL to
// whatever remains.
func terminate(ctx context.Context, cmd *exec.Cmd, exited <-chan struct{}, grace time.Duration) {
	select {
	case <-exited:
		return
	default:
	}
	_ = signalGroup(cmd, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-exited:
	case <-timer.C:
	case <-ctx.Done():
	}
}

func signalGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
