package room

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/bus"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

type loggedEvent struct {
	Seq  uint64          `json:"seq"`
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

func readEventLog(t *testing.T, dir string) []loggedEvent {
	t.Helper()
	file, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []loggedEvent
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		var event loggedEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func summaryRecords(t *testing.T, dir string) (count, bytes int) {
	t.Helper()
	for _, event := range readEventLog(t, dir) {
		if event.Kind == EventTurnSummaryUpdated {
			count++
			bytes += len(event.Data)
		}
	}
	return count, bytes
}

func reopenEngine(t *testing.T, dir string) *Engine {
	t.Helper()
	reopenedStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(Config{Name: "ignored", Repo: t.TempDir(), Store: reopenedStore, Settings: model.DefaultRoomSettings()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	return reopened
}

func toolPayload(i int) json.RawMessage {
	payload, _ := json.Marshal(map[string]any{"index": i, "output": strings.Repeat("x", 8<<10)})
	return payload
}

// A long tool-heavy Turn must checkpoint on an interval instead of rewriting
// the complete summary on every tool event.
func TestTurnSummaryCheckpointsBoundLogGrowth(t *testing.T) {
	dir := t.TempDir()
	engine, _ := newTestEngine(t, dir)
	started := time.Now().UTC().Add(-time.Hour)
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeTurnStarted, TurnID: "long", CreatedAt: started})
	const pairs = 100
	span := 5 * time.Minute
	step := span / (2 * pairs)
	at := started
	for i := 0; i < pairs; i++ {
		item := fmt.Sprintf("tool-%03d", i)
		at = at.Add(step)
		engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeToolStarted, TurnID: "long", ItemID: item, Name: "Bash", Data: toolPayload(i), CreatedAt: at})
		at = at.Add(step)
		engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeToolCompleted, TurnID: "long", ItemID: item, Name: "Bash", Text: "done", Data: toolPayload(i), CreatedAt: at})
	}
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeFinal, TurnID: "long", Text: "finished", CreatedAt: at.Add(time.Second)})
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeTurnCompleted, TurnID: "long", CreatedAt: at.Add(2 * time.Second)})

	count, bytes := summaryRecords(t, dir)
	// created+started share one record, one checkpoint per interval, then final and completion.
	maxRecords := 1 + int(span/turnSummaryCheckpointInterval) + 1 + 2
	if count > maxRecords {
		t.Fatalf("summary records = %d, want <= %d", count, maxRecords)
	}
	// 100 items of metadata per checkpoint, not 100 copies of 8 KiB payloads.
	if bytes > 512<<10 {
		t.Fatalf("summary bytes = %d, want < 512 KiB", bytes)
	}
	got := engine.Snapshot().Turns[0]
	if got.Status != "completed" || got.FinalText != "finished" || len(got.Items) != pairs {
		t.Fatalf("final summary incomplete: status=%s final=%q items=%d", got.Status, got.FinalText, len(got.Items))
	}
	for _, item := range got.Items {
		// Durable tool evidence is referenced, not copied into checkpoints.
		if item.Data != nil || len(item.SourceSeqs) != 2 || len(item.Detail) > turnItemPreviewLimit {
			t.Fatalf("item %s kept inline evidence: data=%d seqs=%v detail=%d", item.ID, len(item.Data), item.SourceSeqs, len(item.Detail))
		}
	}
	encoded, _ := json.Marshal(got)
	if len(encoded) > turnSummaryByteBudget {
		t.Fatalf("summary encodes to %d bytes, budget %d", len(encoded), turnSummaryByteBudget)
	}
	// Raw tool facts remain complete even though the summary copy is bounded.
	tools := 0
	for _, event := range readEventLog(t, dir) {
		if event.Kind == EventRuntime && strings.Contains(string(event.Data), `"kind":"tool.`) {
			tools++
		}
	}
	if tools != 2*pairs {
		t.Fatalf("durable raw tool events = %d, want %d", tools, 2*pairs)
	}
}

