//go:build unix

package osrun

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func processGroup() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// Kill terminates the command's whole process group.
func Kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
