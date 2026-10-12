package sqlstore

import (
	"context"
	"strings"
	"testing"
)

// The deployment lease must be exclusive while live, and its loss must be
// sticky: the deposed incarnation is retired permanently and never revives,
// re-discovers or retries — even though a new holder could otherwise appear.
func TestLeaseIsExclusiveAndLossIsSticky(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := Open(ctx, f.cfg); err == nil {
		t.Fatal("a second Store acquired the live deployment lease")
	} else if !strings.Contains(err.Error(), "live lease holder") {
		t.Fatalf("second Open error: %v", err)
	}

	f.terminate(t, f.store.lease.pid, "lease")

	res, err := f.store.Commit(ctx, f.command("lost-1"))
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeRefused || kerr.Class != ClassLeaseLost {
		t.Fatalf("commit after lease loss: %+v", res)
	}
	// Sticky: the poisoned Store refuses everything without touching the Room.
	res, err = f.store.Commit(ctx, f.command("lost-2"))
	if kerr := kernelErr(t, res, err); kerr.Class != ClassLeaseLost {
		t.Fatalf("lease loss was not sticky: %+v", res)
	}
	cmd := f.command("lost-3")
	res, err = f.store.Resolve(ctx, cmd.Auth, cmd.Key, cmd.Mutation)
	if kerr := kernelErr(t, res, err); kerr.Class != ClassLeaseLost {
		t.Fatalf("resolve after lease loss: %+v", res)
	}
	if events, _, _, _, _ := f.roomCounts(t); events != 0 {
		t.Fatalf("a deposed incarnation still wrote %d events", events)
	}
}
