package enterpriseservice

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/sean2077/pairroom/internal/sqlstore"
)

type readinessBackend interface {
	Readiness(context.Context) (sqlstore.Readiness, error)
}

// BeginDrain refuses new business requests. Already admitted operations retain
// their original context and can finish; the listener's owner performs Shutdown.
func (h *Handler) BeginDrain() { h.resources.draining.Store(true) }

type resourceDiagnostics struct {
	Version                int       `json:"version"`
	Draining               bool      `json:"draining"`
	ConcurrentRequestLimit int       `json:"concurrent_request_limit"`
	ConnectionLimit        int       `json:"connection_limit"`
	ConcurrentReplayLimit  int       `json:"concurrent_replay_limit"`
	ReplaysActive          int       `json:"replays_active"`
	RequestsActive         int64     `json:"requests_active"`
	RequestsRejected       uint64    `json:"requests_rejected"`
	RequestsCompleted      [6]uint64 `json:"requests_completed_by_status_class"`
	DurationBuckets        [8]uint64 `json:"request_duration_buckets"`
	DurationNanoseconds    uint64    `json:"request_duration_nanoseconds"`
	ResidentRooms          int       `json:"resident_rooms"`
}

func (h *Handler) resourceDiagnostics() resourceDiagnostics {
	r := h.resources
	d := resourceDiagnostics{Version: 1, Draining: r.draining.Load(), ConcurrentRequestLimit: cap(r.slots), RequestsActive: r.active.Load(), RequestsRejected: r.rejected.Load(), DurationNanoseconds: r.durationNanoseconds.Load()}
	d.ConnectionLimit = h.policy.resources.Connections
	d.ConcurrentReplayLimit, d.ReplaysActive = cap(r.replaySlots), len(r.replaySlots)
	for i := range d.RequestsCompleted {
		d.RequestsCompleted[i] = r.completed[i].Load()
	}
	for i := range d.DurationBuckets {
		d.DurationBuckets[i] = r.duration[i].Load()
	}
	return d
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/enterprise/health/live" && r.URL.Path != "/enterprise/health/ready" {
		return false
	}
	if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || !h.policy.accepts(r) {
		fail(w, http.StatusBadRequest, "invalid_health_request")
		return true
	}
	if r.URL.Path == "/enterprise/health/live" {
		writeJSON(w, http.StatusOK, struct {
			Live     bool `json:"live"`
			Draining bool `json:"draining"`
		}{true, h.resources.draining.Load()})
		return true
	}
	backend, ok := h.backend.(readinessBackend)
	if !ok || h.resources.draining.Load() {
		fail(w, http.StatusServiceUnavailable, "not_ready")
		return true
	}
	select {
	case h.resources.healthSlots <- struct{}{}:
		defer func() { <-h.resources.healthSlots }()
	default:
		fail(w, http.StatusServiceUnavailable, "not_ready")
		return true
	}
	// Health polls must not indefinitely queue behind business requests or hold
	// another SQL connection. The Store repeats its actual ownership checks.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	state, err := backend.Readiness(ctx)
	if err != nil || !state.SchemaCompatible || !state.OwnershipCurrent || !state.RecoveryComplete || !state.RestoreClear {
		fail(w, http.StatusServiceUnavailable, "not_ready")
		return true
	}
	writeJSON(w, http.StatusOK, struct {
		Ready bool `json:"ready"`
	}{true})
	return true
}

func (h *Handler) observabilityBrowser(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession) bool {
	if r.URL.Path != administrationPrefix+"diagnostics" && r.URL.Path != administrationPrefix+"metrics" {
		return false
	}
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return true
	}
	if !session.Administrator || session.Auth.SessionID == "" || session.Auth.DeviceGrantID != "" || session.Auth.BindingGrantID != "" {
		fail(w, http.StatusForbidden, "administrator_required")
		return true
	}
	backend, ok := h.backend.(AdministrationBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "diagnostics_unavailable")
		return true
	}
	// Recheck the current SQL administrator and original session under the same
	// ownership fence. A stale HTTP Administrator flag cannot authorize metrics.
	if _, err := backend.ReadAdministration(r.Context(), session.Auth, sqlstore.AdministrationQuery{Resource: "users", Limit: 1}); err != nil {
		writeDeviceError(w, err)
		return true
	}
	d := h.resourceDiagnostics()
	if r.URL.Path == administrationPrefix+"diagnostics" {
		writeJSON(w, http.StatusOK, d)
		return true
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "pairroom_http_requests_active %d\npairroom_http_request_limit %d\npairroom_http_requests_rejected_total %d\npairroom_resident_rooms %d\n", d.RequestsActive, d.ConcurrentRequestLimit, d.RequestsRejected, d.ResidentRooms)
	_, _ = fmt.Fprintf(w, "pairroom_replays_active %d\npairroom_replay_limit %d\n", d.ReplaysActive, d.ConcurrentReplayLimit)
	_, _ = fmt.Fprintf(w, "pairroom_connection_limit %d\n", d.ConnectionLimit)
	for i, n := range d.RequestsCompleted {
		_, _ = fmt.Fprintf(w, "pairroom_http_requests_completed_total{status_class=\"%d\"} %d\n", i, n)
	}
	// Only fixed numeric buckets and status classes are labels. URL, User, Room,
	// Device, Binding, deployment and error text never enter this representation.
	var cumulative uint64
	for i, bound := range durationBounds {
		cumulative += d.DurationBuckets[i]
		_, _ = fmt.Fprintf(w, "pairroom_http_request_duration_seconds_bucket{le=\"%g\"} %d\n", bound.Seconds(), cumulative)
	}
	cumulative += d.DurationBuckets[len(durationBounds)]
	_, _ = fmt.Fprintf(w, "pairroom_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\npairroom_http_request_duration_seconds_count %d\n", cumulative, cumulative)
	_, _ = fmt.Fprintf(w, "pairroom_http_request_duration_seconds_sum %g\n", float64(d.DurationNanoseconds)/float64(time.Second))
	return true
}
