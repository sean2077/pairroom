package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/bus"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

type fakeAdapter struct {
	actor        model.ActorID
	sink         agent.EventSink
	submissions  chan model.AgentInput
	mu           sync.Mutex
	state        model.AgentState
	sessionID    string
	beforeReturn func(model.AgentInput)
	submitErr    error
	role         model.ParticipantRole
	workspace    string
	interrupts   int
}

func (f *fakeAdapter) Actor() model.ActorID { return f.actor }
func (f *fakeAdapter) SessionID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessionID
}
func (f *fakeAdapter) State() model.AgentState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}
func (f *fakeAdapter) Start(context.Context) error {
	f.mu.Lock()
	f.state = model.StateIdle
	if f.sessionID == "" {
		f.sessionID = "fake-" + string(f.actor)
	}
	f.mu.Unlock()
	return nil
}
func (f *fakeAdapter) Submit(ctx context.Context, input model.AgentInput) (model.DeliveryState, error) {
	if err := f.Start(ctx); err != nil {
		return model.DeliveryFailed, err
	}
	if f.submitErr != nil {
		return model.DeliveryFailed, f.submitErr
	}
	select {
	case f.submissions <- input:
		if f.beforeReturn != nil {
			f.beforeReturn(input)
		}
		return model.DeliveryStarted, nil
	case <-ctx.Done():
		return model.DeliveryFailed, ctx.Err()
	}
}
func (f *fakeAdapter) Interrupt(context.Context) error {
	f.mu.Lock()
	f.interrupts++
	f.mu.Unlock()
	return nil
}
func (f *fakeAdapter) Stop(context.Context) error {
	f.mu.Lock()
	f.state = model.StateStopped
	f.mu.Unlock()
	return nil
}
func (f *fakeAdapter) ResolveApproval(context.Context, string, model.ApprovalResolution) error {
	return agent.ErrApprovalUnsupported
}
func (f *fakeAdapter) SetRole(_ context.Context, role model.ParticipantRole) error {
	f.mu.Lock()
	f.role = role
	f.mu.Unlock()
	return nil
}
func (f *fakeAdapter) SetWorkspace(_ context.Context, workspace string) error {
	f.mu.Lock()
	f.workspace = workspace
	f.mu.Unlock()
	return nil
}

func newTestEngine(t *testing.T, mode model.RoutingMode, dir string) (*Engine, map[model.ActorID]*fakeAdapter) {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	eventStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	adapters := map[model.ActorID]*fakeAdapter{}
	factory := func(cfg agent.Config, sink agent.EventSink) agent.Adapter {
		adapter := &fakeAdapter{
			actor: cfg.Actor, sink: sink, state: model.StateStopped,
			sessionID: cfg.SessionID, submissions: make(chan model.AgentInput, 16),
		}
		adapters[cfg.Actor] = adapter
		return adapter
	}
	engine, err := New(Config{
		Name: "test", Repo: t.TempDir(), Store: eventStore, Hub: bus.New(64),
		Settings:      model.RoomSettings{RoutingMode: mode, MaxHops: 6},
		ClaudeFactory: factory, CodexFactory: factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return engine, adapters
}

func receiveInput(t *testing.T, adapter *fakeAdapter) model.AgentInput {
	t.Helper()
	select {
	case input := <-adapter.submissions:
		return input
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for agent input")
		return model.AgentInput{}
	}
}

func TestSendPassesRoleAndRoutingContext(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingMentions, "")
	message, err := engine.Send(context.Background(), SendRequest{
		Text: "@claude inspect this", To: []model.ActorID{model.ActorClaude},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := receiveInput(t, adapters[model.ActorClaude])
	if input.MessageID != message.ID || input.From != model.ActorUser || input.To != model.ActorClaude {
		t.Fatalf("unexpected delivery envelope: %#v", input)
	}
	if input.Role != model.RoleDriver || input.RoutingMode != model.RoutingMentions || input.MaxHops != 6 {
		t.Fatalf("missing room context: %#v", input)
	}
}

func TestAgentMentionCreatesReplyAndRoutesPeer(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingMentions, "")
	incoming, err := engine.Send(context.Background(), SendRequest{
		Text: "Design the change", To: []model.ActorID{model.ActorClaude},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorClaude])

	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeFinal,
		TurnID: "turn-1", CorrelationID: incoming.ID,
		Text:      "I propose an event log. @codex please challenge the failure modes.",
		CreatedAt: time.Now().UTC(),
	})
	peerInput := receiveInput(t, adapters[model.ActorCodex])
	if peerInput.From != model.ActorClaude || peerInput.ReplyTo != incoming.ID || peerInput.Hop != 1 {
		t.Fatalf("unexpected peer handoff: %#v", peerInput)
	}

	snapshot := engine.Snapshot()
	last := snapshot.Messages[len(snapshot.Messages)-1]
	if last.From != model.ActorClaude || last.ReplyTo != incoming.ID || last.ThreadID != incoming.ThreadID {
		t.Fatalf("unexpected room reply: %#v", last)
	}
}

