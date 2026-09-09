package room

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

type configurationCapture struct {
	mu     sync.Mutex
	values []agent.Config
}

func (c *configurationCapture) factory(cfg agent.Config, sink agent.EventSink) agent.Adapter {
	c.mu.Lock()
	c.values = append(c.values, cfg)
	c.mu.Unlock()
	return &fakeAdapter{actor: cfg.Actor, sink: sink, state: model.StateStopped, sessionID: cfg.SessionID, submissions: make(chan model.AgentInput, 16)}
}
func (c *configurationCapture) latest(actor model.ActorID) agent.Config {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.values) - 1; i >= 0; i-- {
		if c.values[i].Actor == actor {
			return c.values[i]
		}
	}
	return agent.Config{}
}
func (c *configurationCapture) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.values) }

func newCollaborationEngine(t *testing.T, instructions string) (*Engine, *configurationCapture) {
	t.Helper()
	spec := model.Collaboration{}
	if instructions != "" {
		spec.Mode = model.CollaborationCustom
		spec.Instructions = instructions
	}
	spec, err := spec.ForCreation()
	if err != nil {
		t.Fatal(err)
	}
	eventStore, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	captures := &configurationCapture{}
	e, err := New(Config{Name: "mode-test", Repo: repo, Store: eventStore, Collaboration: &spec,
		ClaudeConfig:  agent.Config{Runtime: model.RuntimeClaude, PermissionMode: "yolo", Model: "planning-model", AdditionalInstructions: "user extra", SessionID: "session-a", RequireExactSession: true},
		CodexConfig:   agent.Config{Runtime: model.RuntimeCodex, ApprovalPolicy: "yolo", Model: "execution-model", SessionID: "session-b", RequireExactSession: true},
		ClaudeFactory: captures.factory, CodexFactory: captures.factory})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e, captures
}

func TestCreationModeSeparatesResponsibilitiesFromWorkspaceAndPermissions(t *testing.T) {
	e, captures := newCollaborationEngine(t, "")
	s := e.Snapshot()
	if s.Meta.Collaboration == nil || s.Meta.Collaboration.Mode != model.CollaborationDefault {
		t.Fatal("missing durable mode")
	}
	for _, actor := range model.SlotActors() {
		p := s.Participants[actor]
		if p.Role != model.RolePeer || p.PermissionProfile != model.PermissionConfigured || p.Workspace.Path != s.Meta.Repo || p.Workspace.ReadOnly {
			t.Fatalf("new responsibility acquired a legacy workspace restriction: %+v", p)
		}
		cfg := captures.latest(actor)
		if cfg.Collaboration == nil || *cfg.Collaboration != *s.Meta.Collaboration {
			t.Fatal("mode not forwarded to native instructions")
		}
	}
	if s.Participants[model.ActorClaude].Responsibility != "lead" || s.Participants[model.ActorCodex].Responsibility != "executor" {
		t.Fatal("wrong display responsibilities")
	}
	if cfg := captures.latest(model.ActorCodex); cfg.ApprovalPolicy != "yolo" || cfg.Sandbox != "danger-full-access" {
		t.Fatalf("Executor is not YOLO: %+v", cfg)
	}
	for _, text := range []string{"@driver inspect", "@reviewer inspect", "@lead inspect", "@executor inspect"} {
		if _, err := e.Send(context.Background(), SendRequest{Text: text}); err == nil {
			t.Fatalf("removed alias %q silently targeted Agent 1", text)
		}
	}
	s.Meta.Collaboration.Instructions = "outside mutation"
	if e.Snapshot().Meta.Collaboration.Instructions != model.DefaultCollaborationInstructions {
		t.Fatal("snapshot mutated stored mode")
	}
}

func TestPermissionChangesPersistWithoutRewritingModeOrNativeIdentity(t *testing.T) {
	e, captures := newCollaborationEngine(t, "Agent 2 plans. Agent 1 implements. Keep tests focused.")
	ctx := context.Background()
	// Materialized identity is an event-sourced fact, not only an adapter value.
	e.updateParticipant(model.ActorCodex, func(p *model.ParticipantSnapshot) { p.SessionID = "session-b" })
	original := *e.Snapshot().Meta.Collaboration
	for _, profile := range []model.PermissionProfile{model.PermissionReadOnly, model.PermissionYOLO, model.PermissionConfigured} {
		if err := e.SetPermissions(ctx, model.ActorCodex, profile); err != nil {
			t.Fatal(err)
		}
		s := e.Snapshot()
		cfg := captures.latest(model.ActorCodex)
		if *s.Meta.Collaboration != original || s.Participants[model.ActorCodex].Responsibility != "participant" || cfg.SessionID != "session-b" || !cfg.RequireExactSession || cfg.Model != "execution-model" || cfg.Repo != s.Meta.Repo {
			t.Fatalf("permission mutated collaboration or identity: %+v %+v", s.Meta, cfg)
		}
		if profile == model.PermissionReadOnly && (cfg.Sandbox != "read-only" || cfg.ApprovalPolicy != "on-request") {
			t.Fatalf("bad restriction: %+v", cfg)
		}
		if profile != model.PermissionReadOnly && cfg.Sandbox != "danger-full-access" {
			t.Fatalf("wrong restored sandbox: %+v", cfg)
		}
	}
	events, _ := e.cfg.Store.Load()
	requested := 0
	committed := 0
	for _, event := range events {
		if event.Kind == "participant.permissions.requested" {
			requested++
		}
		if event.Kind == EventPermissionsUpdated {
			committed++
			if requested < committed {
				t.Fatal("effective policy preceded human intent")
			}
		}
	}
	if requested != 3 || committed != 3 {
		t.Fatalf("requests/commits=%d/%d", requested, committed)
	}
	// A stopped/reopened Room uses the recorded effective policy, not launch defaults.
	if err := e.SetPermissions(ctx, model.ActorCodex, model.PermissionReadOnly); err != nil {
		t.Fatal(err)
	}
	dir := e.cfg.Store.Dir()
	cfg := e.cfg
	_ = e.Close()
	restoredStore, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Store = restoredStore
	// Simulates service defaults changing: persisted custom policy must win.
	defaultSpec, _ := (model.Collaboration{}).ForCreation()
	cfg.Collaboration = &defaultSpec
	restored, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err = restored.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := restored.Snapshot(); *got.Meta.Collaboration != original || got.Participants[model.ActorCodex].PermissionProfile != model.PermissionReadOnly {
		t.Fatalf("restore lost durable mode or permissions: %+v", got.Meta)
	}
	if cfg := captures.latest(model.ActorCodex); cfg.Sandbox != "read-only" || cfg.SessionID != "session-b" {
		t.Fatalf("restore widened native permissions: %+v", cfg)
	}
}

