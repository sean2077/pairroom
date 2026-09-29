//go:build windows

package relayclient

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// platformProcessTable enumerates processes through a Toolhelp32 snapshot.
func platformProcessTable() (map[int]procInfo, error) {
	handle, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(handle)
	// PROCESSENTRY32W; size must be set before the first call.
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	table := make(map[int]procInfo)
	if err := windows.Process32First(handle, &entry); err != nil {
		// An empty snapshot is not fatal; callers treat it as "no lineage".
		return table, nil
	}
	table[int(entry.ProcessID)] = procInfo{ppid: int(entry.ParentProcessID), name: windows.UTF16ToString(entry.ExeFile[:])}
	for {
		if err := windows.Process32Next(handle, &entry); err != nil {
			break
		}
		table[int(entry.ProcessID)] = procInfo{ppid: int(entry.ParentProcessID), name: windows.UTF16ToString(entry.ExeFile[:])}
	}
	return table, nil
}