func TestNewHumanMessageSuppressesStaleRoundtableHandoff(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingRoundtable, "")
	first, err := engine.Send(context.Background(), SendRequest{Text: "First direction", To: []model.ActorID{model.ActorClaude}})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorClaude])
	if _, err := engine.Send(context.Background(), SendRequest{Text: "New direction", To: []model.ActorID{model.ActorClaude}}); err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorClaude])

	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeFinal,
		CorrelationID: first.ID, TurnID: "old", Text: "Old answer without a stop marker",
		CreatedAt: time.Now().UTC(),
	})
	select {
	case got := <-adapters[model.ActorCodex].submissions:
		t.Fatalf("stale response was incorrectly handed to Codex: %#v", got)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestControlMarkersAndHopLimitStopRoundtable(t *testing.T) {
	clean, control := stripControl("Done with evidence.\n[PAIRROOM:CONSENSUS]")
	if clean != "Done with evidence." || control != "CONSENSUS" || !stopsConversation(control) {
		t.Fatalf("unexpected control parsing: clean=%q control=%q", clean, control)
	}
	if stopsConversation("CONTINUE") {
		t.Fatal("CONTINUE must not stop a roundtable")
	}

	engine, _ := newTestEngine(t, model.RoutingRoundtable, "")
	settings := model.RoomSettings{RoutingMode: model.RoutingRoundtable, MaxHops: 3}
	if targets := engine.agentTargets(model.ActorClaude, "continue", "", 3, 1, 1, settings); len(targets) != 0 {
		t.Fatalf("hop limit must stop routing: %v", targets)
	}
	if targets := engine.agentTargets(model.ActorClaude, "continue", "", 2, 1, 1, settings); len(targets) != 1 || targets[0] != model.ActorCodex {
		t.Fatalf("roundtable should route to peer before limit: %v", targets)
	}
}

func TestSessionIDsSurviveRoomRestartButRuntimeStateDoesNot(t *testing.T) {
	dir := t.TempDir()
	engine, _ := newTestEngine(t, model.RoutingMentions, dir)
	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeSession,
		SessionID: "claude-session-123", CreatedAt: time.Now().UTC(),
	})
	if err := engine.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	reopenedStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(Config{
		Name: "ignored", Repo: t.TempDir(), Store: reopenedStore,
		Settings: model.DefaultRoomSettings(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	participant := reopened.Snapshot().Participants[model.ActorClaude]
	if participant.SessionID != "claude-session-123" {
		t.Fatalf("session id was not restored: %#v", participant)
	}
	if participant.State != model.StateStopped || participant.CurrentTurn != "" {
		t.Fatalf("ephemeral runtime state should reset: %#v", participant)
	}
}

func waitForDeliveryState(t *testing.T, engine *Engine, messageID string, target model.ActorID, want model.DeliveryState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := engine.Snapshot()
		for _, message := range snapshot.Messages {
			if message.ID == messageID && message.Delivery[target] == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("delivery %s for %s did not reach %s", messageID, target, want)
}

func TestDeliveryFailureIsTerminalAndLateStateIsNotPublished(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingManual, "")
	message, err := engine.Send(context.Background(), SendRequest{
		Text: "inspect", To: []model.ActorID{model.ActorClaude},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorClaude])
	waitForDeliveryState(t, engine, message.ID, model.ActorClaude, model.DeliveryStarted)

	before := len(engine.Snapshot().Events)
	engine.delivery(message.ID, model.ActorClaude, model.DeliveryFailed, "runtime failed")
	engine.delivery(message.ID, model.ActorClaude, model.DeliveryStarted, "late submit result")

	snapshot := engine.Snapshot()
	var got model.DeliveryState
	for _, item := range snapshot.Messages {
		if item.ID == message.ID {
			got = item.Delivery[model.ActorClaude]
			break
		}
	}
	if got != model.DeliveryFailed {
		t.Fatalf("terminal failure was overwritten: %q", got)
	}
	updates := 0
	for _, event := range snapshot.Events[before:] {
		if event.Kind == EventDeliveryUpdated {
			updates++
		}
	}
	if updates != 1 {
		t.Fatalf("expected only the valid terminal update to publish, got %d", updates)
	}
}

func TestTransientRuntimeEventIsLiveButNotPersisted(t *testing.T) {
	engine, _ := newTestEngine(t, model.RoutingManual, "")
	ch, cancel := engine.Subscribe()
	defer cancel()
	before := engine.Snapshot().LatestSeq

	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeTextDelta,
		TurnID: "turn-live", CorrelationID: "msg-live", Text: "token",
		CreatedAt: time.Now().UTC(),
	})

	select {
	case event := <-ch:
		if event.Seq != 0 || event.Kind != EventRuntime {
			t.Fatalf("unexpected transient event envelope: %#v", event)
		}
		var runtime model.RuntimeEvent
		if err := json.Unmarshal(event.Data, &runtime); err != nil {
			t.Fatal(err)
		}
		if runtime.Kind != model.RuntimeTextDelta || runtime.Text != "token" {
			t.Fatalf("unexpected transient runtime event: %#v", runtime)
		}
	case <-time.After(time.Second):
		t.Fatal("transient runtime event was not published")
	}

	after := engine.Snapshot()
	if after.LatestSeq != before {
		t.Fatalf("transient event advanced durable sequence: before=%d after=%d", before, after.LatestSeq)
	}
	for _, event := range after.Events {
		if event.Seq == 0 {
			t.Fatalf("transient event leaked into durable snapshot: %#v", event)
		}
	}
	events, err := engine.cfg.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind != EventRuntime {
			continue
		}
		var runtime model.RuntimeEvent
		if err := json.Unmarshal(event.Data, &runtime); err != nil {
			t.Fatal(err)
		}
		if runtime.Kind == model.RuntimeTextDelta && runtime.Text == "token" {
			t.Fatal("transient text delta was persisted")
		}
	}
}

