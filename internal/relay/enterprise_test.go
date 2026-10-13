package relay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func whiteBoxAuth(userID string) EnterpriseAuth {
	return EnterpriseAuth{Deployment: "dep-test", UserID: userID}
}

// fakePort is a scripted enterprise port for the white-box engine tests.
type fakePort struct {
	mu        sync.Mutex
	snapshot  EnterpriseRoomSnapshot
	commits   int
	received  []EnterpriseSendCommand
	commitFn  func(cmd EnterpriseSendCommand) (EnterpriseSendResult, error)
	snapFn    func(auth EnterpriseAuth) (EnterpriseRoomSnapshot, error)
	committed chan EnterpriseSendCommand
}

func committedEcho(cmd EnterpriseSendCommand) EnterpriseSendResult {
	fact := cmd.Fact
	fact.Seq = cmd.ExpectedSeq + 1
	return EnterpriseSendResult{Outcome: EnterpriseOutcomeCommitted, Fact: fact, Seq: fact.Seq, Revision: cmd.ExpectedRevision + 1}
}

func (f *fakePort) CommitHumanSend(ctx context.Context, cmd EnterpriseSendCommand) (EnterpriseSendResult, error) {
	f.mu.Lock()
	f.commits++
	f.received = append(f.received, cmd)
	f.mu.Unlock()
	if f.committed != nil {
		f.committed <- cmd
	}
	if f.commitFn != nil {
		return f.commitFn(cmd)
	}
	return committedEcho(cmd), nil
}

func (f *fakePort) ResolveHumanSend(ctx context.Context, auth EnterpriseAuth, roomID, clientID string, intent EnterpriseSendIntent) (EnterpriseSendResult, error) {
	return EnterpriseSendResult{}, errors.New("fakePort: ResolveHumanSend not scripted")
}

func (f *fakePort) ReadonlySnapshot(ctx context.Context, auth EnterpriseAuth, roomID string) (EnterpriseRoomSnapshot, error) {
	if f.snapFn != nil {
		return f.snapFn(auth)
	}
	return f.snapshot, nil
}

func (f *fakePort) commitCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commits
}

func (f *fakePort) receivedAuths() []EnterpriseAuth {
	f.mu.Lock()
	defer f.mu.Unlock()
	auths := make([]EnterpriseAuth, 0, len(f.received))
	for _, cmd := range f.received {
		auths = append(auths, cmd.Auth)
	}
	return auths
}

func emptySnapshot(t *testing.T) EnterpriseRoomSnapshot {
	t.Helper()
	return snapshotWithFacts(t)
}

