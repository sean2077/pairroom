package enterpriseservice

import (
	"net/http"
	"sync/atomic"
	"time"
)

// ResourceLimits bounds simultaneous HTTP operations, including requests
// waiting for the single SQL work connection. A zero value selects defaults;
// it never disables admission. Limits are shared by browser and Native routes.
type ResourceLimits struct {
	ConcurrentRequests int `json:"concurrent_requests"`
	Connections        int `json:"connections"`
}

const DefaultConcurrentRequests = 32

func (l ResourceLimits) Normalized() (ResourceLimits, error) { return l.normalized() }

func (l ResourceLimits) normalized() (ResourceLimits, error) {
	if l.Connections == 0 {
		l.Connections = 256
	}
	if l.ConcurrentRequests == 0 {
		l.ConcurrentRequests = DefaultConcurrentRequests
	}
	if l.ConcurrentRequests < 1 || l.ConcurrentRequests > 256 || l.Connections < 1 || l.Connections > 256 {
		return ResourceLimits{}, ErrConfiguration
	}
	return l, nil
}

type requestResources struct {
	slots               chan struct{}
	replaySlots         chan struct{}
	healthSlots         chan struct{}
	draining            atomic.Bool
	active              atomic.Int64
	rejected            atomic.Uint64
	completed           [6]atomic.Uint64
	duration            [8]atomic.Uint64
	durationNanoseconds atomic.Uint64
}

var durationBounds = [...]time.Duration{10 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 500 * time.Millisecond, time.Second, 5 * time.Second, 30 * time.Second}

func newRequestResources(l ResourceLimits) *requestResources {
	return &requestResources{slots: make(chan struct{}, l.ConcurrentRequests), replaySlots: make(chan struct{}, 2), healthSlots: make(chan struct{}, 2)}
}

func (r *requestResources) admit() bool {
	if r.draining.Load() {
		r.rejected.Add(1)
		return false
	}
	select {
	case r.slots <- struct{}{}:
		// Drain can start between the first check and slot reservation.
		if r.draining.Load() {
			<-r.slots
			r.rejected.Add(1)
			return false
		}
		r.active.Add(1)
		return true
	default:
		r.rejected.Add(1)
		return false
	}
}

func (r *requestResources) finish(status int, elapsed time.Duration) {
	r.active.Add(-1)
	<-r.slots
	class := status / 100
	if class < 1 || class > 5 {
		class = 0
	}
	r.completed[class].Add(1)
	if elapsed > 0 {
		r.durationNanoseconds.Add(uint64(elapsed))
	}
	bucket := len(durationBounds)
	for i, bound := range durationBounds {
		if elapsed <= bound {
			bucket = i
			break
		}
	}
	r.duration[bucket].Add(1)
}

type resourceResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *resourceResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *resourceResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

// Unwrap preserves ResponseController's access to the original transport.
func (w *resourceResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
