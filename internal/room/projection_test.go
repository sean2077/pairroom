package room

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
)

func TestWindowedProjectionIsDetachedAndComplete(t *testing.T) {
	engine := projectionBenchmarkEngine(12)
	engine.snapshot.Events = []model.Event{{Seq: 12, Data: json.RawMessage(`{"value":1}`)}}
	engine.snapshot.LatestSeq = 12
	engine.snapshot.Approvals = []model.Approval{{Status: "pending", Detail: json.RawMessage(`{"question":1}`)}}
	engine.snapshot.Turns = []model.TurnSummary{{ID: "turn", MessageIDs: []string{"message-0"}}}
	window := engine.WindowedSnapshot(3)
	if window.MessageWindow.Total != 12 || window.MessageWindow.Loaded != 3 || window.MessageWindow.OldestSeq != 10 || !window.MessageWindow.HasMore {
		t.Fatalf("incorrect page metadata: %+v", window.MessageWindow)
	}
	if len(window.Turns) != 1 || len(window.Approvals) != 1 || len(window.Events) != 1 {
		t.Fatal("windowing must retain non-transcript state")
	}
	window.Messages[0].To[0] = model.ActorClaude
	window.Messages[0].Delivery[model.ActorCodex] = model.DeliveryFailed
	window.Events[0].Data[0] = '!'
	window.Approvals[0].Detail[0] = '!'
	window.Turns[0].MessageIDs[0] = "changed"
	fresh := engine.Snapshot()
	if len(fresh.Messages) != 12 || fresh.Messages[9].To[0] != model.ActorCodex || fresh.Messages[9].Delivery[model.ActorCodex] != model.DeliveryQueued {
		t.Fatal("window mutation reached authoritative transcript")
	}
	if !json.Valid(fresh.Events[0].Data) || !json.Valid(fresh.Approvals[0].Detail) || fresh.Turns[0].MessageIDs[0] != "message-0" {
		t.Fatal("window mutation reached non-transcript projection")
	}
}

func TestWindowedProjectionAllocationDoesNotScaleWithHiddenHistory(t *testing.T) {
	small, large := projectionBenchmarkEngine(250), projectionBenchmarkEngine(10000)
	smallAllocs := testing.AllocsPerRun(3, func() { _ = small.WindowedSnapshot(250) })
	largeAllocs := testing.AllocsPerRun(3, func() { _ = large.WindowedSnapshot(250) })
	if largeAllocs > smallAllocs+5 {
		t.Fatalf("hidden history increased page allocations: small=%v large=%v", smallAllocs, largeAllocs)
	}
}

func TestMessagesPageSparseSequenceBoundaries(t *testing.T) {
	engine := &Engine{snapshot: model.RoomSnapshot{Messages: []model.Message{{Seq: 2}, {Seq: 7}, {Seq: 11}}}}
	for _, test := range []struct {
		cursor uint64
		want   []uint64
	}{
		{0, []uint64{2, 7, 11}}, {1, []uint64{}}, {2, []uint64{}},
		{6, []uint64{2}}, {7, []uint64{2}}, {8, []uint64{2, 7}}, {12, []uint64{2, 7, 11}},
	} {
		page := engine.MessagesPage(test.cursor, 100)
		got := make([]uint64, 0, len(page.Messages))
		for _, message := range page.Messages {
			got = append(got, message.Seq)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("cursor %d: got %v want %v", test.cursor, got, test.want)
		}
	}
}

func TestReplayEventsDetachedAndConsistent(t *testing.T) {
	engine := projectionBenchmarkEngine(10000)
	engine.snapshot.Events = []model.Event{{Seq: 7, Data: json.RawMessage(`{"ok":true}`)}}
	engine.snapshot.LatestSeq = 7
	events, latest := engine.ReplayEvents()
	if latest != 7 || len(events) != 1 {
		t.Fatalf("unexpected tail: %v, %d", events, latest)
	}
	events[0].Data[0] = '!'
	fresh, _ := engine.ReplayEvents()
	if !json.Valid(fresh[0].Data) {
		t.Fatal("replay shares mutable event data")
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for i := uint64(8); i < 100; i++ {
			engine.mu.Lock()
			engine.snapshot.Events[0].Seq = i
			engine.snapshot.LatestSeq = i
			engine.mu.Unlock()
		}
	}()
	for i := 0; i < 100; i++ {
		events, latest := engine.ReplayEvents()
		if events[0].Seq != latest {
			t.Errorf("tail and cursor crossed projection boundaries")
		}
		_ = engine.WindowedSnapshot(1)
		_ = engine.Busy()
	}
	workers.Wait()
}

func TestBusyPreservesControlPlaneActivitySemantics(t *testing.T) {
	for _, state := range []model.AgentState{model.StateStarting, model.StateWorking, model.StateWaiting} {
		engine := &Engine{snapshot: model.RoomSnapshot{Participants: map[model.ActorID]model.ParticipantSnapshot{model.ActorClaude: {State: state}}}}
		if !engine.Busy() {
			t.Errorf("%s must be busy", state)
		}
	}
	for _, snapshot := range []model.RoomSnapshot{
		{Participants: map[model.ActorID]model.ParticipantSnapshot{model.ActorClaude: {CurrentTurn: "native-turn"}}},
		{Approvals: []model.Approval{{Status: "pending"}}},
		{Messages: []model.Message{{Processing: map[model.ActorID]model.ProcessingState{model.ActorCodex: model.ProcessingWaiting}}}},
		{Messages: []model.Message{{Processing: map[model.ActorID]model.ProcessingState{model.ActorCodex: model.ProcessingWorking}}}},
	} {
		if !(&Engine{snapshot: snapshot}).Busy() {
			t.Errorf("lost activity: %+v", snapshot)
		}
	}
	engine := projectionBenchmarkEngine(10000)
	if engine.Busy() {
		t.Fatal("terminal processing is not active work")
	}
	if allocations := testing.AllocsPerRun(5, func() { _ = engine.Busy() }); allocations != 0 {
		t.Fatalf("management poll clones history: %v allocations", allocations)
	}
}

func TestBoundedTailPreservesUnicodeAndByteBudget(t *testing.T) {
	for _, value := range []string{"plain text", "中文审查结果", "review ✅ 🔎 complete", "a界🙂b"} {
		for limit := 1; limit <= len(value)+1; limit++ {
			got := boundedTail(value, limit)
			if !utf8.ValidString(got) || len(got) > limit {
				t.Errorf("%q limit=%d produced invalid/bloated %q (%d bytes)", value, limit, got, len(got))
			}
			if !strings.HasSuffix(value, strings.TrimPrefix(got, "…")) {
				t.Errorf("tail content changed: %q -> %q", value, got)
			}
		}
	}
}
