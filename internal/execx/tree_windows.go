//go:build windows

package execx

import (
	"os"
	"syscall"
	"unsafe"
)

type jobHandle = syscall.Handle

// The frozen dependency closure keeps the root module on the standard library,
// so the Job Object entry points are loaded from kernel32 directly.
var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
)

const (
	jobObjectExtendedLimitInformationClass = 9
	jobObjectLimitKillOnJobClose           = 0x00002000
	processSetQuota                        = 0x0100
	processTerminate                       = 0x0001
)

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// attachJob creates a kill-on-close Job Object and assigns process to it. It
// returns zero when any step fails; the caller then kills only the direct
// child. The job handle is not inheritable, so descendants never hold it and
// closing it (or the Service exiting) terminates the whole tree.
func attachJob(process *os.Process) jobHandle {
	if process == nil {
		return 0
	}
	r, _, _ := procCreateJobObjectW.Call(0, 0)
	job := syscall.Handle(r)
	if job == 0 {
		return 0
	}
	var info jobObjectExtendedLimitInformation
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	if ok, _, _ := procSetInformationJobObject.Call(uintptr(job), jobObjectExtendedLimitInformationClass,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); ok == 0 {
		_ = syscall.CloseHandle(job)
		return 0
	}
	handle, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(process.Pid))
	if err != nil {
		_ = syscall.CloseHandle(job)
		return 0
	}
	defer syscall.CloseHandle(handle)
	if ok, _, _ := procAssignProcessToJobObject.Call(uintptr(job), uintptr(handle)); ok == 0 {
		_ = syscall.CloseHandle(job)
		return 0
	}
	return job
}

func terminateJob(job jobHandle) error {
	if ok, _, err := procTerminateJobObject.Call(uintptr(job), 1); ok == 0 {
		return err
	}
	return nil
}

func closeJob(job jobHandle) {
	_ = syscall.CloseHandle(job)
}
