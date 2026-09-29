//go:build windows

package execx

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type jobHandle = windows.Handle

// attachJob creates a kill-on-close Job Object and assigns process to it. It
// returns zero when any step fails; the caller then kills only the direct
// child. The job handle is not inheritable, so descendants never hold it and
// closing it (or the Service exiting) terminates the whole tree.
func attachJob(process *os.Process) jobHandle {
	if process == nil {
		return 0
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(process.Pid))
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	defer windows.CloseHandle(handle)
	if err := windows.AssignProcessToJobObject(job, handle); err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	return job
}

func terminateJob(job jobHandle) error {
	return windows.TerminateJobObject(job, 1)
}

func closeJob(job jobHandle) {
	_ = windows.CloseHandle(job)
}
