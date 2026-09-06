package room

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func telemetryProjectionFixture() *Engine {
	e := &Engine{}
	for i := 0; i < 1000; i++ {
		e.snapshot.Turns = append(e.snapshot.Turns, model.TurnSummary{ID: "historical"})
	}
	summary := model.TurnSummary{ID: "claude:active", TurnID: "active", Agent: model.ActorClaude,
		Status: "working", MessageIDs: []string{"message"}, Usage: json.RawMessage(`{"tokens":42}`)}
	for i := 0; i < 128; i++ {
		summary.Items = append(summary.Items, model.TurnWorkItem{ID: "tool", Data: json.RawMessage(`{"command":"go test ./..."}`)})
	}
	e.snapshot.Turns = append(e.snapshot.Turns, summary)
	return e
}

func TestNonProjectingTelemetryDoesNotCloneTurnHistory(t *testing.T) {
	e := telemetryProjectionFixture()
	before, _ := json.Marshal(e.snapshot)
	for _, kind := range []string{model.RuntimeTextDelta, model.RuntimeState, model.RuntimeSession, model.RuntimeInfoUpdated} {
		event := model.RuntimeEvent{Agent: model.ActorClaude, TurnID: "active", Kind: kind, Text: "delta"}
		if allocs := testing.AllocsPerRun(10, func() { e.projectTurnSummary(event) }); allocs != 0 {
			t.Errorf("%s allocated %v times despite not projecting a summary", kind, allocs)
		}
	}
	after, _ := json.Marshal(e.snapshot)
	if !bytes.Equal(before, after) {
		t.Fatal("ignored telemetry mutated turn history")
	}
}

func TestSnapshotOwnsTimestampPointers(t *testing.T) {
	now := time.Now().UTC()
	resolved, completed, itemCompleted := now, now, now
	e := &Engine{snapshot: model.RoomSnapshot{
		Approvals: []model.Approval{{ID: "approval", ResolvedAt: &resolved}},
		Turns:     []model.TurnSummary{{ID: "turn", CompletedAt: &completed, Items: []model.TurnWorkItem{{CompletedAt: &itemCompleted}}}},
	}}
	for _, snapshot := range []model.RoomSnapshot{e.Snapshot(), e.WindowedSnapshot(1)} {
		*snapshot.Approvals[0].ResolvedAt = time.Time{}
		*snapshot.Turns[0].CompletedAt = time.Time{}
		*snapshot.Turns[0].Items[0].CompletedAt = time.Time{}
	}
	if resolved != now || completed != now || itemCompleted != now {
		t.Fatal("snapshot timestamp mutations reached engine authority")
	}
}

func TestRecentEventTailIsOrderedAndReleasesEvictedPayloads(t *testing.T) {
	e := &Engine{}
	for i := 1; i <= recentEventLimit*4; i++ {
		event := model.Event{Seq: uint64(i), Kind: "fixture", Data: json.RawMessage(`{"body":"retained only within tail"}`)}
		e.mu.Lock()
		if err := e.applyLocked(event); err != nil {
			t.Fatal(err)
		}
		e.mu.Unlock()
		if len(e.snapshot.Events) > recentEventLimit {
			t.Fatal("tail exceeded bound")
		}
	}
	events, latest := e.ReplayEvents()
	if len(events) != recentEventLimit || latest != uint64(recentEventLimit*4) {
		t.Fatal("incorrect replay window")
	}
	for i, event := range events {
		if event.Seq != uint64(recentEventLimit*3+i+1) {
			t.Fatalf("tail out of order: %+v", event)
		}
	}
}

func TestEvictedEventDoesNotKeepPayloadInBackingArray(t *testing.T) {
	e := &Engine{}
	// Leave capacity for the next append, so this assertion checks the exact
	// backing slot rather than an old array abandoned by slice growth.
	e.snapshot.Events = make([]model.Event, recentEventLimit, recentEventLimit+1)
	e.snapshot.Events[0].Data = json.RawMessage(`{"large":"payload"}`)
	backing := e.snapshot.Events
	if err := e.applyLocked(model.Event{Seq: 42, Kind: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if backing[0].Data != nil {
		t.Fatal("evicted payload is retained by the live tail's backing array")
	}
}

func BenchmarkTextDeltaSummaryProjection(b *testing.B) {
	e := telemetryProjectionFixture()
	event := model.RuntimeEvent{Agent: model.ActorClaude, TurnID: "active", Kind: model.RuntimeTextDelta, Text: "delta"}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		e.projectTurnSummary(event)
	}
}

func BenchmarkRecentEventTail(b *testing.B) {
	e := &Engine{}
	event := model.Event{Kind: "fixture", Data: json.RawMessage(`{"body":"fixture"}`)}
	for i := 0; i < recentEventLimit; i++ {
		_ = e.applyLocked(event)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = e.applyLocked(event)
	}
}