func TestRestartExpiresPendingDeliveryAndApproval(t *testing.T) {
	dir := t.TempDir()
	eventStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Config{
		Name: "restore", Repo: t.TempDir(), Store: eventStore,
		Settings: model.DefaultRoomSettings(),
	})
	if err != nil {
		t.Fatal(err)
	}
	message := model.Message{
		ID: model.NewID("msg"), From: model.ActorUser, To: []model.ActorID{model.ActorCodex},
		Text: "pending", ThreadID: model.NewID("thread"), CreatedAt: time.Now().UTC(),
		Delivery: map[model.ActorID]model.DeliveryState{model.ActorCodex: model.DeliveryPending},
	}
	if _, err := engine.record(EventMessageCreated, model.ActorUser, message); err != nil {
		t.Fatal(err)
	}
	approval := model.Approval{
		ID: model.NewID("approval"), Agent: model.ActorCodex, Kind: "command",
		Title: "pending", Status: "pending", RequestedAt: time.Now().UTC(),
	}
	if _, err := engine.record(EventApprovalUpdated, model.ActorCodex, approval); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(Config{
		Name: "ignored", Repo: t.TempDir(), Store: reopenedStore,
		Settings: model.DefaultRoomSettings(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot := reopened.Snapshot()
	if got := snapshot.Messages[len(snapshot.Messages)-1].Delivery[model.ActorCodex]; got != model.DeliverySkipped {
		t.Fatalf("pending delivery was not expired: %q", got)
	}
	var restored *model.Approval
	for i := range snapshot.Approvals {
		if snapshot.Approvals[i].ID == approval.ID {
			restored = &snapshot.Approvals[i]
			break
		}
	}
	if restored == nil || restored.Status != "expired" || restored.Decision != "runtime_restarted" || restored.ResolvedAt == nil {
		t.Fatalf("pending approval was not expired safely: %#v", restored)
	}
}

func findMessage(t *testing.T, snapshot model.RoomSnapshot, id string) model.Message {
	t.Helper()
	for _, message := range snapshot.Messages {
		if message.ID == id {
			return message
		}
	}
	t.Fatalf("message %s was not found", id)
	return model.Message{}
}

func waitForProcessingState(t *testing.T, engine *Engine, messageID string, target model.ActorID, want model.ProcessingState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		message := findMessage(t, engine.Snapshot(), messageID)
		if message.Processing[target] == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	message := findMessage(t, engine.Snapshot(), messageID)
	t.Fatalf("processing %s for %s = %s, want %s", messageID, target, message.Processing[target], want)
}

func TestDeliveryFallbackDoesNotOverwriteNativeProcessingCorrelation(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingManual, "")
	adapter := adapters[model.ActorCodex]
	adapter.beforeReturn = func(input model.AgentInput) {
		adapter.sink(model.RuntimeEvent{
			Agent: model.ActorCodex, Kind: model.RuntimeInputProcessing,
			CorrelationID: input.MessageID, TurnID: "turn-native", Text: "native runtime accepted input",
			CreatedAt: time.Now().UTC(),
		})
	}

	message, err := engine.Send(context.Background(), SendRequest{
		Text: "inspect", To: []model.ActorID{model.ActorCodex},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapter)
	waitForDeliveryState(t, engine, message.ID, model.ActorCodex, model.DeliveryStarted)
	waitForProcessingState(t, engine, message.ID, model.ActorCodex, model.ProcessingWorking)

	got := findMessage(t, engine.Snapshot(), message.ID)
	if got.ProcessingDetail[model.ActorCodex] != "native runtime accepted input" || got.ProcessingTurn[model.ActorCodex] != "turn-native" {
		t.Fatalf("delivery fallback overwrote native processing metadata: %#v", got)
	}
}

func TestRuntimeFailurePreservesAcceptedDeliveryAndMarksProcessingFailed(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingManual, "")
	message, err := engine.Send(context.Background(), SendRequest{
		Text: "inspect", To: []model.ActorID{model.ActorCodex},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorCodex])
	waitForDeliveryState(t, engine, message.ID, model.ActorCodex, model.DeliveryStarted)

	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorCodex, Kind: model.RuntimeInputProcessing,
		CorrelationID: message.ID, TurnID: "turn-1", Text: "working",
		CreatedAt: time.Now().UTC(),
	})
	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorCodex, Kind: model.RuntimeError,
		CorrelationID: message.ID, TurnID: "turn-1", Text: "native turn failed",
		CreatedAt: time.Now().UTC(),
	})

	waitForProcessingState(t, engine, message.ID, model.ActorCodex, model.ProcessingFailed)
	got := findMessage(t, engine.Snapshot(), message.ID)
	if got.Delivery[model.ActorCodex] != model.DeliveryStarted {
		t.Fatalf("accepted delivery was overwritten by runtime failure: %#v", got.Delivery)
	}
	if got.ProcessingDetail[model.ActorCodex] != "native turn failed" || got.ProcessingTurn[model.ActorCodex] != "turn-1" {
		t.Fatalf("failure detail was not projected: %#v", got)
	}
}