func jsonInt(i int) string { return fmt.Sprint(i) }

// High-frequency diff/usage telemetry stays off disk and is throttled live.
func TestTransientTelemetryDoesNotPersistSummaries(t *testing.T) {
	dir := t.TempDir()
	eventStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hub := bus.New(4096)
	engine, err := New(Config{Name: "test", Repo: t.TempDir(), Store: eventStore, Hub: hub, Settings: model.RoomSettings{StallWarningSeconds: 300}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	events, cancel := engine.Subscribe()
	defer cancel()
	started := time.Now().UTC()
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot2, Kind: model.RuntimeTurnStarted, TurnID: "busy", CreatedAt: started})
	before, _ := summaryRecords(t, dir)
	for i := 0; i < 1000; i++ {
		at := started.Add(time.Duration(i) * 10 * time.Millisecond) // 10 s total, below the checkpoint interval
		kind := model.RuntimeDiffUpdated
		if i%2 == 1 {
			kind = model.RuntimeUsageUpdated
		}
		engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot2, Kind: kind, TurnID: "busy", Text: "diff", Data: json.RawMessage(`{"tokens":1}`), CreatedAt: at})
	}
	after, _ := summaryRecords(t, dir)
	if after != before {
		t.Fatalf("diff/usage persisted %d extra summary records", after-before)
	}
	transient := 0
drain:
	for {
		select {
		case event := <-events:
			if event.Kind == EventTurnSummaryUpdated && event.Seq == 0 {
				transient++
			}
		default:
			break drain
		}
	}
	if transient == 0 || transient > 11 {
		t.Fatalf("transient summaries = %d, want 1..11 for a 10 s burst", transient)
	}
}

func TestTurnSummaryCloseFlushesAndCrashKeepsLastCheckpoint(t *testing.T) {
	started := time.Now().UTC().Add(-time.Hour)
	feed := func(engine *Engine) {
		engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeTurnStarted, TurnID: "open", CreatedAt: started})
		engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeToolStarted, TurnID: "open", ItemID: "a", Name: "Read", CreatedAt: started.Add(time.Second)})
		engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeToolCompleted, TurnID: "open", ItemID: "a", Name: "Read", CreatedAt: started.Add(2 * time.Second)})
	}

	t.Run("clean close persists dirty items", func(t *testing.T) {
		dir := t.TempDir()
		engine, _ := newTestEngine(t, dir)
		feed(engine)
		if err := engine.Close(); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		got := reopenEngine(t, dir).Snapshot().Turns
		if len(got) != 1 || len(got[0].Items) != 1 || got[0].Items[0].Status != "completed" {
			t.Fatalf("close lost dirty summary: %#v", got)
		}
	})

	t.Run("crash keeps last checkpoint and raw facts", func(t *testing.T) {
		dir := t.TempDir()
		eventStore, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		engine, err := New(Config{Name: "test", Repo: t.TempDir(), Store: eventStore, Settings: model.RoomSettings{StallWarningSeconds: 300}})
		if err != nil {
			t.Fatal(err)
		}
		feed(engine)
		// Simulate process death: release the file without Engine.Close.
		if err := eventStore.Close(); err != nil {
			t.Fatal(err)
		}
		got := reopenEngine(t, dir).Snapshot().Turns
		if len(got) != 1 || got[0].Status != "working" || len(got[0].Items) != 0 {
			t.Fatalf("crash should retain only the turn.started checkpoint: %#v", got)
		}
		tools := 0
		for _, event := range readEventLog(t, dir) {
			if event.Kind == EventRuntime && strings.Contains(string(event.Data), `"kind":"tool.`) {
				tools++
			}
		}
		if tools != 2 {
			t.Fatalf("raw tool facts = %d, want 2", tools)
		}
	})
}