func TestExplicitModeChangeOnReopenFailsBeforeWriting(t *testing.T) {
	e, _ := newCollaborationEngine(t, "")
	dir := e.cfg.Store.Dir()
	cfg := e.cfg
	_ = e.Close()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	before, _ := st.Load()
	other, _ := (model.Collaboration{Mode: model.CollaborationCustom, Instructions: "Different instructions"}).ForCreation()
	cfg.Store = st
	cfg.Collaboration = &other
	cfg.RequireCollaborationMatch = true
	if _, err = New(cfg); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("mode mutation was not rejected: %v", err)
	}
	after, _ := st.Load()
	if len(after) != len(before) {
		t.Fatal("rejected mode mutation wrote events")
	}
}

func TestPermissionsRejectBusyAndCancelledWithoutSideEffects(t *testing.T) {
	e, captures := newCollaborationEngine(t, "")
	ctx := context.Background()
	before := e.Snapshot().LatestSeq
	count := captures.count()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := e.SetPermissions(cancelled, model.ActorCodex, model.PermissionReadOnly); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled permissions: %v", err)
	}
	e.turnMu.Lock()
	e.turnOwner = model.ActorClaude
	e.turnMu.Unlock()
	if err := e.SetPermissions(ctx, model.ActorCodex, model.PermissionReadOnly); err == nil {
		t.Fatal("policy changed during native ownership")
	}
	e.turnMu.Lock()
	e.turnOwner = ""
	e.turnQueue = []scheduledDelivery{{}}
	e.turnMu.Unlock()
	if err := e.SetPermissions(ctx, model.ActorCodex, model.PermissionReadOnly); err == nil {
		t.Fatal("policy changed with queued delivery")
	}
	e.turnMu.Lock()
	e.turnQueue = nil
	e.turnMu.Unlock()
	if e.Snapshot().LatestSeq != before || captures.count() != count {
		t.Fatal("blocked permission request had side effects")
	}

}

func TestStopFailureDoesNotCommitPermissionGrant(t *testing.T) {
	e, captures := newCollaborationEngine(t, "")
	current, _ := e.adapter(model.ActorClaude)
	f := current.(*fakeAdapter)
	f.mu.Lock()
	f.stopErr = errors.New("stop failed")
	f.mu.Unlock()
	before := captures.count()
	if err := e.SetPermissions(context.Background(), model.ActorClaude, model.PermissionYOLO); err == nil {
		t.Fatal("stop failure swallowed")
	}
	if e.Snapshot().Participants[model.ActorClaude].PermissionProfile != model.PermissionConfigured || captures.count() != before {
		t.Fatal("failed stop changed effective permissions")
	}
	f.mu.Lock()
	f.stopErr = nil
	f.mu.Unlock()
}

func TestCloseWaitsForPermissionReplacementAndStopsTheNewAdapter(t *testing.T) {
	e, _ := newCollaborationEngine(t, "")
	started, release := make(chan struct{}), make(chan struct{})
	next := &fakeAdapter{actor: model.ActorCodex, state: model.StateStopped, startStarted: started, startRelease: release, submissions: make(chan model.AgentInput, 1)}
	e.cfg.CodexFactory = func(_ agent.Config, sink agent.EventSink) agent.Adapter { next.sink = sink; return next }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Start the old adapter so a policy replacement also starts the new one.
	if err := e.StartAgent(ctx, model.ActorCodex); err != nil {
		t.Fatal(err)
	}
	e.updateParticipant(model.ActorCodex, func(p *model.ParticipantSnapshot) { p.State = model.StateIdle })
	changed := make(chan error, 1)
	go func() { changed <- e.SetPermissions(ctx, model.ActorCodex, model.PermissionReadOnly) }()
	select {
	case <-started:
	case err := <-changed:
		t.Fatalf("replacement failed: %v", err)
	case <-ctx.Done():
		t.Fatal("replacement did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close raced past a starting replacement: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if next.State() != model.StateStopped {
		t.Fatal("replacement process survived Room close")
	}
	if err := e.SetPermissions(ctx, model.ActorCodex, model.PermissionYOLO); err == nil {
		t.Fatal("closed Room accepted permission changes")
	}
}
