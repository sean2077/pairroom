//go:build !windows

package execx

import "os"

// Unix vendor launchers (including npm bin links) execute the CLI directly,
// so the direct child is the process tree PairRoom owns.
type jobHandle = uintptr

func attachJob(*os.Process) jobHandle { return 0 }

func terminateJob(jobHandle) error { return nil }

func closeJob(jobHandle) {}
