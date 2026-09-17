package service

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

type wakeAuditRecord struct {
	outcome string
	reason  string
	target  model.ActorID
}

type fakeNativeWakeRelay struct {
	mu             sync.Mutex
	candidates     map[string]relay.WakeCandidate
	reservations   []relay.WakeReservation
	records        []wakeAuditRecord
	reserveErr     error
	recordErr      error
	reservationNow func() time.Time
}

func (f *fakeNativeWakeRelay) WakeCandidate(messageID string) (relay.WakeCandidate, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.candidates[messageID]
	return value, ok
}

func (f *fakeNativeWakeRelay) ReserveWake(messageID string, target model.ActorID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveErr != nil {
		return f.reserveErr
	}
	for _, value := range f.reservations {
		if value.MessageID == messageID {
			return relay.ErrWakeReserved
		}
	}
	at := time.Now().UTC()
	if f.reservationNow != nil {
		at = f.reservationNow().UTC()
	}
	f.reservations = append(f.reservations, relay.WakeReservation{MessageID: messageID, Target: target, At: at})
	return nil
}

func (f *fakeNativeWakeRelay) RecordWake(outcome, reason string, target model.ActorID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return f.recordErr
	}
	f.records = append(f.records, wakeAuditRecord{outcome: outcome, reason: reason, target: target})
	return nil
}

func (f *fakeNativeWakeRelay) WakeReservations() []relay.WakeReservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]relay.WakeReservation(nil), f.reservations...)
}

func (f *fakeNativeWakeRelay) snapshot() ([]relay.WakeReservation, []wakeAuditRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]relay.WakeReservation(nil), f.reservations...), append([]wakeAuditRecord(nil), f.records...)
}

func codexCandidate(id string) relay.WakeCandidate {
	return relay.WakeCandidate{MessageID: id, Target: model.ActorSlot2, Runtime: model.RuntimeCodex, SessionID: "vendor-session-must-not-reach-audit", Enabled: true, QueueStart: true}
}

func newTestNativeWaker(now *time.Time, relayState *fakeNativeWakeRelay, run nativeWakeRun, wait nativeWakeWait) *nativeWaker {
	relayState.mu.Lock()
	relayState.reservationNow = func() time.Time { return *now }
	relayState.mu.Unlock()
	return newNativeWaker(nativeWakerConfig{
		Relay: relayState,
		Now:   func() time.Time { return *now },
		Run:   run,
		Wait:  wait,
	})
}

func TestNativeWakerRunsOnlyFixedBodyFreeNudge(t *testing.T) {
	now := time.Date(2026, time.September, 17, 8, 0, 0, 0, time.UTC)
	state := &fakeNativeWakeRelay{candidates: map[string]relay.WakeCandidate{"message-1": codexCandidate("message-1")}}
	var commandName string
	var commandArgs []string
	waker := newTestNativeWaker(&now, state, func(_ context.Context, name string, args ...string) error {
		commandName, commandArgs = name, append([]string(nil), args...)
		return nil
	}, func(context.Context, time.Duration) error { return nil })

	if err := waker.Wake(context.Background(), "message-1"); err != nil {
		t.Fatal(err)
	}
	if commandName != "codex" {
		t.Fatalf("command = %q", commandName)
	}
	want := []string{"queue", "--thread", "vendor-session-must-not-reach-audit", "--message", nativeWakeNudge}
	if strings.Join(commandArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", commandArgs, want)
	}
	reservations, records := state.snapshot()
	if len(reservations) != 1 || reservations[0].MessageID != "message-1" || reservations[0].Target != model.ActorSlot2 {
		t.Fatalf("reservations = %#v", reservations)
	}
	if len(records) != 1 || records[0] != (wakeAuditRecord{outcome: "accepted", target: model.ActorSlot2}) {
		t.Fatalf("records = %#v", records)
	}
	type auditProjection struct {
		Outcome string        `json:"outcome"`
		Reason  string        `json:"reason,omitempty"`
		Target  model.ActorID `json:"target"`
	}
	projected := make([]auditProjection, 0, len(records))
	for _, record := range records {
		projected = append(projected, auditProjection{Outcome: record.outcome, Reason: record.reason, Target: record.target})
	}
	audit, err := json.Marshal(struct {
		Reservations []relay.WakeReservation `json:"reservations"`
		Records      []auditProjection       `json:"records"`
	}{reservations, projected})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"vendor-session-must-not-reach-audit", nativeWakeNudge, "message body"} {
		if strings.Contains(string(audit), forbidden) {
			t.Fatalf("audit leaked %q: %s", forbidden, audit)
		}
	}
}