func TestRetryCreatesNewAuditableMessageForFailedTarget(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingManual, "")
	original, err := engine.Send(context.Background(), SendRequest{
		Text: "review", To: []model.ActorID{model.ActorClaude, model.ActorCodex},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorClaude])
	_ = receiveInput(t, adapters[model.ActorCodex])
	waitForDeliveryState(t, engine, original.ID, model.ActorClaude, model.DeliveryStarted)
	waitForDeliveryState(t, engine, original.ID, model.ActorCodex, model.DeliveryStarted)

	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorCodex, Kind: model.RuntimeInputFailed,
		CorrelationID: original.ID, TurnID: "turn-failed", Text: "tool crashed",
		CreatedAt: time.Now().UTC(),
	})
	waitForProcessingState(t, engine, original.ID, model.ActorCodex, model.ProcessingFailed)

	retry, err := engine.Retry(context.Background(), original.ID, RetryRequest{To: []model.ActorID{model.ActorCodex}})
	if err != nil {
		t.Fatal(err)
	}
	if retry.ID == original.ID || retry.RetryOf != original.ID {
		t.Fatalf("retry did not create an auditable child message: original=%#v retry=%#v", original, retry)
	}
	if retry.ThreadID != original.ThreadID || retry.Text != original.Text || len(retry.To) != 1 || retry.To[0] != model.ActorCodex {
		t.Fatalf("retry did not preserve conversation identity: %#v", retry)
	}
	input := receiveInput(t, adapters[model.ActorCodex])
	if input.MessageID != retry.ID {
		t.Fatalf("runtime received original message ID instead of retry ID: %#v", input)
	}
	waitForDeliveryState(t, engine, retry.ID, model.ActorCodex, model.DeliveryStarted)

	if _, err := engine.Retry(context.Background(), original.ID, RetryRequest{To: []model.ActorID{model.ActorClaude}}); err == nil {
		t.Fatal("successful/non-terminal Claude target should not be retryable")
	}
}

func TestRestartCancelsInFlightProcessing(t *testing.T) {
	dir := t.TempDir()
	engine, adapters := newTestEngine(t, model.RoutingManual, dir)
	message, err := engine.Send(context.Background(), SendRequest{
		Text: "long-running", To: []model.ActorID{model.ActorClaude},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorClaude])
	waitForDeliveryState(t, engine, message.ID, model.ActorClaude, model.DeliveryStarted)
	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeInputProcessing,
		CorrelationID: message.ID, TurnID: "turn-live", Text: "working",
		CreatedAt: time.Now().UTC(),
	})
	waitForProcessingState(t, engine, message.ID, model.ActorClaude, model.ProcessingWorking)
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(Config{
		Name: "ignored", Repo: t.TempDir(), Store: reopenedStore,
		Settings: model.DefaultRoomSettings(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got := findMessage(t, reopened.Snapshot(), message.ID)
	if got.Delivery[model.ActorClaude] != model.DeliveryStarted {
		t.Fatalf("restart must retain historical delivery acceptance: %#v", got.Delivery)
	}
	if got.Processing[model.ActorClaude] != model.ProcessingCancelled {
		t.Fatalf("restart left ghost working state: %#v", got.Processing)
	}
	if !strings.Contains(got.ProcessingDetail[model.ActorClaude], "restarted") {
		t.Fatalf("restart cancellation lacks a useful reason: %#v", got.ProcessingDetail)
	}
}

func TestRuntimeInfoProjectionIsDeepCloned(t *testing.T) {
	engine, _ := newTestEngine(t, model.RoutingManual, "")
	info := model.RuntimeInfo{
		Available: true, Version: "2.1.231", Protocol: "claude-stream-json",
		Capabilities: []string{"stream-json"}, Warnings: []string{"test warning"},
		Data: json.RawMessage(`{"source":"probe"}`), ProbedAt: time.Now().UTC(),
	}
	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeInfoUpdated, Runtime: &info,
		CreatedAt: time.Now().UTC(),
	})
	first := engine.Snapshot()
	participant := first.Participants[model.ActorClaude]
	if participant.Runtime.Version != info.Version || len(participant.Runtime.Capabilities) != 1 {
		t.Fatalf("runtime info was not projected: %#v", participant.Runtime)
	}
	participant.Runtime.Capabilities[0] = "mutated"
	participant.Runtime.Warnings[0] = "mutated"
	participant.Runtime.Data[0] = '['
	first.Participants[model.ActorClaude] = participant

	second := engine.Snapshot().Participants[model.ActorClaude].Runtime
	if second.Capabilities[0] != "stream-json" || second.Warnings[0] != "test warning" || string(second.Data) != `{"source":"probe"}` {
		t.Fatalf("snapshot leaked mutable runtime info: %#v", second)
	}
}

