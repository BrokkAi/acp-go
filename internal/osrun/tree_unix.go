//go:build unix

package osrun

import (
	"os/exec"
	"time"
)

// tree needs nothing on Unix: the agent leads its own process group, which
// Kill signals even after the agent itself has exited.
type tree struct{}

func startTree(*exec.Cmd) tree   { return tree{} }
func (*tree) kill(time.Duration) {}
func (*tree) close()             {}
