//go:build windows

package osrun

import (
	"os/exec"
	"strconv"
	"syscall"
)

func processGroup() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// Kill terminates the command and its descendants. Windows has no process
// groups to signal, so taskkill walks the tree; the direct kill covers a
// missing taskkill and reports os.ErrProcessDone once the process is gone.
func Kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run() == nil {
		return nil
	}
	return cmd.Process.Kill()
}