func TestStopAgentCancelsInFlightMessagesAndExpiresApprovals(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingManual, "")
	message, err := engine.Send(context.Background(), SendRequest{
		Text: "keep working", To: []model.ActorID{model.ActorCodex},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorCodex])
	waitForDeliveryState(t, engine, message.ID, model.ActorCodex, model.DeliveryStarted)
	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorCodex, Kind: model.RuntimeInputProcessing,
		CorrelationID: message.ID, TurnID: "turn-stop", Text: "working",
		CreatedAt: time.Now().UTC(),
	})
	approval := model.Approval{
		ID: model.NewID("approval"), Agent: model.ActorCodex, Kind: "command",
		Title: "run command", Status: "pending", RequestedAt: time.Now().UTC(),
	}
	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorCodex, Kind: model.RuntimeApprovalRequested,
		Approval: &approval, CreatedAt: time.Now().UTC(),
	})

	if err := engine.StopAgent(context.Background(), model.ActorCodex); err != nil {
		t.Fatal(err)
	}
	got := findMessage(t, engine.Snapshot(), message.ID)
	if got.Processing[model.ActorCodex] != model.ProcessingCancelled {
		t.Fatalf("stop left an in-flight message unresolved: %#v", got.Processing)
	}
	if got.Delivery[model.ActorCodex] != model.DeliveryStarted {
		t.Fatalf("stop rewrote historical delivery state: %#v", got.Delivery)
	}
	var resolved *model.Approval
	for i := range engine.Snapshot().Approvals {
		item := engine.Snapshot().Approvals[i]
		if item.ID == approval.ID {
			resolved = &item
			break
		}
	}
	if resolved == nil || resolved.Status != "expired" || resolved.Decision != "runtime_stopped" || resolved.ResolvedAt == nil {
		t.Fatalf("stop left an unresolvable approval pending: %#v", resolved)
	}
}