func TestTurnSummaryQuietCheckpointAndStopFlush(t *testing.T) {
	dir := t.TempDir()
	engine, _ := newTestEngine(t, dir)
	started := time.Now().UTC().Add(-time.Hour)
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot2, Kind: model.RuntimeTurnStarted, TurnID: "quiet", CreatedAt: started})
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot2, Kind: model.RuntimeToolStarted, TurnID: "quiet", ItemID: "a", CreatedAt: started.Add(time.Second)})
	before, _ := summaryRecords(t, dir)
	if err := engine.flushTurnSummaries("", started.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if count, _ := summaryRecords(t, dir); count != before {
		t.Fatal("checkpoint persisted before its interval elapsed")
	}
	if err := engine.flushTurnSummaries(model.ActorSlot1, started.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if count, _ := summaryRecords(t, dir); count != before {
		t.Fatal("flush for another participant persisted this participant's summary")
	}
	if err := engine.flushTurnSummaries("", started.Add(turnSummaryCheckpointInterval)); err != nil {
		t.Fatal(err)
	}
	if count, _ := summaryRecords(t, dir); count != before+1 {
		t.Fatal("due quiet checkpoint was not persisted")
	}
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot2, Kind: model.RuntimeToolCompleted, TurnID: "quiet", ItemID: "a", CreatedAt: started.Add(turnSummaryCheckpointInterval + time.Second)})
	if err := engine.StopAgent(context.Background(), model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	if count, _ := summaryRecords(t, dir); count != before+2 {
		t.Fatal("adapter stop did not flush the dirty summary")
	}
}

func TestTurnSummaryFlushFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	eventStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Config{Name: "test", Repo: t.TempDir(), Store: eventStore, Settings: model.RoomSettings{StallWarningSeconds: 300}})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeTurnStarted, TurnID: "fail", CreatedAt: started})
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeToolStarted, TurnID: "fail", ItemID: "a", CreatedAt: started.Add(time.Second)})
	if err := eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := engine.flushTurnSummaries("", time.Time{}); err == nil {
		t.Fatal("checkpoint write failure was swallowed")
	}
	if engine.Fatal() == nil {
		t.Fatal("checkpoint write failure did not mark the Room store fatal")
	}
	if got := engine.Snapshot().Turns[0]; len(got.Items) != 1 {
		t.Fatalf("failed checkpoint discarded the in-memory projection: %#v", got)
	}
}

// A log written by the previous every-event persistence replays unchanged.
func TestLegacyFrequentSummaryLogReplays(t *testing.T) {
	dir := t.TempDir()
	eventStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Config{Name: "legacy", Repo: t.TempDir(), Store: eventStore, Settings: model.RoomSettings{StallWarningSeconds: 300}})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC()
	summary := model.TurnSummary{ID: "slot1:legacy", Agent: model.ActorSlot1, TurnID: "legacy", Status: "working", StartedAt: started}
	for i := 0; i < 20; i++ {
		summary.UpdatedAt = started.Add(time.Duration(i) * time.Second)
		summary.Items = append(summary.Items, model.TurnWorkItem{ID: "tool-" + jsonInt(i), Kind: "tool", Status: "completed", Data: json.RawMessage(`{"body":"` + strings.Repeat("y", 16<<10-64) + `"}`)})
		if _, err := engine.record(EventTurnSummaryUpdated, model.ActorSlot1, summary); err != nil {
			t.Fatal(err)
		}
	}
	want := engine.Snapshot().Turns
	if err := engine.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	got := reopenEngine(t, dir).Snapshot().Turns
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		t.Fatal("legacy summary log replayed differently")
	}
}

