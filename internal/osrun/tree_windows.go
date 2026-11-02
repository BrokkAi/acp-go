//go:build windows

package osrun

import (
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW          = kernel32.NewProc("CreateJobObjectW")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject        = kernel32.NewProc("TerminateJobObject")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")
)

const (
	processSetQuota                     = 0x0100
	jobObjectBasicAccountingInformation = 1
)

// jobAccounting is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// tree is a job object holding the agent and every process it starts.
// Windows has no process groups, and taskkill /T cannot find descendants once
// the agent itself has exited, so the job is what lets kill reach them, as
// the Unix process group does. A zero tree, when the job could not be set up,
// leaves the agent to Kill alone.
type tree struct{ job syscall.Handle }

// startTree puts the started command into a new job object. Processes it
// starts afterwards join the job automatically.
func startTree(cmd *exec.Cmd) tree {
	job, _, _ := procCreateJobObjectW.Call(0, 0)
	if job == 0 {
		return tree{}
	}
	process, err := syscall.OpenProcess(processSetQuota|syscall.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = syscall.CloseHandle(syscall.Handle(job))
		return tree{}
	}
	defer syscall.CloseHandle(process)
	if ok, _, _ := procAssignProcessToJobObject.Call(job, uintptr(process)); ok == 0 {
		_ = syscall.CloseHandle(syscall.Handle(job))
		return tree{}
	}
	return tree{job: syscall.Handle(job)}
}

// kill terminates every process in the job and waits up to grace for them
// to be gone, so their handles, such as working directories, are released.
func (t *tree) kill(grace time.Duration) {
	if t.job == 0 {
		return
	}
	_, _, _ = procTerminateJobObject.Call(uintptr(t.job), 1)
	deadline := time.Now().Add(grace)
	for {
		var info jobAccounting
		ok, _, _ := procQueryInformationJobObject.Call(uintptr(t.job), jobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info), 0)
		if ok == 0 || info.ActiveProcesses == 0 || time.Now().After(deadline) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (t *tree) close() {
	if t.job != 0 {
		_ = syscall.CloseHandle(t.job)
		t.job = 0
	}
}
