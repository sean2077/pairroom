//go:build windows

package relayclient

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/relay"
)

// A peer slot's hook scans this slot's state.json without the slot lock. That
// concurrent readPrivate must never make the owner's atomic save fail.
func TestConcurrentReadPrivateDoesNotFailStateSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := relay.AtomicJSON(path, State{Schema: 2, LastSeq: 0}); err != nil {
		t.Fatal(err)
	}
	const saves = 200
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				var s State
				_ = readPrivate(path, &s) // a scan may miss; it must not block saves
			}
		}()
	}
	var failed error
	for seq := uint64(1); seq <= saves && failed == nil; seq++ {
		failed = relay.AtomicJSON(path, State{Schema: 2, LastSeq: seq})
	}
	close(stop)
	wg.Wait()
	if failed != nil {
		t.Fatalf("state save failed while peers read it: %v", failed)
	}
	var final State
	if err := readPrivate(path, &final); err != nil || final.LastSeq != saves {
		t.Fatalf("final state = %+v, %v; want seq %d", final, err, saves)
	}
}
