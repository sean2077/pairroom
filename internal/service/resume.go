package service

import (
	"context"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/room"
)

// ResumeResult names what one startup resume pass requested. Activation is a
// request: capacity, a missing Project or a failed start still surface through
// ordinary runtime status rather than here.
type ResumeResult struct {
	Requested []string `json:"requested"`
	Skipped   int      `json:"skipped"`
}

// ResumePendingRooms requests activation for suspended Rooms whose durable
// facts show work that needs an active Room Runtime: an Embedded
// Room-owned FIFO input that never crossed native submission, or a wake-enabled
// Native slot with unattempted queued input for its bound session. Without this
// a restarted Service leaves both wake and waiting-input notifications idle
// until someone opens the Room or a relay call arrives. Native input does not
// require a supported external wake transport to qualify for startup recovery.
//
// Detection replays each Event Log read-only; nothing is appended before
// activation. Activation itself applies the existing restore rules: accepted or
// uncertain input is never replayed, and wake keeps its reservation and rate
// limits. Embedded Rooms still queue behind the capacity limit. Archived Rooms,
// unavailable Projects and unreadable logs are skipped.
func ResumePendingRooms(ctx context.Context, registry *Registry, runtimes *RuntimeManager) ResumeResult {
	var result ResumeResult
	snapshot := registry.Snapshot(false)
	available := map[string]bool{}
	for _, project := range snapshot.Projects {
		available[project.ID] = project.Available
	}
	for _, durable := range snapshot.Rooms {
		if ctx.Err() != nil {
			return result
		}
		if durable.Archived() || !available[durable.ProjectID] || runtimes.Status(durable.ID).Phase != RuntimeSuspended {
			continue
		}
		pending, err := roomHasResumableWork(durable)
		if err != nil || !pending {
			if err != nil {
				result.Skipped++
			}
			continue
		}
		if _, err := runtimes.RequestActivation(durable.ID); err != nil {
			result.Skipped++
			continue
		}
		result.Requested = append(result.Requested, durable.ID)
	}
	return result
}

func roomHasResumableWork(durable Room) (bool, error) {
	events, err := readEventsReadOnly(filepath.Join(durable.DataDir, "events.jsonl"))
	if err != nil {
		return false, err
	}
	if durable.HostMode == model.HostNative {
		kinds := map[model.ActorID]model.RuntimeKind{}
		for actor, selection := range durable.Agents {
			kinds[actor] = selection.Runtime
		}
		var sharedSlot model.ActorID
		if durable.Sharing == "lan" {
			sharedSlot = model.OtherParticipant(durable.OwnerSlot)
		}
		return relay.HasWakeWorkWithSharedSlot(durable.ID, events, kinds, sharedSlot)
	}
	return room.HasRecoverableWork(events)
}
