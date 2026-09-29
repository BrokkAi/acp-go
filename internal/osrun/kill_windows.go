//go:build windows

package osrun

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

func processGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// Kill terminates the command and its descendants. Windows has no process
// groups to signal, so taskkill walks the tree; the direct kill covers a
// missing taskkill.
//
// Wait releases Go's process handle, after which the PID may name an unrelated
// process. Kill therefore opens its own handle first and then confirms Go has
// not reaped the command: Go's handle was held from Start until that check, so
// ours names the same process and pins the PID while taskkill runs. Descendants
// orphaned by an exited command are out of reach here, unlike on Unix.
func Kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return reaped(cmd.Process.Kill())
	}
	defer syscall.CloseHandle(handle)
	// Signal(0) has no effect on a live process and fails once Wait has run.
	if err := reaped(cmd.Process.Signal(syscall.Signal(0))); errors.Is(err, os.ErrProcessDone) {
		return err
	}
	if exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run() == nil {
		return nil
	}
	return reaped(cmd.Process.Kill())
}

// reaped maps the EINVAL that exec.Cmd.Wait's Release leaves behind to
// os.ErrProcessDone, matching the Unix result for a finished command.
func reaped(err error) error {
	if errors.Is(err, syscall.EINVAL) {
		return os.ErrProcessDone
	}
	return err
}
