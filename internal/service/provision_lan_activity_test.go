package service

import (
	"context"
	"errors"
	"testing"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

// Exercise the actual pinned LAN listener: the same certificate supports
// native confirmation and body-free observation, which have different wake
// meanings. No vendor wake command or model acceptance is simulated here.
func TestLANHostNativeActivityIsSeparateFromObservation(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	client, pending, _ := f.join(t)
	admitted := f.accept(t, client, pending)
	slot := admitted.Room.Slot
	ctx := context.Background()
	call := func(action string, payload, result any) {
		t.Helper()
		if err := lanshare.Call(ctx, client, f.invite, action, payload, result); err != nil {
			t.Fatalf("LAN %s: %v", action, err)
		}
	}
	initial := f.native.engine.Snapshot().Bindings[slot].LastActivity
	sequence := f.native.engine.Sequence()
	for _, action := range []string{"inspect", "room", "summary", "doctor", "wake-candidate"} {
		call(action, nil, nil)
	}
	if got := f.native.engine.Snapshot().Bindings[slot].LastActivity; !got.Equal(initial) || f.native.engine.Sequence() != sequence {
		t.Fatal("HTTP observation manufactured native activity or a durable heartbeat")
	}
	var binding relay.Binding
	call("confirm", nil, &binding)
	if !binding.LastActivity.After(initial) || binding.SessionID != "" || binding.TranscriptPath != "" || f.native.engine.Sequence() != sequence {
		t.Fatalf("native confirmation did not advance activity without exporting session data: %+v", binding)
	}
	call("head", map[string]any{"park": true, "timeout_seconds": 1}, nil)
	first, err := f.native.engine.Send(f.owner, relay.SendRequest{ID: "first-native-nudge", Text: "first"})
	if err != nil {
		t.Fatal(err)
	}
	call("wake-reserve", map[string]string{"id": first.ID}, nil)
	call("wake-record", map[string]string{"id": first.ID, "outcome": "accepted"}, nil)
	if err := f.native.engine.Cancel(first.ID); err != nil {
		t.Fatal(err)
	}
	next, err := f.native.engine.Send(f.owner, relay.SendRequest{ID: "next-native-nudge", Text: "next"})
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		Candidate *relay.WakeCandidate `json:"candidate"`
	}
	call("wake-candidate", map[string]string{"id": next.ID}, &observed)
	if observed.Candidate == nil || !observed.Candidate.NudgePending || observed.Candidate.Reserved {
		t.Fatalf("observation consumed the prior Codex nudge: %+v", observed.Candidate)
	}
	call("confirm", nil, nil)
	call("wake-candidate", map[string]string{"id": next.ID}, &observed)
	if observed.Candidate == nil || observed.Candidate.NudgePending || observed.Candidate.Reserved {
		t.Fatalf("actual native progress did not release the next burst: %+v", observed.Candidate)
	}
	call("wake-reserve", map[string]string{"id": next.ID}, nil)
	if auth, err := f.native.engine.LANAuth(binding.RemoteKey); err != nil {
		t.Fatal(err)
	} else if err := f.native.engine.ReserveLANWake(auth, first.ID); !errors.Is(err, relay.ErrWakeReserved) {
		t.Fatalf("real native progress replayed the original spent wake: %v", err)
	}
}
