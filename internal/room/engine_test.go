package room

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/bus"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

type fakeAdapter struct {
	actor       model.ActorID
	sink        agent.EventSink
	submissions chan model.AgentInput
	mu          sync.Mutex
	state       model.AgentState
	sessionID   string
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
	select {
	case f.submissions <- input:
		return model.DeliveryStarted, nil
	case <-ctx.Done():
		return model.DeliveryFailed, ctx.Err()
	}
}
func (f *fakeAdapter) Interrupt(context.Context) error { return nil }
func (f *fakeAdapter) Stop(context.Context) error {
	f.mu.Lock()
	f.state = model.StateStopped
	f.mu.Unlock()
	return nil
}
func (f *fakeAdapter) ResolveApproval(context.Context, string, string) error {
	return agent.ErrApprovalUnsupported
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
