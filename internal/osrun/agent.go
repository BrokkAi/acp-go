package osrun

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Shutdown bounds the graceful shutdown of an agent process. The defaults
// match the Rust SDK's AcpAgent, which also keeps them out of its public API.
type Shutdown struct {
	// Exit is how long the agent gets to exit on its own after its stdin is
	// closed. It also bounds the wait for stderr to drain after the exit.
	Exit time.Duration
	// Terminate is how long the agent's process group gets to exit after
	// SIGTERM before it is sent SIGKILL. Windows has no SIGTERM step.
	Terminate time.Duration
}

// DefaultShutdown waits one second for the agent to exit after stdin EOF and,
// on Unix, two more seconds after SIGTERM.
var DefaultShutdown = Shutdown{Exit: time.Second, Terminate: 2 * time.Second}

// Agent is an agent process speaking a protocol over its standard streams.
//
// All three streams are OS pipes the child holds directly, so the process exit
// is observed as soon as it happens, independently of descendants that keep
// stderr open and of readers that have not yet consumed stdout.
type Agent struct {
	Cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser

	stderr  *os.File
	treeMu  sync.Mutex
	tree    tree
	drained chan struct{}
	exited  chan struct{}
	err     error
}

// StartAgent launches args as the leader of a new process group (on Unix) and
// copies its stderr to stderr. The caller must call Stop or Kill.
func StartAgent(dir string, args []string, env map[string]string, stderr io.Writer) (*Agent, error) {
	cmd := StartCommand(context.Background(), dir, args, env)
	// Read and write ends of the stdin, stdout, and stderr pipes, in order.
	var files [6]*os.File
	closeFiles := func(files ...*os.File) {
		for _, file := range files {
			if file != nil {
				_ = file.Close()
			}
		}
	}
	for i := 0; i < len(files); i += 2 {
		r, w, err := os.Pipe()
		if err != nil {
			closeFiles(files[:]...)
			return nil, err
		}
		files[i], files[i+1] = r, w
	}
	stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW := files[0], files[1], files[2], files[3], files[4], files[5]
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	err := cmd.Start()
	closeFiles(stdinR, stdoutW, stderrW)
	if err != nil {
		closeFiles(stdinW, stdoutR, stderrR)
		return nil, err
	}
	a := &Agent{Cmd: cmd, Stdin: stdinW, Stdout: stdoutR, stderr: stderrR, tree: startTree(cmd),
		drained: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		_, _ = io.Copy(stderr, stderrR)
		close(a.drained)
	}()
	go func() {
		a.err = cmd.Wait()
		close(a.exited)
	}()
	return a, nil
}

// Exited is closed once the process has exited and been reaped.
func (a *Agent) Exited() <-chan struct{} { return a.exited }

// Protocol is the connection running over the agent's stdio.
type Protocol interface {
	Done() <-chan struct{}
	Close() error
}

// Supervise races the agent process against the protocol, as the Rust SDK's
// AcpAgent does. If the agent exits while the protocol is still running, its
// process group is killed at once, so a descendant cannot hold stdout open.
// After a failed exit the protocol is closed once stderr reaches EOF, or after
// s.Exit; after a successful exit it gets s.Exit to finish before it is
// closed.
//
// The returned release function ends supervision. Call it when the protocol
// work is over and before closing the connection: it reports the exit error of
// an agent that exited on its own with a failure status before then.
func (a *Agent) Supervise(p Protocol, s Shutdown) (release func() error) {
	released := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-released:
			return
		case <-a.exited:
		}
		a.kill(s.Exit)
		wait := p.Done()
		if a.err != nil {
			wait = a.drained
		}
		timer := time.NewTimer(s.Exit)
		defer timer.Stop()
		select {
		case <-wait:
		case <-timer.C:
		case <-released:
			return
		}
		_ = p.Close()
	}()
	var once sync.Once
	return func() error {
		once.Do(func() { close(released) })
		<-done
		select {
		case <-a.exited:
			return a.err
		default:
			return nil
		}
	}
}

// Stop shuts the agent down the way the ACP stdio transport asks: it closes
// stdin and gives the agent s.Exit to exit on its own. An agent still running
// after that is terminated: on Unix its process group is sent SIGTERM, then
// SIGKILL if it has not exited within s.Terminate; on Windows its process tree
// is killed. Once the agent has exited, whatever remains of its process group
// is killed, so a launcher cannot leave its real agent behind.
//
// Cancelling ctx, before or during Stop, abandons the grace periods and kills
// the process group at once, as Kill does.
//
// Stop returns the exit error of an agent that exited on its own, and nil for
// one that Stop terminated, since that exit status was requested.
func (a *Agent) Stop(ctx context.Context, s Shutdown) error {
	if ctx.Err() != nil {
		a.Kill(s)
		return nil
	}
	_ = a.Stdin.Close()
	timer := time.NewTimer(s.Exit)
	defer timer.Stop()
	select {
	case <-a.exited:
		a.finish(s.Exit)
		return a.err
	case <-timer.C:
		terminate(ctx, a.Cmd, a.exited, s.Terminate)
	case <-ctx.Done():
	}
	a.finish(s.Exit)
	return nil
}

// Kill is the path for a cancelled or failed run: it kills the agent's process
// group (process tree on Windows) at once, without a grace period, and waits
// for it. It gives stderr up to s.Exit to reach EOF.
func (a *Agent) Kill(s Shutdown) { a.finish(s.Exit) }

// finish kills whatever remains of the process group, waits for the exit,
// closes stdin and stdout, and then gives stderr up to grace to reach EOF
// before closing it.
func (a *Agent) finish(grace time.Duration) {
	a.kill(grace)
	<-a.exited
	_ = a.Stdin.Close()
	_ = a.Stdout.Close()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-a.drained:
	case <-timer.C:
		// A descendant outside the group still holds stderr. Closing the
		// read end ends the copy; captured bytes stay with the writer.
	}
	_ = a.stderr.Close()
	a.treeMu.Lock()
	a.tree.close()
	a.treeMu.Unlock()
}

// kill kills the agent's process group (process tree on Windows), including
// descendants of an agent that has already exited, waiting up to grace for
// them on Windows.
func (a *Agent) kill(grace time.Duration) {
	_ = Kill(a.Cmd)
	a.treeMu.Lock()
	defer a.treeMu.Unlock()
	a.tree.kill(grace)
}