func TestWindowedSnapshotBoundsTurnsAndSummaryEvents(t *testing.T) {
	engine := projectionBenchmarkEngine(1000)
	base := time.Now().UTC()
	for i := 0; i < 200; i++ {
		completed := base.Add(time.Duration(i) * time.Second)
		turn := model.TurnSummary{ID: "slot1:t" + jsonInt(i), Agent: model.ActorSlot1, TurnID: "t" + jsonInt(i),
			Status: "completed", StartedAt: completed, UpdatedAt: completed, CompletedAt: &completed}
		if i == 3 {
			turn.MessageIDs = []string{engine.snapshot.Messages[999].ID} // old Turn tied to a visible message
		}
		if i == 5 {
			turn.CompletedAt, turn.Status = nil, "working" // old Turn the participant still owns
		}
		if i == 7 {
			// An adapter that died before turn.completed leaves an orphaned
			// incomplete summary; it must not be pinned into every snapshot.
			turn.CompletedAt, turn.Status = nil, "cancelled"
		}
		engine.snapshot.Turns = append(engine.snapshot.Turns, turn)
	}
	engine.snapshot.Participants = map[model.ActorID]model.ParticipantSnapshot{
		model.ActorSlot1: {CurrentTurn: "t5"},
	}
	engine.snapshot.Events = []model.Event{
		{Seq: 1, Kind: EventTurnSummaryUpdated, Data: json.RawMessage(`{}`)},
		{Seq: 2, Kind: EventSystemNotice, Data: json.RawMessage(`{}`)},
	}
	window := engine.WindowedSnapshot(250)
	if window.TurnWindow == nil || window.TurnWindow.Total != 200 || window.TurnWindow.Loaded != len(window.Turns) {
		t.Fatalf("turn window metadata = %#v (turns %d)", window.TurnWindow, len(window.Turns))
	}
	if len(window.Turns) != snapshotTurnWindow+2 {
		t.Fatalf("turns = %d, want newest %d plus correlated and unfinished", len(window.Turns), snapshotTurnWindow)
	}
	ids := map[string]bool{}
	for _, turn := range window.Turns {
		ids[turn.TurnID] = true
	}
	if !ids["t3"] || !ids["t5"] || !ids["t199"] || ids["t100"] || ids["t7"] {
		t.Fatalf("unexpected turn selection: %v", ids)
	}
	for _, event := range window.Events {
		if event.Kind == EventTurnSummaryUpdated {
			t.Fatal("windowed snapshot repeated turn summary payloads in its event tail")
		}
	}
	if len(window.Events) != 1 {
		t.Fatalf("events = %d, want the non-summary event", len(window.Events))
	}

	page := engine.MessagesPage(engine.snapshot.Messages[999].Seq+1, 1)
	if len(page.Turns) != 1 || page.Turns[0].TurnID != "t3" {
		t.Fatalf("message page turns = %#v", page.Turns)
	}
}

func TestRecentEventTailIsByteBounded(t *testing.T) {
	e := &Engine{}
	large := json.RawMessage(`"` + strings.Repeat("z", 256<<10) + `"`)
	for i := 1; i <= 100; i++ {
		e.mu.Lock()
		if err := e.applyLocked(model.Event{Seq: uint64(i), Kind: "fixture", Data: large}); err != nil {
			t.Fatal(err)
		}
		e.mu.Unlock()
	}
	total := 0
	for _, event := range e.snapshot.Events {
		total += len(event.Data)
	}
	if total > recentEventBytes || total != e.recentEventDataBytes {
		t.Fatalf("tail payload %d (tracked %d) exceeds %d", total, e.recentEventDataBytes, recentEventBytes)
	}
	if last := e.snapshot.Events[len(e.snapshot.Events)-1]; last.Seq != 100 {
		t.Fatalf("tail lost the newest event: %d", last.Seq)
	}
	events, latest := e.ReplayEvents()
	if latest != 100 || !replayNeedsResetForTest(events, 1, latest) {
		t.Fatal("a cursor older than the byte-bounded tail must require a snapshot reset")
	}
	oversized := &Engine{}
	if err := oversized.applyLocked(model.Event{Seq: 1, Kind: "fixture", Data: json.RawMessage(`"` + strings.Repeat("q", recentEventBytes) + `"`)}); err != nil {
		t.Fatal(err)
	}
	if len(oversized.snapshot.Events) != 1 {
		t.Fatal("the newest event must stay in the tail even when it alone exceeds the budget")
	}
}

