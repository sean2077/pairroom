package service

import (
	"net/http"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/version"
)

const cliBuildStabilityWindow = 30 * time.Second

// CLIBuildMismatch is a last-observed authenticated CLI report, not a disk
// inventory or an update check. It is ephemeral and never enters Room facts.
type CLIBuildMismatch struct {
	Service string `json:"service"`
	CLI     string `json:"cli"`
}
type cliBuildObserver struct {
	mu      sync.Mutex
	running string
	latest  string
	since   time.Time
	stable  bool
}

func newCLIBuildObserver(running string) *cliBuildObserver {
	token := version.StampedToken(running)
	if token == "" {
		return nil
	}
	return &cliBuildObserver{running: token}
}
func (o *cliBuildObserver) observe(value string, now time.Time) {
	if o == nil {
		return
	}
	token := version.StampedToken(value)
	if token == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if token == o.running {
		o.latest = ""
		o.since = time.Time{}
		o.stable = false
		return
	}
	if token != o.latest || now.Before(o.since) {
		o.latest = token
		o.since = now
		o.stable = false
		return
	}
	// A second observation spanning the window is required. Browser polling
	// alone cannot promote one isolated CLI report into a banner.
	if now.Sub(o.since) >= cliBuildStabilityWindow {
		o.stable = true
	}
}
func (o *cliBuildObserver) snapshot() *CLIBuildMismatch {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.stable {
		return nil
	}
	return &CLIBuildMismatch{Service: o.running, CLI: o.latest}
}
func (s *ManagementServer) observeCLIBuild(r *http.Request) {
	values := r.Header.Values(relay.CLIBuildHeader)
	if len(values) != 1 {
		return
	}
	s.cliBuild.observe(values[0], time.Now())
}
