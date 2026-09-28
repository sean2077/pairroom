package relayclient

import (
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// Presence restates transport facts per slot and never claims model activity.
func TestPresenceLineRestatesTransportFacts(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	oldest := now.Add(-12 * time.Minute)
	summary := &relay.Summary{
		Bindings: map[model.ActorID]relay.BindingSummary{
			model.ActorSlot1: {Active: true, Associated: true, CollectorActive: true, LastActivity: now.Add(-5 * time.Second)},
			model.ActorSlot2: {Active: true, Associated: true, LastActivity: now.Add(-3 * time.Hour)},
		},
		Inboxes: map[model.ActorID]relay.InboxSummary{
			model.ActorSlot2: {Queued: 2, Unknown: 1, OldestQueuedAt: &oldest},
		},
		LastWake: map[model.ActorID]relay.WakeObservation{
			model.ActorSlot2: {Outcome: "suppressed", Reason: "minimum_interval", At: now.Add(-90 * time.Second)},
		},
	}
	lines := presenceLine(summary, model.ActorSlot1, now)
	want := []string{
		"you slot1: waiting on its inbox now; last relay call 5s ago",
		"peer slot2: no collector waiting; last relay call 3h ago; 2 queued, oldest 12m ago; 1 unknown (inspect before Retry); last wake suppressed (minimum_interval) 1m ago",
	}
	if len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("presence =\n%s", strings.Join(lines, "\n"))
	}
	for _, line := range lines {
		for _, forbidden := range []string{"working", "idle", "finished", "online", "session"} {
			if strings.Contains(line, forbidden) {
				t.Fatalf("presence claims more than transport facts (%q): %s", forbidden, line)
			}
		}
	}
	unbound := presenceLine(&relay.Summary{Bindings: map[model.ActorID]relay.BindingSummary{}}, model.ActorSlot2, now)
	if unbound[0] != "peer slot1: not bound" || unbound[1] != "you slot2: not bound" {
		t.Fatalf("unbound presence = %q", unbound)
	}
}