// replayNeedsResetForTest mirrors the server's reset rule: the first retained
// durable sequence must directly follow the client's cursor.
func replayNeedsResetForTest(events []model.Event, since, latest uint64) bool {
	if since > latest {
		return true
	}
	for _, event := range events {
		if event.Seq > 0 {
			return since < event.Seq-1
		}
	}
	return since < latest
}

func TestEnforceTurnSummaryBudgetShedsOldestFirst(t *testing.T) {
	summary := model.TurnSummary{ID: "slot1:big", Agent: model.ActorSlot1, TurnID: "big"}
	for i := 0; i < 128; i++ {
		summary.Items = append(summary.Items, model.TurnWorkItem{ID: "i" + jsonInt(i), Kind: "tool", Status: "completed",
			Detail: strings.Repeat("d", 12<<10), Data: json.RawMessage(`"` + strings.Repeat("p", 4000) + `"`)})
	}
	enforceTurnSummaryBudget(&summary)
	encoded, _ := json.Marshal(summary)
	if len(encoded) > turnSummaryByteBudget {
		t.Fatalf("budgeted summary = %d bytes", len(encoded))
	}
	last := summary.Items[len(summary.Items)-1]
	if last.ID != "i127" || last.Data == nil || last.Detail == "" {
		t.Fatalf("newest item lost its evidence: %#v", last.ID)
	}
	if summary.Items[0].Data != nil {
		t.Fatal("oldest item payload should be shed first")
	}
}

func TestTurnItemEvidenceLoadsDurableSourcesOnDemand(t *testing.T) {
	dir := t.TempDir()
	engine, _ := newTestEngine(t, dir)
	started := time.Now().UTC()
	long := strings.Repeat("line of tool output\n", 200)
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeTurnStarted, TurnID: "lazy", CreatedAt: started})
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeToolStarted, TurnID: "lazy", ItemID: "call-1", Name: "Bash",
		Text: "go test ./...", Data: json.RawMessage(`{"command":"go test ./..."}`), CreatedAt: started.Add(time.Second)})
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeToolCompleted, TurnID: "lazy", ItemID: "call-1", Name: "Bash",
		Text: long, Data: toolPayload(1), CreatedAt: started.Add(2 * time.Second)})
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeToolStarted, TurnID: "lazy", ItemID: "no-evidence", Name: "Noop", CreatedAt: started.Add(3 * time.Second)})

	item := engine.Snapshot().Turns[0].Items[0]
	if item.Data != nil || len(item.SourceSeqs) != 2 || len(item.Detail) > turnItemPreviewLimit || !strings.HasSuffix(item.Detail, "…") {
		t.Fatalf("summary item should hold a preview and two references: %#v", item)
	}
	evidence, err := engine.TurnItemEvidence("slot1:lazy", "call-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) != 2 || evidence[0].Kind != model.RuntimeToolStarted || evidence[1].Text != long || string(evidence[1].Data) != string(toolPayload(1)) {
		t.Fatalf("evidence incomplete: %#v", evidence)
	}
	if none, err := engine.TurnItemEvidence("slot1:lazy", "no-evidence"); err != nil || len(none) != 0 {
		t.Fatalf("item without evidence = %v, %v", none, err)
	}
	for _, key := range [][2]string{{"slot1:lazy", "missing"}, {"slot2:lazy", "call-1"}} {
		if _, err := engine.TurnItemEvidence(key[0], key[1]); !errors.Is(err, ErrTurnItemNotFound) {
			t.Fatalf("%v: err = %v", key, err)
		}
	}

	// References survive restart through the open-time offset index.
	if err := engine.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	reopened := reopenEngine(t, dir)
	again, err := reopened.TurnItemEvidence("slot1:lazy", "call-1")
	if err != nil || len(again) != 2 || again[1].Text != long {
		t.Fatalf("evidence after restart = %d records, %v", len(again), err)
	}
}