func TestSubmitFailureSettlesDeliveryAndProcessing(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingManual, "")
	adapters[model.ActorClaude].submitErr = errors.New("native input rejected")

	message, err := engine.Send(context.Background(), SendRequest{
		Text: "inspect", To: []model.ActorID{model.ActorClaude},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForDeliveryState(t, engine, message.ID, model.ActorClaude, model.DeliveryFailed)
	waitForProcessingState(t, engine, message.ID, model.ActorClaude, model.ProcessingFailed)

	got := findMessage(t, engine.Snapshot(), message.ID)
	if !strings.Contains(got.DeliveryDetail[model.ActorClaude], "native input rejected") {
		t.Fatalf("delivery failure detail was lost: %#v", got.DeliveryDetail)
	}
	if !strings.Contains(got.ProcessingDetail[model.ActorClaude], "did not accept") {
		t.Fatalf("processing failure did not explain pre-execution rejection: %#v", got.ProcessingDetail)
	}
}

type fakeAttachmentStore struct {
	metadata   map[string]model.Attachment
	paths      map[string]string
	discovered []model.Attachment
}

func (f *fakeAttachmentStore) Resolve(id string) (model.Attachment, string, error) {
	value, ok := f.metadata[id]
	if !ok {
		return model.Attachment{}, "", errors.New("unknown fake attachment")
	}
	return value, f.paths[id], nil
}

func (f *fakeAttachmentStore) DiscoverRepoImages(string, string) []model.Attachment {
	return append([]model.Attachment(nil), f.discovered...)
}

func newAttachmentEngine(t *testing.T, media AttachmentStore) (*Engine, map[model.ActorID]*fakeAdapter) {
	t.Helper()
	eventStore, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	adapters := map[model.ActorID]*fakeAdapter{}
	factory := func(cfg agent.Config, sink agent.EventSink) agent.Adapter {
		value := &fakeAdapter{actor: cfg.Actor, sink: sink, state: model.StateStopped, submissions: make(chan model.AgentInput, 16)}
		adapters[cfg.Actor] = value
		return value
	}
	engine, err := New(Config{
		Name: "media", Repo: t.TempDir(), Store: eventStore, Hub: bus.New(64),
		Settings:      model.RoomSettings{RoutingMode: model.RoutingManual, MaxHops: 4, StallWarningSeconds: 300},
		ClaudeFactory: factory, CodexFactory: factory, Attachments: media,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return engine, adapters
}

func TestSendCanonicalizesImagesAndResolvesPathsOnlyForAgentBoundary(t *testing.T) {
	image := model.Attachment{
		ID: "att-0123456789abcdef01234567", Name: "diagram.png", MediaType: "image/png", Kind: "image",
		Size: 68, SHA256: strings.Repeat("a", 64), Width: 1, Height: 1, CreatedAt: time.Now().UTC(),
	}
	media := &fakeAttachmentStore{
		metadata: map[string]model.Attachment{image.ID: image},
		paths:    map[string]string{image.ID: "/private/pairroom/attachments/diagram.png"},
	}
	engine, adapters := newAttachmentEngine(t, media)
	message, err := engine.Send(context.Background(), SendRequest{
		Text: "Review the image", To: []model.ActorID{model.ActorClaude},
		Attachments: []model.Attachment{{ID: image.ID, Name: "forged-name.exe", Size: 999999}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(message.Attachments) != 1 || message.Attachments[0].Name != "diagram.png" || message.Attachments[0].Size != 68 {
		t.Fatalf("durable message did not use canonical metadata: %#v", message.Attachments)
	}
	if encoded, _ := json.Marshal(message); strings.Contains(string(encoded), "/private/pairroom") {
		t.Fatalf("host-local path leaked into durable transcript: %s", encoded)
	}
	input := receiveInput(t, adapters[model.ActorClaude])
	if len(input.Attachments) != 1 || input.Attachments[0].Path != "/private/pairroom/attachments/diagram.png" || input.Attachments[0].Name != "diagram.png" {
		t.Fatalf("native adapter did not receive resolved image: %#v", input.Attachments)
	}
}

func TestAgentFinalImportsSafeImagePreviewIntoSharedRoom(t *testing.T) {
	generated := model.Attachment{
		ID: "att-89abcdef0123456789abcdef", Name: "architecture.png", MediaType: "image/png", Kind: "image",
		Size: 512, SHA256: strings.Repeat("b", 64), Width: 1280, Height: 720, CreatedAt: time.Now().UTC(), Source: "claude-artifact",
	}
	media := &fakeAttachmentStore{metadata: map[string]model.Attachment{}, paths: map[string]string{}, discovered: []model.Attachment{generated}}
	engine, adapters := newAttachmentEngine(t, media)
	incoming, err := engine.Send(context.Background(), SendRequest{Text: "Show the architecture", To: []model.ActorID{model.ActorClaude}})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorClaude])
	engine.HandleRuntimeEvent(model.RuntimeEvent{
		Agent: model.ActorClaude, Kind: model.RuntimeFinal, CorrelationID: incoming.ID, TurnID: "turn-image",
		Text: "Rendered the result: ![architecture](docs/architecture.png)", CreatedAt: time.Now().UTC(),
	})

	snapshot := engine.Snapshot()
	var response *model.Message
	for i := range snapshot.Messages {
		if snapshot.Messages[i].From == model.ActorClaude && snapshot.Messages[i].ReplyTo == incoming.ID {
			copy := snapshot.Messages[i]
			response = &copy
		}
	}
	if response == nil || len(response.Attachments) != 1 || response.Attachments[0].ID != generated.ID {
		t.Fatalf("agent-produced image was not projected into the room: %#v", response)
	}
}

func TestSwitchDriverAppliesNativeRoleBeforeFutureTurns(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingManual, "")
	if err := engine.SwitchDriver(context.Background(), model.ActorCodex); err != nil {
		t.Fatal(err)
	}
	snapshot := engine.Snapshot()
	if snapshot.Participants[model.ActorCodex].Role != model.RoleDriver || snapshot.Participants[model.ActorClaude].Role != model.RoleReviewer {
		t.Fatalf("room roles did not switch atomically: %#v", snapshot.Participants)
	}
	adapters[model.ActorClaude].mu.Lock()
	claudeRole := adapters[model.ActorClaude].role
	adapters[model.ActorClaude].mu.Unlock()
	adapters[model.ActorCodex].mu.Lock()
	codexRole := adapters[model.ActorCodex].role
	adapters[model.ActorCodex].mu.Unlock()
	if claudeRole != model.RoleReviewer || codexRole != model.RoleDriver {
		t.Fatalf("native role policies were not applied: claude=%q codex=%q", claudeRole, codexRole)
	}

	_, err := engine.Send(context.Background(), SendRequest{Text: "Compare", To: []model.ActorID{model.ActorClaude, model.ActorCodex}})
	if err != nil {
		t.Fatal(err)
	}
	if got := receiveInput(t, adapters[model.ActorClaude]).Role; got != model.RoleReviewer {
		t.Fatalf("Claude turn role = %q", got)
	}
	if got := receiveInput(t, adapters[model.ActorCodex]).Role; got != model.RoleDriver {
		t.Fatalf("Codex turn role = %q", got)
	}
}

func TestRuntimeErrorExpiresConnectionLocalApproval(t *testing.T) {
	engine, _ := newTestEngine(t, model.RoutingManual, "")
	approval := model.Approval{
		ID: model.NewID("approval"), Agent: model.ActorClaude, Kind: "claude.toolApproval",
		Title: "Use Bash", Status: "pending", RequestedAt: time.Now().UTC(),
	}
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorClaude, Kind: model.RuntimeApprovalRequested, Approval: &approval, CreatedAt: time.Now().UTC()})
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorClaude, Kind: model.RuntimeError, Text: "native process exited", CreatedAt: time.Now().UTC()})
	var got *model.Approval
	for _, value := range engine.Snapshot().Approvals {
		if value.ID == approval.ID {
			copy := value
			got = &copy
		}
	}
	if got == nil || got.Status != "expired" || got.Decision != "runtime_error" {
		t.Fatalf("runtime error left stale approval pending: %#v", got)
	}
}