func snapshotWithFacts(t *testing.T, facts ...model.Event) EnterpriseRoomSnapshot {
	t.Helper()
	intent, err := model.PrepareEnterpriseRoomIntent("Test Room", model.Collaboration{})
	if err != nil {
		t.Fatal(err)
	}
	creation, err := model.NewEnterpriseRoomCreation("room-1", "dep-test", "org-test", "u-owner", intent, time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	creation.ID = "evt-creation"
	events := append([]model.Event{creation}, facts...)
	return EnterpriseRoomSnapshot{RoomID: "room-1", HostMode: "native", DeploymentID: "dep-test", OrgID: "org-test", CreationUserID: "u-owner", Events: events, Seq: uint64(len(events)), Revision: uint64(len(events))}
}

func enterpriseTestEngine(t *testing.T, port EnterprisePort) *Engine {
	t.Helper()
	engine, err := OpenEnterprise(context.Background(), Config{
		RoomID: "room-1",
	}, port, whiteBoxAuth("u-owner"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return engine
}

func originalMessageFact(t *testing.T, messageID, clientID, text string) model.Event {
	t.Helper()
	now := time.Now().UTC()
	fact, err := model.NewEvent("room-1", EventMessage, model.ActorUser, messageFact{
		Message: Message{
			ID: messageID, From: model.ActorUser, To: model.ActorSlot2, Text: text,
			State: "queued", TargetGeneration: 1, CreatedAt: now, UpdatedAt: now, Source: "send",
		},
		ClientKey: enterpriseClientKey("u-owner", clientID),
	})
	if err != nil {
		t.Fatal(err)
	}
	fact.Seq = 2
	return fact
}

// Every legacy mutator is rejected at its entry on an enterprise engine,
// before any port call or state change.
func TestEnterpriseRejectsLegacyMutators(t *testing.T) {
	port := &fakePort{snapshot: emptySnapshot(t)}
	e := enterpriseTestEngine(t, port)
	ctx := context.Background()
	auth := Auth{}
	type call struct {
		name string
		err  error
	}
	var calls []call
	_, err := e.Send(auth, SendRequest{})
	calls = append(calls, call{"Send", err})
	_, err = e.SendUser(SendRequest{})
	calls = append(calls, call{"SendUser", err})
	_, err = e.Report(auth, 1, "x")
	calls = append(calls, call{"Report", err})
	_, err = e.Claim(ctx, auth, false)
	calls = append(calls, call{"Claim", err})
	calls = append(calls, call{"Ack", e.Ack(auth, "i", "r")})
	calls = append(calls, call{"Cancel", e.Cancel("i")})
	_, err = e.Retry("i")
	calls = append(calls, call{"Retry", err})
	calls = append(calls, call{"Reap", e.Reap()})
	calls = append(calls, call{"Failure", e.Failure(auth, "x")})
	_, err = e.Bind(model.ActorSlot1, BindRequest{})
	calls = append(calls, call{"Bind", err})
	_, _, err = e.BindReport(model.ActorSlot1, BindRequest{})
	calls = append(calls, call{"BindReport", err})
	_, err = e.ConfirmSession(auth, "s", "t")
	calls = append(calls, call{"ConfirmSession", err})
	calls = append(calls, call{"Unbind", e.Unbind(model.ActorSlot1)})
	calls = append(calls, call{"Park", e.Park(model.ActorSlot1, true)})
	calls = append(calls, call{"UnbindAs", e.UnbindAs(auth)})
	calls = append(calls, call{"ParkAs", e.ParkAs(auth, true)})
	calls = append(calls, call{"SetWakeEnabled", e.SetWakeEnabled(true)})
	calls = append(calls, call{"ReserveWake", e.ReserveWake("m", model.ActorSlot1)})
	calls = append(calls, call{"RecordWake", e.RecordWake("o", "r", model.ActorSlot1)})
	calls = append(calls, call{"RecordWakeAttempt", e.RecordWakeAttempt("m", "o", "r", model.ActorSlot1)})
	_, err = e.CreateLANInvite()
	calls = append(calls, call{"CreateLANInvite", err})
	_, _, err = e.RequestLANJoin(LANJoinRequest{})
	calls = append(calls, call{"RequestLANJoin", err})
	_, err = e.AcceptLANJoin("r", "k")
	calls = append(calls, call{"AcceptLANJoin", err})
	calls = append(calls, call{"RevokeLANMember", e.RevokeLANMember()})
	_, err = e.SendLANUser(auth, SendRequest{})
	calls = append(calls, call{"SendLANUser", err})
	_, err = e.PrepareHead(ctx, auth, false)
	calls = append(calls, call{"PrepareHead", err})
	_, err = e.ClaimPrepared(ctx, auth, "i", "d", false)
	calls = append(calls, call{"ClaimPrepared", err})
	_, err = e.ConfirmLAN(auth)
	calls = append(calls, call{"ConfirmLAN", err})
	calls = append(calls, call{"ReserveLANWake", e.ReserveLANWake(auth, "i")})
	calls = append(calls, call{"RecordLANWake", e.RecordLANWake(auth, "i", "o", "r")})
	calls = append(calls, call{"AuthorizedLANEffect", e.AuthorizedLANEffect(auth, func() error { return nil })})
	_, err = e.BeginLANTransfer(ctx, auth, nil)
	calls = append(calls, call{"BeginLANTransfer", err})
	_, err = e.ReclaimAttachments()
	calls = append(calls, call{"ReclaimAttachments", err})
	for _, c := range calls {
		if !errors.Is(c.err, ErrEnterpriseReadOnly) {
			t.Fatalf("%s: err = %v, want ErrEnterpriseReadOnly", c.name, err)
		}
	}
	if port.commitCalls() != 0 {
		t.Fatalf("legacy rejections reached the port: %d commits", port.commitCalls())
	}
	if e.Snapshot().Sequence != 1 {
		t.Fatal("legacy rejections changed the projection")
	}
	if e.TryBeginIdleClose() {
		t.Fatal("enterprise engine must never idle-close itself")
	}
}

// One engine serves several authenticated users: the per-command identity
// reaches the port unchanged, the same client ID under two users stays
// independent, and a foreign deployment is refused before any port call.
func TestEnterprisePerCommandIdentity(t *testing.T) {
	port := &fakePort{snapshot: emptySnapshot(t)}
	e := enterpriseTestEngine(t, port)
	ctx := context.Background()

	first, err := e.ExecuteHumanSend(ctx, whiteBoxAuth("u-owner"), SendRequest{ID: "same", Text: "one", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.ExecuteHumanSend(ctx, whiteBoxAuth("u-owner2"), SendRequest{ID: "same", Text: "two", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("two users shared one message identity")
	}
	auths := port.receivedAuths()
	if len(auths) != 2 || auths[0].UserID != "u-owner" || auths[1].UserID != "u-owner2" {
		t.Fatalf("per-command identities at the port: %+v", auths)
	}
	if _, err := e.ExecuteHumanSend(ctx, EnterpriseAuth{Deployment: "other", UserID: "u-owner"}, SendRequest{ID: "x", Text: "x", To: model.ActorSlot2}); err == nil {
		t.Fatal("foreign deployment accepted")
	}
	if port.commitCalls() != 2 {
		t.Fatalf("foreign deployment reached the port: %d commits", port.commitCalls())
	}
}

// A confirmed commit applies and notifies exactly once, and the gate is held
// for the whole plan -> SQL -> apply sequence (probed from inside apply).
func TestEnterpriseExecuteCommittedOnceWithGateProbe(t *testing.T) {
	port := &fakePort{snapshot: emptySnapshot(t)}
	var appends, attentions int
	var gateHeldDuringApply bool
	engine, err := OpenEnterprise(context.Background(), Config{
		RoomID:      "room-1",
		OnAttention: func(Attention) { attentions++ },
	}, port, whiteBoxAuth("u-owner"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	engine.cfg.OnAppend = func(model.Event) {
		appends++
		select {
		case engine.gate <- struct{}{}:
			t.Error("gate was free during apply")
			<-engine.gate
		default:
			gateHeldDuringApply = true
		}
	}

	message, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner"), SendRequest{ID: "c1", Text: "hello", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	if message.ID == "" || message.From != model.ActorUser || message.To != model.ActorSlot2 || message.State != "queued" || message.TargetGeneration != 1 {
		t.Fatalf("message: %+v", message)
	}
	if !gateHeldDuringApply {
		t.Fatal("apply ran without the gate held")
	}
	// A plain queued append derives no attention in the Local contract either;
	// the OnAttention pipeline order is exercised structurally by the shared
	// post-commit block.
	if appends != 1 || attentions != 0 {
		t.Fatalf("notifications: appends=%d attentions=%d", appends, attentions)
	}
	if port.commitCalls() != 1 || engine.Snapshot().Sequence != 2 {
		t.Fatalf("commits=%d sequence=%d", port.commitCalls(), engine.Snapshot().Sequence)
	}
}

// A replayed result returns the canonical original message, applies and
// notifies nothing, and never lowers the projection cursor.
func TestEnterpriseExecuteReplayedCanonicalAndCursor(t *testing.T) {
	original := originalMessageFact(t, "relay-orig", "orig", "original body")
	port := &fakePort{snapshot: snapshotWithFacts(t, original)}
	port.commitFn = func(cmd EnterpriseSendCommand) (EnterpriseSendResult, error) {
		return EnterpriseSendResult{Outcome: EnterpriseOutcomeReplayed, Fact: original, Seq: 2, Revision: 2}, nil
	}
	var appends, attentions int
	engine, err := OpenEnterprise(context.Background(), Config{
		RoomID:      "room-1",
		OnAppend:    func(model.Event) { appends++ },
		OnAttention: func(Attention) { attentions++ },
	}, port, whiteBoxAuth("u-owner"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	message, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner"), SendRequest{ID: "orig", Text: "original body", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	if message.ID != "relay-orig" || message.Text != "original body" {
		t.Fatalf("canonical replay: %+v", message)
	}
	if appends != 0 || attentions != 0 {
		t.Fatalf("replay notified: appends=%d attentions=%d", appends, attentions)
	}
	if engine.Snapshot().Sequence != 2 {
		t.Fatalf("cursor moved on replay: %d", engine.Snapshot().Sequence)
	}
}

// A lost outcome fails the engine closed and stays there: read-only
// verification at the port does not restore write capability, and a second
// command is refused.
func TestEnterpriseUnknownFailsClosedAndStaysClosed(t *testing.T) {
	port := &fakePort{snapshot: emptySnapshot(t)}
	port.commitFn = func(cmd EnterpriseSendCommand) (EnterpriseSendResult, error) {
		return EnterpriseSendResult{}, &EnterpriseCommandError{Outcome: EnterpriseOutcomeUnknown, Class: "connection_exception", Err: errors.New("injected loss")}
	}
	var appends, attentions int
	engine, err := OpenEnterprise(context.Background(), Config{
		RoomID:      "room-1",
		OnAppend:    func(model.Event) { appends++ },
		OnAttention: func(Attention) { attentions++ },
	}, port, whiteBoxAuth("u-owner"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })

	if _, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner"), SendRequest{ID: "u1", Text: "lost", To: model.ActorSlot2}); err == nil {
		t.Fatal("lost outcome reported success")
	}
	if engine.Fatal() == nil {
		t.Fatal("lost outcome did not fail the engine closed")
	}
	if appends != 0 || attentions != 0 {
		t.Fatal("lost outcome applied or notified")
	}
	if _, err := port.ReadonlySnapshot(context.Background(), whiteBoxAuth("u-owner"), "room-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner"), SendRequest{ID: "u2", Text: "later", To: model.ActorSlot2}); err == nil {
		t.Fatal("the engine accepted a write after a lost outcome")
	}
	if err := engine.SetWakeEnabled(true); !errors.Is(err, ErrEnterpriseReadOnly) {
		t.Fatalf("legacy mutator after loss: %v", err)
	}
}

// One revision conflict rebuilds from a read-only snapshot under the command
// identity and replans once; a second conflict fails the engine closed and
// signals, and every later command is refused with zero port calls.
func TestEnterpriseConflictRebuildsOnce(t *testing.T) {
	pushed := originalMessageFact(t, "relay-pushed", "pushed", "peer fact")
	port := &fakePort{snapshot: emptySnapshot(t)}
	var committedFact model.Event
	conflicts := 0
	port.commitFn = func(cmd EnterpriseSendCommand) (EnterpriseSendResult, error) {
		conflicts++
		if conflicts == 1 {
			return EnterpriseSendResult{}, &EnterpriseCommandError{Outcome: EnterpriseOutcomeRefused, Class: EnterpriseClassRevisionConflict, Err: errors.New("moved")}
		}
		fact := cmd.Fact
		fact.Seq = cmd.ExpectedSeq + 1
		committedFact = fact
		return EnterpriseSendResult{Outcome: EnterpriseOutcomeCommitted, Fact: fact, Seq: fact.Seq, Revision: cmd.ExpectedRevision + 1}, nil
	}
	// The constructor consumes the initial creation-only snapshot; only the rebuild
	// calls snapFn, and it must use the per-command identity.
	engine := enterpriseTestEngine(t, port)
	port.snapFn = func(auth EnterpriseAuth) (EnterpriseRoomSnapshot, error) {
		if auth.UserID != "u-owner2" {
			t.Errorf("rebuild used the wrong command identity: %+v", auth)
		}
		return snapshotWithFacts(t, pushed), nil
	}

	message, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner2"), SendRequest{ID: "r1", Text: "replan", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	if message.ID == "" {
		t.Fatalf("message: %+v", message)
	}
	if port.commitCalls() != 2 {
		t.Fatalf("commit attempts = %d, want 2", port.commitCalls())
	}
	if engine.Snapshot().Sequence != 3 {
		t.Fatalf("sequence after rebuild+commit = %d, want 3", engine.Snapshot().Sequence)
	}

	// Second-conflict path: the snapshot reflects the engine's own committed
	// fact; the conflict is terminal — fatal + signal, later commands refused
	// with zero additional port calls.
	port.snapFn = func(auth EnterpriseAuth) (EnterpriseRoomSnapshot, error) {
		return snapshotWithFacts(t, pushed, committedFact), nil
	}
	port.commitFn = func(cmd EnterpriseSendCommand) (EnterpriseSendResult, error) {
		return EnterpriseSendResult{}, &EnterpriseCommandError{Outcome: EnterpriseOutcomeRefused, Class: EnterpriseClassRevisionConflict, Err: errors.New("moved again")}
	}
	if _, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner2"), SendRequest{ID: "r2", Text: "conflict", To: model.ActorSlot2}); err == nil {
		t.Fatal("second conflict reported success")
	}
	if engine.Fatal() == nil {
		t.Fatal("second conflict did not fail the engine closed")
	}
	attempts := port.commitCalls()
	if _, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner2"), SendRequest{ID: "r3", Text: "after", To: model.ActorSlot2}); err == nil {
		t.Fatal("the engine accepted a command after the terminal conflict")
	}
	if port.commitCalls() != attempts {
		t.Fatalf("a refused command reached the port: %d -> %d", attempts, port.commitCalls())
	}
}

// A read-only rebuild that advances the projection closes the previous change
// channel — the deterministic signal proof — and wakes a real WaitChanges
// waiter, staying silent: no OnAppend/OnAttention on the rebuild path. A
// per-request authorization refusal after the rebuild must not poison the
// Room.
func TestEnterpriseRebuildSignalsWaitersSilently(t *testing.T) {
	pushed := originalMessageFact(t, "relay-pushed2", "pushed2", "peer fact")
	port := &fakePort{snapshot: emptySnapshot(t)} // constructor: creation at sequence 1
	conflicts := 0
	port.commitFn = func(cmd EnterpriseSendCommand) (EnterpriseSendResult, error) {
		conflicts++
		if conflicts == 1 {
			return EnterpriseSendResult{}, &EnterpriseCommandError{Outcome: EnterpriseOutcomeRefused, Class: EnterpriseClassRevisionConflict, Err: errors.New("moved")}
		}
		return EnterpriseSendResult{}, &EnterpriseCommandError{Outcome: EnterpriseOutcomeRefused, Class: EnterpriseClassAuthorization, Err: errors.New("denied after rebuild")}
	}
	var appends, attentions int
	engine, err := OpenEnterprise(context.Background(), Config{
		RoomID:      "room-1",
		OnAppend:    func(model.Event) { appends++ },
		OnAttention: func(Attention) { attentions++ },
	}, port, whiteBoxAuth("u-owner"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	// Only the rebuild consumes the advanced snapshot.
	port.snapFn = func(auth EnterpriseAuth) (EnterpriseRoomSnapshot, error) {
		return snapshotWithFacts(t, pushed), nil
	}

	// Capture the live change channel under the lock: the rebuild must close
	// exactly this one to signal.
	engine.mu.Lock()
	previousChannel := engine.changed
	engine.mu.Unlock()

	woke := make(chan error, 1)
	go func() { woke <- engine.WaitChanges(context.Background(), 1) }()

	if _, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner"), SendRequest{ID: "w1", Text: "wake", To: model.ActorSlot2}); err == nil {
		t.Fatal("retry after rebuild unexpectedly succeeded")
	}
	if engine.Fatal() != nil {
		t.Fatalf("a per-request authorization refusal poisoned the room: %v", engine.Fatal())
	}
	select {
	case <-previousChannel:
		// The rebuild closed the previous channel: the signal is proven.
	default:
		t.Fatal("rebuild did not close the previous change channel")
	}
	select {
	case werr := <-woke:
		if werr != nil {
			t.Fatalf("waiter woke with %v", werr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not wake on the rebuild")
	}
	if appends != 0 || attentions != 0 {
		t.Fatalf("rebuild notified: appends=%d attentions=%d", appends, attentions)
	}
	if engine.Snapshot().Sequence != 2 {
		t.Fatalf("sequence after rebuild = %d, want 2", engine.Snapshot().Sequence)
	}
}

// A failed rebuild leaves the existing projection untouched, and the
// configuration is isolated per engine.
func TestEnterpriseRebuildFailureIsolatesProjection(t *testing.T) {
	original := originalMessageFact(t, "relay-keep", "keep", "kept body")
	port := &fakePort{snapshot: snapshotWithFacts(t, original)}
	bad := model.Event{Seq: 2, ID: "evt-bad", RoomID: "room-1", Kind: EventBinding, Actor: model.ActorSlot1, CreatedAt: time.Now().UTC(), Data: []byte(`{malformed`)}
	port.commitFn = func(cmd EnterpriseSendCommand) (EnterpriseSendResult, error) {
		return EnterpriseSendResult{}, &EnterpriseCommandError{Outcome: EnterpriseOutcomeRefused, Class: EnterpriseClassRevisionConflict, Err: errors.New("moved")}
	}
	// The constructor consumes the good snapshot; only the rebuild call sees
	// the malformed one.
	engine := enterpriseTestEngine(t, port)
	port.snapFn = func(auth EnterpriseAuth) (EnterpriseRoomSnapshot, error) {
		return snapshotWithFacts(t, bad), nil
	}
	if len(engine.Snapshot().Messages) != 1 || engine.Snapshot().Messages[0].ID != "relay-keep" {
		t.Fatalf("setup projection: %+v", engine.Snapshot().Messages)
	}
	if _, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner"), SendRequest{ID: "f1", Text: "x", To: model.ActorSlot2}); err == nil {
		t.Fatal("malformed rebuild snapshot accepted")
	}
	if engine.Fatal() == nil {
		t.Fatal("failed rebuild did not fail closed")
	}
	if len(engine.Snapshot().Messages) != 1 || engine.Snapshot().Messages[0].ID != "relay-keep" {
		t.Fatalf("failed rebuild mutated the projection: %+v", engine.Snapshot().Messages)
	}

	// Configuration isolation: mutating the caller's Runtimes map after
	// construction must not change the engine's map.
	caller := map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeAwaitingBinding, model.ActorSlot2: model.RuntimeAwaitingBinding}
	port2 := &fakePort{snapshot: emptySnapshot(t)}
	engine2, err := OpenEnterprise(context.Background(), Config{RoomID: "room-1", Runtimes: caller}, port2, whiteBoxAuth("u-owner"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine2.Close() })
	caller[model.ActorSlot2] = model.RuntimeClaude
	if engine2.cfg.Runtimes[model.ActorSlot2] != model.RuntimeAwaitingBinding {
		t.Fatal("the engine's Runtimes map was polluted by the caller")
	}
}

// The gate serializes whole commands: a queued command obeys its own deadline,
// and an unbounded caller still gets the internal budget.
func TestEnterpriseGateQueuesCancelsAndBudgetBound(t *testing.T) {
	port := &fakePort{snapshot: emptySnapshot(t), committed: make(chan EnterpriseSendCommand, 4)}
	hold := make(chan struct{})
	port.commitFn = func(cmd EnterpriseSendCommand) (EnterpriseSendResult, error) {
		<-hold
		return committedEcho(cmd), nil
	}
	engine := enterpriseTestEngine(t, port)

	// An unbounded caller still gets the internal budget: acquire the gate
	// manually and inspect the op context the queueing call returns with.
	engine.gate <- struct{}{}
	type acquired struct {
		ctx context.Context
		rel func()
		err error
	}
	got := make(chan acquired, 1)
	go func() {
		opCtx, release, err := engine.beginEnterpriseCommand(context.Background())
		got <- acquired{ctx: opCtx, rel: release, err: err}
	}()
	select {
	case <-got:
		t.Fatal("queued acquisition returned while the gate was held")
	case <-time.After(100 * time.Millisecond):
	}
	<-engine.gate
	acq := <-got
	if acq.err != nil {
		t.Fatal(acq.err)
	}
	if deadline, ok := acq.ctx.Deadline(); !ok || time.Until(deadline) > enterpriseCommandBudget+2*time.Second || time.Until(deadline) < enterpriseCommandBudget-2*time.Second {
		t.Fatalf("op context deadline is not the internal budget: %v", deadline)
	}
	acq.rel()
	if enterpriseCommandBudget != 60*time.Second {
		t.Fatalf("enterpriseCommandBudget = %v", enterpriseCommandBudget)
	}

	// A queued command obeys its own earlier deadline and never reaches SQL.
	firstDone := make(chan error, 1)
	go func() {
		_, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner"), SendRequest{ID: "g1", Text: "first", To: model.ActorSlot2})
		firstDone <- err
	}()
	<-port.committed // the first command reached SQL; the gate is held

	queuedCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := engine.ExecuteHumanSend(queuedCtx, whiteBoxAuth("u-owner"), SendRequest{ID: "g2", Text: "queued", To: model.ActorSlot2}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued command error = %v, want DeadlineExceeded", err)
	}
	if port.commitCalls() != 1 {
		t.Fatalf("queued command reached the port: %d calls", port.commitCalls())
	}

	close(hold)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

// CloseContext is cancellable and honest: a timed-out wait does not claim the
// engine closed, the context is re-checked after the token is acquired (a
// cancellation racing the token never starts the close), and a successful
// close performs no reap and no port call.
func TestEnterpriseCloseContextBounded(t *testing.T) {
	port := &fakePort{snapshot: emptySnapshot(t), committed: make(chan EnterpriseSendCommand, 2)}
	hold := make(chan struct{})
	port.commitFn = func(cmd EnterpriseSendCommand) (EnterpriseSendResult, error) {
		<-hold
		return committedEcho(cmd), nil
	}
	engine := enterpriseTestEngine(t, port)
	go func() {
		_, _ = engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner"), SendRequest{ID: "cl1", Text: "hold", To: model.ActorSlot2})
	}()
	<-port.committed

	closeCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := engine.CloseContext(closeCtx); err == nil {
		t.Fatal("a timed-out close claimed success")
	}
	engine.mu.Lock()
	closed := engine.closed
	engine.mu.Unlock()
	if closed {
		t.Fatal("a timed-out close claimed the engine closed")
	}

	// Cancellation racing the token: with the gate free and the context
	// already cancelled, the post-token re-check must refuse the close.
	raceCtx, raceCancel := context.WithCancel(context.Background())
	raceCancel()
	if err := engine.CloseContext(raceCtx); err == nil {
		t.Fatal("a cancelled close claimed success")
	}
	engine.mu.Lock()
	closed = engine.closed
	engine.mu.Unlock()
	if closed {
		t.Fatal("a cancelled close claimed the engine closed")
	}

	close(hold)
	before := port.commitCalls()
	if err := engine.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if port.commitCalls() != before {
		t.Fatal("close called the port")
	}
	if _, err := engine.ExecuteHumanSend(context.Background(), whiteBoxAuth("u-owner"), SendRequest{ID: "cl2", Text: "after", To: model.ActorSlot2}); err == nil {
		t.Fatal("closed engine accepted a command")
	}
}

// The constructor validates the port and the snapshot before building any
// projection.
func TestOpenEnterpriseValidation(t *testing.T) {
	cfg := Config{RoomID: "room-1"}
	auth := whiteBoxAuth("u-owner")
	if _, err := OpenEnterprise(context.Background(), cfg, nil, auth); err == nil {
		t.Fatal("nil port accepted")
	}
	var typedNil *fakePort
	if _, err := OpenEnterprise(context.Background(), cfg, typedNil, auth); err == nil {
		t.Fatal("typed-nil port accepted")
	}
	wrongRoom := &fakePort{snapshot: EnterpriseRoomSnapshot{RoomID: "room-2", HostMode: "native"}}
	if _, err := OpenEnterprise(context.Background(), cfg, wrongRoom, auth); err == nil {
		t.Fatal("wrong-room snapshot accepted")
	}
	wrongHost := &fakePort{snapshot: EnterpriseRoomSnapshot{RoomID: "room-1", HostMode: "embedded"}}
	if _, err := OpenEnterprise(context.Background(), cfg, wrongHost, auth); err == nil {
		t.Fatal("non-native snapshot accepted")
	}
	gapped := &fakePort{snapshot: EnterpriseRoomSnapshot{RoomID: "room-1", HostMode: "native", Seq: 2}}
	if _, err := OpenEnterprise(context.Background(), cfg, gapped, auth); err == nil {
		t.Fatal("gapped snapshot accepted")
	}
}