func TestTurnItemEvidenceRejectsForeignSourceRecords(t *testing.T) {
	engine, _ := newTestEngine(t, "")
	started := time.Now().UTC()
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeTurnStarted, TurnID: "own", CreatedAt: started})
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeToolStarted, TurnID: "own", ItemID: "tool", Text: "x", CreatedAt: started})
	engine.notice("info", "unrelated fact")
	noticeSeq := engine.Snapshot().LatestSeq
	engine.mu.Lock()
	engine.snapshot.Turns[0].Items[0].SourceSeqs = []uint64{noticeSeq}
	engine.mu.Unlock()
	if _, err := engine.TurnItemEvidence("slot1:own", "tool"); err == nil {
		t.Fatal("a reference to a non-runtime record was served")
	}
}

func TestTurnItemEvidenceBudgetAndInlineFallback(t *testing.T) {
	if got := boundedHead("abcdef", 2); got != "" {
		t.Fatalf("tiny head limit = %q", got)
	}
	if got := boundedHead("ab€cd", 5); got != "ab…" {
		t.Fatalf("head must not split a rune: %q", got)
	}
	// Without a durable source record the bounded evidence stays inline.
	summary := model.TurnSummary{}
	upsertTurnItem(&summary, model.RuntimeEvent{ItemID: "t", Text: "detail", Data: toolPayload(2)}, 0, "tool", "working")
	if item := summary.Items[0]; len(item.SourceSeqs) != 0 || item.Data == nil || item.Detail != "detail" {
		t.Fatalf("inline fallback lost evidence: %#v", item)
	}
	for i := uint64(1); i <= turnItemSourceLimit+5; i++ {
		upsertTurnItem(&summary, model.RuntimeEvent{ItemID: "t", Text: "progress"}, i, "tool", "working")
	}
	seqs := summary.Items[0].SourceSeqs
	if len(seqs) != turnItemSourceLimit || seqs[0] != 1 || seqs[len(seqs)-1] != turnItemSourceLimit+5 || summary.Items[0].Data != nil {
		t.Fatalf("source references not bounded to first and newest: %v", seqs)
	}
}

func TestSnapshotEventTailBoundsRuntimePayloadsWithoutMutatingEngine(t *testing.T) {
	large := model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeToolCompleted, TurnID: "t", ItemID: "i",
		Text: strings.Repeat("t", 64<<10), Data: toolPayload(3), CorrelationID: "message-1"}
	data, _ := json.Marshal(large)
	small, _ := json.Marshal(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeLog, Text: "short"})
	events := []model.Event{
		{Seq: 1, Kind: EventRuntime, Data: data},
		{Seq: 2, Kind: EventTurnSummaryUpdated, Data: json.RawMessage(`{}`)},
		{Seq: 3, Kind: EventRuntime, Data: small},
	}
	tail := SnapshotEventTail(events)
	if len(tail) != 2 || tail[0].Seq != 1 || tail[1].Seq != 3 || string(tail[1].Data) != string(small) {
		t.Fatalf("unexpected projected tail: %d events", len(tail))
	}
	var projected model.RuntimeEvent
	if err := json.Unmarshal(tail[0].Data, &projected); err != nil {
		t.Fatal(err)
	}
	if len(projected.Text) > snapshotEventTextLimit || !strings.Contains(string(projected.Data), `"truncated":true`) ||
		projected.CorrelationID != "message-1" || projected.ItemID != "i" || projected.Kind != model.RuntimeToolCompleted {
		t.Fatalf("projection lost routing fields or kept full payload: text=%d data=%d", len(projected.Text), len(projected.Data))
	}
	if string(events[0].Data) != string(data) {
		t.Fatal("snapshot projection mutated the engine's event")
	}
}

