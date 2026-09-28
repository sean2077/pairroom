package relayclient

import (
	"fmt"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// presenceLine renders one human-readable line per slot from the bounded
// Summary. It restates transport facts only: whether a binding exists, whether
// a collector is blocked on the inbox right now, when the slot last called the
// Service, and what is waiting for it. It never claims that a native model is
// working, idle or finished; the Summary does not know that.
func presenceLine(summary *relay.Summary, self model.ActorID, now time.Time) []string {
	if summary == nil {
		return nil
	}
	var lines []string
	for _, slot := range model.SlotActors() {
		who := "you"
		if slot != self {
			who = "peer"
		}
		b, bound := summary.Bindings[slot]
		var parts []string
		switch {
		case !bound || !b.Active:
			parts = append(parts, "not bound")
		case !b.Associated:
			parts = append(parts, "bound, no session yet")
		case b.CollectorActive:
			parts = append(parts, "waiting on its inbox now")
		default:
			parts = append(parts, "no collector waiting")
		}
		if bound && b.Active && !b.LastActivity.IsZero() {
			parts = append(parts, "last relay call "+ago(now.Sub(b.LastActivity)))
		}
		inbox := summary.Inboxes[slot]
		if inbox.Queued > 0 {
			queued := fmt.Sprintf("%d queued", inbox.Queued)
			if inbox.OldestQueuedAt != nil {
				queued += ", oldest " + ago(now.Sub(*inbox.OldestQueuedAt))
			}
			parts = append(parts, queued)
		}
		if inbox.Delivering > 0 {
			parts = append(parts, fmt.Sprintf("%d delivering", inbox.Delivering))
		}
		if inbox.Unknown > 0 {
			parts = append(parts, fmt.Sprintf("%d unknown (inspect before Retry)", inbox.Unknown))
		}
		if wake, ok := summary.LastWake[slot]; ok && inbox.Queued > 0 {
			w := "last wake " + wake.Outcome
			if wake.Reason != "" {
				w += " (" + wake.Reason + ")"
			}
			parts = append(parts, w+" "+ago(now.Sub(wake.At)))
		}
		lines = append(lines, fmt.Sprintf("%s %s: %s", who, slot, strings.Join(parts, "; ")))
	}
	return lines
}

// ago is a coarse, stable duration for a status line, not a timestamp.
func ago(d time.Duration) string {
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}