func TestNativeWakerSuppressesIneligibleCandidates(t *testing.T) {
	now := time.Date(2026, time.September, 17, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		change func(*relay.WakeCandidate)
		reason string
	}{
		{"disabled", func(v *relay.WakeCandidate) { v.Enabled = false }, "disabled"},
		{"waiter", func(v *relay.WakeCandidate) { v.WaiterActive = true }, "waiter_active"},
		{"delivering", func(v *relay.WakeCandidate) { v.Delivering = true }, "waiter_active"},
		{"unbound", func(v *relay.WakeCandidate) { v.SessionID = "" }, "unbound"},
		{"other runtime", func(v *relay.WakeCandidate) { v.Runtime = model.RuntimeClaude }, "unsupported_runtime"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := codexCandidate("message-" + tc.name)
			tc.change(&candidate)
			state := &fakeNativeWakeRelay{candidates: map[string]relay.WakeCandidate{candidate.MessageID: candidate}}
			calls := 0
			waker := newTestNativeWaker(&now, state, func(context.Context, string, ...string) error {
				calls++
				return nil
			}, func(context.Context, time.Duration) error { return nil })
			if err := waker.Wake(context.Background(), candidate.MessageID); err != nil {
				t.Fatal(err)
			}
			_, records := state.snapshot()
			if calls != 0 || len(records) != 1 || records[0].outcome != "suppressed" || records[0].reason != tc.reason {
				t.Fatalf("calls=%d records=%#v", calls, records)
			}
		})
	}
}

func TestNativeWakerMergesBurstWithoutExtraAudit(t *testing.T) {
	now := time.Date(2026, time.September, 17, 8, 0, 0, 0, time.UTC)
	state := &fakeNativeWakeRelay{candidates: map[string]relay.WakeCandidate{
		"later-burst": {MessageID: "later-burst", Target: model.ActorSlot2, Runtime: model.RuntimeCodex, SessionID: "session", Enabled: true},
	}}
	waker := newTestNativeWaker(&now, state, func(context.Context, string, ...string) error {
		t.Fatal("burst follower must not run a vendor command")
		return nil
	}, func(context.Context, time.Duration) error {
		t.Fatal("burst follower must not start a grace wait")
		return nil
	})
	if err := waker.Wake(context.Background(), "later-burst"); err != nil {
		t.Fatal(err)
	}
	reservations, records := state.snapshot()
	if len(reservations) != 0 || len(records) != 0 {
		t.Fatalf("burst follower created audit: reservations=%#v records=%#v", reservations, records)
	}
}