func TestSupersedeMarksOldInputAndCarriesIntent(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingManual, "")
	first, err := engine.Send(context.Background(), SendRequest{
		Text: "old instruction", To: []model.ActorID{model.ActorCodex},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorCodex])
	waitForProcessingState(t, engine, first.ID, model.ActorCodex, model.ProcessingWorking)

	replacement, err := engine.Send(context.Background(), SendRequest{
		Text: "new instruction", To: []model.ActorID{model.ActorCodex}, Intent: model.IntentSupersede,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := receiveInput(t, adapters[model.ActorCodex])
	if input.Intent != model.IntentSupersede {
		t.Fatalf("input intent = %q", input.Intent)
	}
	waitForProcessingState(t, engine, first.ID, model.ActorCodex, model.ProcessingSuperseded)
	if got := replacement.Supersedes[model.ActorCodex]; len(got) != 1 || got[0] != first.ID {
		t.Fatalf("supersedes = %#v", got)
	}
	adapters[model.ActorCodex].mu.Lock()
	interrupts := adapters[model.ActorCodex].interrupts
	adapters[model.ActorCodex].mu.Unlock()
	if interrupts != 1 {
		t.Fatalf("interrupts = %d, want 1", interrupts)
	}
}

func TestCancelMessageMarksParticipantQueue(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingManual, "")
	first, err := engine.Send(context.Background(), SendRequest{Text: "one", To: []model.ActorID{model.ActorClaude}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.Send(context.Background(), SendRequest{Text: "two", To: []model.ActorID{model.ActorClaude}, Intent: model.IntentNextTurn})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorClaude])
	_ = receiveInput(t, adapters[model.ActorClaude])
	waitForProcessingState(t, engine, first.ID, model.ActorClaude, model.ProcessingWorking)
	waitForProcessingState(t, engine, second.ID, model.ActorClaude, model.ProcessingWorking)
	if err := engine.CancelMessage(context.Background(), first.ID, model.ActorClaude); err != nil {
		t.Fatal(err)
	}
	waitForProcessingState(t, engine, first.ID, model.ActorClaude, model.ProcessingCancelled)
	waitForProcessingState(t, engine, second.ID, model.ActorClaude, model.ProcessingCancelled)
}

func TestTurnSummaryPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	engine, adapters := newTestEngine(t, model.RoutingManual, dir)
	message, err := engine.Send(context.Background(), SendRequest{
		Text: "inspect and verify", To: []model.ActorID{model.ActorClaude},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorClaude])
	started := time.Now().UTC().Add(-2 * time.Second)
	events := []model.RuntimeEvent{
		{Agent: model.ActorClaude, Kind: model.RuntimeTurnStarted, TurnID: "turn-summary", SessionID: "session-summary", CorrelationID: message.ID, CreatedAt: started},
		{Agent: model.ActorClaude, Kind: model.RuntimeToolStarted, TurnID: "turn-summary", CorrelationID: message.ID, ItemID: "tool-1", Name: "Read", Text: "README.md", CreatedAt: started.Add(100 * time.Millisecond)},
		{Agent: model.ActorClaude, Kind: model.RuntimeCommandOutput, TurnID: "turn-summary", CorrelationID: message.ID, ItemID: "tool-1", Text: "line one\nline two\n", CreatedAt: started.Add(200 * time.Millisecond)},
		{Agent: model.ActorClaude, Kind: model.RuntimeToolCompleted, TurnID: "turn-summary", CorrelationID: message.ID, ItemID: "tool-1", Name: "Read", Text: "completed", CreatedAt: started.Add(300 * time.Millisecond)},
		{Agent: model.ActorClaude, Kind: model.RuntimePlanUpdated, TurnID: "turn-summary", CorrelationID: message.ID, Text: "1. Inspect\n2. Verify", CreatedAt: started.Add(400 * time.Millisecond)},
		{Agent: model.ActorClaude, Kind: model.RuntimeDiffUpdated, TurnID: "turn-summary", CorrelationID: message.ID, Text: "diff --git a/a b/a", CreatedAt: started.Add(500 * time.Millisecond)},
		{Agent: model.ActorClaude, Kind: model.RuntimeUsageUpdated, TurnID: "turn-summary", CorrelationID: message.ID, Data: json.RawMessage(`{"input_tokens":12,"output_tokens":7}`), CreatedAt: started.Add(600 * time.Millisecond)},
		{Agent: model.ActorClaude, Kind: model.RuntimeFinal, TurnID: "turn-summary", CorrelationID: message.ID, Text: "Verified successfully.", CreatedAt: started.Add(700 * time.Millisecond)},
		{Agent: model.ActorClaude, Kind: model.RuntimeTurnCompleted, TurnID: "turn-summary", CorrelationID: message.ID, Name: "completed", CreatedAt: started.Add(2 * time.Second)},
	}
	for _, event := range events {
		engine.HandleRuntimeEvent(event)
	}

	assertSummary := func(t *testing.T, snapshot model.RoomSnapshot) {
		t.Helper()
		if len(snapshot.Turns) != 1 {
			t.Fatalf("turn summaries = %d, want 1: %#v", len(snapshot.Turns), snapshot.Turns)
		}
		got := snapshot.Turns[0]
		if got.TurnID != "turn-summary" || got.Agent != model.ActorClaude || got.SessionID != "session-summary" {
			t.Fatalf("unexpected summary identity: %#v", got)
		}
		if got.Status != "completed" || got.DurationMillis != 2000 || got.CompletedAt == nil {
			t.Fatalf("unexpected completion projection: %#v", got)
		}
		if len(got.MessageIDs) != 1 || got.MessageIDs[0] != message.ID {
			t.Fatalf("message correlation lost: %#v", got.MessageIDs)
		}
		if got.Plan != "1. Inspect\n2. Verify" || got.Diff == "" || got.FinalText != "Verified successfully." {
			t.Fatalf("summary content incomplete: %#v", got)
		}
		if len(got.Items) != 1 || got.Items[0].Status != "completed" || !strings.Contains(got.Items[0].Detail, "completed") {
			t.Fatalf("work item projection incomplete: %#v", got.Items)
		}
		if !strings.Contains(string(got.Usage), "input_tokens") {
			t.Fatalf("usage missing: %s", got.Usage)
		}
	}
	assertSummary(t, engine.Snapshot())
	if err := engine.Close(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	reopenedStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := New(Config{Name: "ignored", Repo: t.TempDir(), Store: reopenedStore, Settings: model.DefaultRoomSettings()})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertSummary(t, reopened.Snapshot())
}

