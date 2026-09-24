//go:build linux || darwin

package daemon

import "time"

// durationSeconds rounds up so service-manager stop timeouts never undercut
// the configured drain budget. Only the systemd and launchd writers use it.
func durationSeconds(value time.Duration) int64 {
	seconds := int64(value / time.Second)
	if value%time.Second != 0 {
		seconds++
	}
	return seconds
}
