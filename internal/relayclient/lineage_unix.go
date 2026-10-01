//go:build !windows

package relayclient

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// platformProcessTable reads /proc where available and falls back to the
// platform's own `ps` (darwin/BSD). Only the standard library and platform
// tools are used, keeping the frozen dependency closure intact.
func platformProcessTable() (map[int]procInfo, error) {
	if _, err := os.Stat("/proc/self/stat"); err == nil {
		return procTableFromProc()
	}
	return procTableFromPS()
}

func procTableFromProc() (map[int]procInfo, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	table := make(map[int]procInfo, len(entries))
	for _, entry := range entries {
		pid, convErr := strconv.Atoi(entry.Name())
		if convErr != nil || !entry.IsDir() {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if readErr != nil {
			continue // a vanished process is not an error
		}
		// Format: <pid> (<comm>, may contain spaces/parens) <state> <ppid> ...
		text := string(data)
		openIdx := strings.IndexByte(text, '(')
		closeIdx := strings.LastIndexByte(text, ')')
		if openIdx < 0 || closeIdx <= openIdx+1 {
			continue
		}
		fields := strings.Fields(text[closeIdx+1:])
		if len(fields) < 2 {
			continue
		}
		ppid, ppidErr := strconv.Atoi(fields[1])
		if ppidErr != nil {
			continue
		}
		name := text[openIdx+1 : closeIdx]
		// Recent Node releases name their main thread "MainThread" in comm,
		// although the executable and process.title are still node. Resolve
		// this ambiguous thread label through the kernel's executable link;
		// never recognize an arbitrary MainThread as a Gemini host.
		if name == "MainThread" {
			if executable, err := os.Readlink(filepath.Join("/proc", entry.Name(), "exe")); err == nil {
				if base := filepath.Base(executable); geminiHostName(base) {
					name = base
				}
			}
		}
		table[pid] = procInfo{ppid: ppid, name: name}
	}
	return table, nil
}

func procTableFromPS() (map[int]procInfo, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return nil, err
	}
	table := map[int]procInfo{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		if pidErr != nil || ppidErr != nil {
			continue
		}
		table[pid] = procInfo{ppid: ppid, name: filepath.Base(fields[2])}
	}
	return table, nil
}