func TestTurnSummaryBoundsHighVolumeDetail(t *testing.T) {
	engine, _ := newTestEngine(t, model.RoutingManual, "")
	started := time.Now().UTC()
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorCodex, Kind: model.RuntimeTurnStarted, TurnID: "bounded", CreatedAt: started})
	payload := strings.Repeat("x", 20<<10)
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorCodex, Kind: model.RuntimeCommandOutput, TurnID: "bounded", ItemID: "command", Text: payload, CreatedAt: started.Add(time.Second)})
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorCodex, Kind: model.RuntimeTurnCompleted, TurnID: "bounded", CreatedAt: started.Add(2 * time.Second)})
	got := engine.Snapshot().Turns[0]
	if len(got.Items) != 1 || len(got.Items[0].Detail) > (12<<10)+3 || !strings.HasPrefix(got.Items[0].Detail, "…") {
		t.Fatalf("command output was not bounded: %d %#v", len(got.Items[0].Detail), got.Items)
	}
}

func TestWindowedSnapshotAndMessagesPage(t *testing.T) {
	engine, adapters := newTestEngine(t, model.RoutingManual, "")
	for i := 0; i < 12; i++ {
		message, err := engine.Send(context.Background(), SendRequest{
			Text: fmt.Sprintf("message-%02d", i), To: []model.ActorID{model.ActorClaude},
		})
		if err != nil {
			t.Fatal(err)
		}
		input := receiveInput(t, adapters[model.ActorClaude])
		if input.MessageID != message.ID {
			t.Fatalf("submission %d correlated to %q, want %q", i, input.MessageID, message.ID)
		}
	}

	window := engine.WindowedSnapshot(5)
	if len(window.Messages) != 5 {
		t.Fatalf("window messages = %d, want 5", len(window.Messages))
	}
	if window.MessageWindow == nil || window.MessageWindow.Total != 12 || window.MessageWindow.Loaded != 5 || !window.MessageWindow.HasMore {
		t.Fatalf("unexpected window metadata: %#v", window.MessageWindow)
	}
	if got := window.Messages[0].Text; got != "message-07" {
		t.Fatalf("oldest window message = %q, want message-07", got)
	}

	page := engine.MessagesPage(window.Messages[0].Seq, 4)
	if len(page.Messages) != 4 || page.Total != 12 || !page.HasMore {
		t.Fatalf("unexpected page: %#v", page)
	}
	if page.Messages[0].Text != "message-03" || page.Messages[3].Text != "message-06" {
		t.Fatalf("unexpected page order: %#v", page.Messages)
	}

	oldest := engine.MessagesPage(page.Messages[0].Seq, 10)
	if len(oldest.Messages) != 3 || oldest.HasMore {
		t.Fatalf("unexpected oldest page: %#v", oldest)
	}
	if oldest.Messages[0].Text != "message-00" || oldest.Messages[2].Text != "message-02" {
		t.Fatalf("unexpected oldest page messages: %#v", oldest.Messages)
	}
}
