//go:build windows

package relayclient

import (
	"syscall"
	"unsafe"
)

// platformProcessTable enumerates processes through a Toolhelp32 snapshot
// using only the standard library, keeping the frozen dependency closure
// intact.
func platformProcessTable() (map[int]procInfo, error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	create := kernel32.NewProc("CreateToolhelp32Snapshot")
	first := kernel32.NewProc("Process32FirstW")
	next := kernel32.NewProc("Process32NextW")
	const th32csSnapProcess = 0x00000002
	handle, _, err := create.Call(th32csSnapProcess, 0)
	if handle == ^uintptr(0) {
		return nil, err
	}
	defer syscall.CloseHandle(syscall.Handle(handle))
	// PROCESSENTRY32W; size must be set before the first call.
	var entry struct {
		size          uint32
		usage         uint32
		processID     uint32
		defaultHeapID uintptr
		moduleID      uint32
		threads       uint32
		parentPID     uint32
		priClassBase  int32
		priDelta      uint32
		exeFile       [260]uint16
	}
	entry.size = uint32(unsafe.Sizeof(entry))
	table := make(map[int]procInfo)
	ok, _, _ := first.Call(handle, uintptr(unsafe.Pointer(&entry)))
	if ok == 0 {
		// An empty snapshot is not fatal; callers treat it as "no lineage".
		return table, nil
	}
	table[int(entry.processID)] = procInfo{ppid: int(entry.parentPID), name: syscall.UTF16ToString(entry.exeFile[:])}
	for {
		ok, _, _ := next.Call(handle, uintptr(unsafe.Pointer(&entry)))
		if ok == 0 {
			break
		}
		table[int(entry.processID)] = procInfo{ppid: int(entry.parentPID), name: syscall.UTF16ToString(entry.exeFile[:])}
	}
	return table, nil
}