func TestForcedFlushDropsStoppedParticipantBookkeeping(t *testing.T) {
	engine, _ := newTestEngine(t, "")
	started := time.Now().UTC().Add(-time.Hour)
	for _, actor := range []model.ActorID{model.ActorSlot1, model.ActorSlot2} {
		engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: actor, Kind: model.RuntimeTurnStarted, TurnID: "open", CreatedAt: started})
		engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: actor, Kind: model.RuntimeToolStarted, TurnID: "open", ItemID: "a", CreatedAt: started.Add(time.Second)})
	}
	if err := engine.StopAgent(context.Background(), model.ActorSlot1); err != nil {
		t.Fatal(err)
	}
	engine.summaryMu.Lock()
	_, slot1 := engine.turnSummaries.persistedAt["slot1:open"]
	_, slot2 := engine.turnSummaries.persistedAt["slot2:open"]
	dirty2 := engine.turnSummaries.dirty["slot2:open"]
	engine.summaryMu.Unlock()
	if slot1 || !slot2 || !dirty2 {
		t.Fatalf("stop must drop only the stopped participant's bookkeeping: slot1=%v slot2=%v dirty2=%v", slot1, slot2, dirty2)
	}
	window := engine.WindowedSnapshot(250)
	if len(window.Turns) != 2 {
		t.Fatalf("both summaries remain available in the projection: %d", len(window.Turns))
	}
}

// A stop-time checkpoint failure reaches the lifecycle caller: Stop reports it,
// and Restart and a permission change do not launch a runtime afterwards.
func TestStopTimeCheckpointFailureReachesLifecycleCallers(t *testing.T) {
	for _, op := range []string{"stop", "restart", "permissions"} {
		t.Run(op, func(t *testing.T) {
			engine, adapters := newTestEngine(t, "")
			started := time.Now().UTC()
			engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeTurnStarted, TurnID: "open", CreatedAt: started})
			engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeToolStarted, TurnID: "open", ItemID: "a", CreatedAt: started.Add(time.Second)})
			// Permission changes require an idle participant; the Turn summary
			// stays dirty regardless.
			engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeState, State: model.StateIdle, CreatedAt: started.Add(2 * time.Second)})
			old := adapters[model.ActorSlot1]
			old.onStop = func() { _ = engine.cfg.Store.Close() }
			var err error
			switch op {
			case "stop":
				err = engine.StopAgent(context.Background(), model.ActorSlot1)
			case "restart":
				err = engine.RestartAgent(context.Background(), model.ActorSlot1)
			case "permissions":
				err = engine.SetPermissions(context.Background(), model.ActorSlot1, model.PermissionReadOnly)
			}
			if err == nil {
				t.Fatal("stop-time checkpoint failure was not returned")
			}
			if engine.Fatal() == nil {
				t.Fatal("checkpoint failure did not mark the Room store fatal")
			}
			old.mu.Lock()
			starts := old.starts
			old.mu.Unlock()
			if starts != 0 || adapters[model.ActorSlot1] != old {
				t.Fatalf("a runtime was launched or rebuilt after the failed checkpoint: starts=%d rebuilt=%v", starts, adapters[model.ActorSlot1] != old)
			}
		})
	}
}

// A forced flush drops a stopped participant's bookkeeping even when its last
// checkpoint left nothing dirty.
func TestForcedFlushDropsBookkeepingWithoutDirtySummaries(t *testing.T) {
	engine, _ := newTestEngine(t, "")
	started := time.Now().UTC()
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeTurnStarted, TurnID: "open", CreatedAt: started})
	engine.summaryMu.Lock()
	_, tracked := engine.turnSummaries.persistedAt["slot1:open"]
	dirty := len(engine.turnSummaries.dirty)
	engine.summaryMu.Unlock()
	if !tracked || dirty != 0 {
		t.Fatalf("setup needs a checkpointed, clean Turn: tracked=%v dirty=%d", tracked, dirty)
	}
	if err := engine.StopAgent(context.Background(), model.ActorSlot1); err != nil {
		t.Fatal(err)
	}
	engine.summaryMu.Lock()
	_, tracked = engine.turnSummaries.persistedAt["slot1:open"]
	_, published := engine.turnSummaries.publishedAt["slot1:open"]
	engine.summaryMu.Unlock()
	if tracked || published {
		t.Fatalf("stop kept bookkeeping for the stopped Turn: persisted=%v published=%v", tracked, published)
	}
}