func TestNativeWakerKeepsOnePendingGracePerTarget(t *testing.T) {
	now := time.Date(2026, time.September, 17, 8, 0, 0, 0, time.UTC)
	state := &fakeNativeWakeRelay{candidates: map[string]relay.WakeCandidate{
		"first":  codexCandidate("first"),
		"second": codexCandidate("second"),
	}}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls int
	waker := newTestNativeWaker(&now, state, func(context.Context, string, ...string) error {
		calls++
		return nil
	}, func(context.Context, time.Duration) error {
		started <- struct{}{}
		<-release
		return nil
	})
	firstDone := make(chan error, 1)
	go func() { firstDone <- waker.Wake(context.Background(), "first") }()
	<-started
	if err := waker.Wake(context.Background(), "second"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	reservations, records := state.snapshot()
	if calls != 1 || len(reservations) != 1 || len(records) != 1 || records[0].outcome != "accepted" {
		t.Fatalf("calls=%d reservations=%#v records=%#v", calls, reservations, records)
	}
}

func TestNativeWakerGraceSkipsMessageCollectedByPark(t *testing.T) {
	now := time.Date(2026, time.September, 17, 8, 0, 0, 0, time.UTC)
	state := &fakeNativeWakeRelay{candidates: map[string]relay.WakeCandidate{"message-park": codexCandidate("message-park")}}
	calls := 0
	waker := newTestNativeWaker(&now, state, func(context.Context, string, ...string) error {
		calls++
		return nil
	}, func(context.Context, time.Duration) error {
		state.mu.Lock()
		delete(state.candidates, "message-park")
		state.mu.Unlock()
		return nil
	})
	if err := waker.Wake(context.Background(), "message-park"); err != nil {
		t.Fatal(err)
	}
	reservations, records := state.snapshot()
	if calls != 0 || len(reservations) != 0 || len(records) != 1 || records[0] != (wakeAuditRecord{outcome: "suppressed", reason: "collected", target: model.ActorSlot2}) {
		t.Fatalf("calls=%d reservations=%#v records=%#v", calls, reservations, records)
	}
}

func TestNativeWakerLimitsAndDeduplicatesAcrossRestart(t *testing.T) {
	now := time.Date(2026, time.September, 17, 8, 0, 0, 0, time.UTC)
	state := &fakeNativeWakeRelay{candidates: map[string]relay.WakeCandidate{}}
	for _, id := range []string{"first", "interval", "over-hour"} {
		state.candidates[id] = codexCandidate(id)
	}
	calls := 0
	run := func(context.Context, string, ...string) error {
		calls++
		return nil
	}
	waker := newTestNativeWaker(&now, state, run, func(context.Context, time.Duration) error { return nil })
	if err := waker.Wake(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	if err := waker.Wake(context.Background(), "interval"); err != nil {
		t.Fatal(err)
	}
	_, records := state.snapshot()
	if records[len(records)-1].reason != "minimum_interval" {
		t.Fatalf("minimum interval record = %#v", records[len(records)-1])
	}
	now = now.Add(31 * time.Second)
	restarted := newTestNativeWaker(&now, state, run, func(context.Context, time.Duration) error { return nil })
	if err := restarted.Wake(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	_, records = state.snapshot()
	if records[len(records)-1].reason != "duplicate" {
		t.Fatalf("duplicate record = %#v", records[len(records)-1])
	}
	for i := 0; i < nativeWakeHourlyLimit-1; i++ {
		id := "hour-" + string(rune('a'+i))
		state.mu.Lock()
		state.candidates[id] = codexCandidate(id)
		state.mu.Unlock()
		now = now.Add(nativeWakeMinimumInterval + time.Second)
		if err := restarted.Wake(context.Background(), id); err != nil {
			t.Fatalf("wake %d: %v", i, err)
		}
	}
	now = now.Add(nativeWakeMinimumInterval + time.Second)
	if err := restarted.Wake(context.Background(), "over-hour"); err != nil {
		t.Fatal(err)
	}
	_, records = state.snapshot()
	if records[len(records)-1].reason != "hourly_limit" {
		t.Fatalf("hourly record = %#v", records[len(records)-1])
	}
	if calls != nativeWakeHourlyLimit {
		t.Fatalf("runner calls = %d, want %d", calls, nativeWakeHourlyLimit)
	}
}

func TestNativeWakerReservesRateSlotAtomically(t *testing.T) {
	now := time.Date(2026, time.September, 17, 8, 0, 0, 0, time.UTC)
	var ticks atomic.Int64
	state := &fakeNativeWakeRelay{candidates: map[string]relay.WakeCandidate{
		"concurrent-a": codexCandidate("concurrent-a"),
		"concurrent-b": codexCandidate("concurrent-b"),
	}}
	var calls int
	var callsMu sync.Mutex
	waker := newNativeWaker(nativeWakerConfig{
		Relay:       state,
		Now:         func() time.Time { return now.Add(time.Duration(ticks.Add(1)) * time.Second) },
		MinInterval: time.Millisecond,
		HourlyLimit: 1,
		Run: func(context.Context, string, ...string) error {
			callsMu.Lock()
			calls++
			callsMu.Unlock()
			return nil
		},
		Wait: func(context.Context, time.Duration) error { return nil },
	})
	var group sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"concurrent-a", "concurrent-b"} {
		group.Add(1)
		go func(messageID string) {
			defer group.Done()
			errs <- waker.Wake(context.Background(), messageID)
		}(id)
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	callsMu.Lock()
	gotCalls := calls
	callsMu.Unlock()
	if gotCalls != 1 {
		t.Fatalf("runner calls = %d, want 1", gotCalls)
	}
	_, records := state.snapshot()
	var accepted, hourlySuppressed int
	for _, record := range records {
		switch {
		case record.outcome == "accepted":
			accepted++
		case record.outcome == "suppressed" && record.reason == "hourly_limit":
			hourlySuppressed++
		default:
			t.Fatalf("unexpected audit record %#v", record)
		}
	}
	// Both legitimate schedules preserve the atomicity invariant (exactly one
	// vendor command, asserted above): a serialized loser reaches the hourly
	// cap and records one suppression, while a truly concurrent loser is
	// swallowed by the per-target pending dedup, which by design records no
	// audit fact. Pinning the record shape would make the test race-prone.
	if accepted != 1 || hourlySuppressed > 1 {
		t.Fatalf("records = %#v", records)
	}
}

func TestNativeWakerFailsClosedBeforeVendorCommandWhenReservationFails(t *testing.T) {
	now := time.Date(2026, time.September, 17, 8, 0, 0, 0, time.UTC)
	state := &fakeNativeWakeRelay{candidates: map[string]relay.WakeCandidate{"audit-failure": codexCandidate("audit-failure")}, reserveErr: errors.New("event store unavailable")}
	calls := 0
	waker := newTestNativeWaker(&now, state, func(context.Context, string, ...string) error {
		calls++
		return nil
	}, func(context.Context, time.Duration) error { return nil })
	if err := waker.Wake(context.Background(), "audit-failure"); !errors.Is(err, errNativeWakeAudit) {
		t.Fatalf("wake error = %v", err)
	}
	if calls != 0 {
		t.Fatal("vendor command ran without a durable reservation")
	}
}

func TestNativeWakerRedactsVendorFailure(t *testing.T) {
	now := time.Date(2026, time.September, 17, 8, 0, 0, 0, time.UTC)
	state := &fakeNativeWakeRelay{candidates: map[string]relay.WakeCandidate{"command-failure": codexCandidate("command-failure")}}
	waker := newTestNativeWaker(&now, state, func(context.Context, string, ...string) error {
		return &exec.Error{Name: "codex", Err: errors.New("vendor-session-must-not-reach-audit")}
	}, func(context.Context, time.Duration) error { return nil })
	if err := waker.Wake(context.Background(), "command-failure"); err != nil {
		t.Fatal(err)
	}
	_, records := state.snapshot()
	if len(records) != 1 || records[0] != (wakeAuditRecord{outcome: "failed", reason: "command_unavailable", target: model.ActorSlot2}) {
		t.Fatalf("records = %#v", records)
	}
}
