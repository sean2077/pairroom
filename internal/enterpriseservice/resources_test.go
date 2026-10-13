package enterpriseservice

import (
	"sync"
	"testing"
	"time"
)

func TestResourceLimitsAlwaysBoundAdmission(t *testing.T) {
	defaults, err := (ResourceLimits{}).normalized()
	if err != nil || defaults.ConcurrentRequests != 32 {
		t.Fatal("zero did not select bounded defaults")
	}
	for _, n := range []int{-1, 257} {
		if _, err := (ResourceLimits{ConcurrentRequests: n}).normalized(); err == nil {
			t.Fatal("invalid admission limit accepted")
		}
	}
	r := newRequestResources(ResourceLimits{ConcurrentRequests: 2})
	for range 2 {
		if !r.admit() {
			t.Fatal("capacity refused an available business slot")
		}
	}
	if r.admit() {
		t.Fatal("capacity not enforced before business operations")
	}
	r.finish(200, 25*time.Millisecond)
	if !r.admit() {
		t.Fatal("completed request retained capacity")
	}
	r.draining.Store(true)
	if r.admit() {
		t.Fatal("drain admitted new business work")
	}
	r.finish(409, time.Second)
	r.finish(503, 31*time.Second)
	if r.active.Load() != 0 || len(r.slots) != 0 || r.rejected.Load() != 2 || r.completed[2].Load() != 1 || r.completed[4].Load() != 1 || r.completed[5].Load() != 1 || r.duration[1].Load() != 1 || r.duration[4].Load() != 1 || r.duration[7].Load() != 1 {
		t.Fatal("resource counters lost accepted/rejected work")
	}
}

func TestResourceAdmissionConcurrentDrainPreservesAcceptedWork(t *testing.T) {
	r := newRequestResources(ResourceLimits{ConcurrentRequests: 4})
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 128; i++ {
		wg.Go(func() {
			<-start
			if r.admit() {
				r.finish(200, time.Millisecond)
			}
		})
	}
	close(start)
	r.draining.Store(true)
	wg.Wait()
	if r.active.Load() != 0 || len(r.slots) != 0 || r.admit() {
		t.Fatal("drain leaked reservations")
	}
}
