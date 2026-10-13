//go:build windows

package enterpriseservice

import (
	"golang.org/x/sys/windows"
	"unsafe"
)

func capacityRSS() uint64 {
	var counters struct {
		Size                                                                                                                       uint32
		PageFaults                                                                                                                 uint32
		PeakWorkingSet, WorkingSet, PeakPagedPool, PagedPool, PeakNonPagedPool, NonPagedPool, Pagefile, PeakPagefile, PrivateUsage uintptr
	}
	counters.Size = uint32(unsafe.Sizeof(counters))
	proc := windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")
	result, _, _ := proc.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&counters)), uintptr(counters.Size))
	if result == 0 {
		return 0
	}
	return uint64(counters.WorkingSet)
}
