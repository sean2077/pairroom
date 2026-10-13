package relay_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
	"github.com/sean2077/pairroom/internal/sqlstore/sqltest"
	"github.com/sean2077/pairroom/internal/store"
)

func roomRuntimes() map[model.ActorID]model.RuntimeKind {
	return map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex}
}

func executeAuth(userID string) relay.EnterpriseAuth {
	return relay.EnterpriseAuth{Deployment: "dep-test", UserID: userID}
}

func openSQLStore(t *testing.T, dsn, schema string) *sqlstore.Store {
	t.Helper()
	dbStore, err := sqlstore.Open(context.Background(), sqlstore.Config{DSN: dsn, Schema: schema, DeploymentID: "dep-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbStore.Close() })
	return dbStore
}

// openRoom builds ONE engine per Room; the open identity is used for the
// constructor snapshot, and every command carries its own user.
func openRoom(t *testing.T, dbStore *sqlstore.Store, cfg relay.Config) *relay.Engine {
	t.Helper()
	cfg.RoomID = "room-1"
	engine, err := relay.OpenEnterprise(context.Background(), cfg, sqlstore.NewRoomPort(dbStore), executeAuth("u-owner"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return engine
}

// The SQL creation fact, rather than process configuration, supplies the
// Native v9 identity. Opening it leaves both participant slots unbound.
func TestEnterpriseTypedCreationOpensVacantNativeV9(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedEnterpriseRoom(t, dsn, schema)
	before := rowCounts3c(t, admin, "room-1")
	if before != [3]int64{1, 1, 1} {
		t.Fatalf("birth prefix: %v", before)
	}
	engine := openRoom(t, openSQLStore(t, dsn, schema), relay.Config{})
	snapshot := engine.Snapshot()
	if snapshot.HostMode != model.HostNative || !snapshot.DynamicBindings || snapshot.Protocol != "pairroom-protocol/v9" || snapshot.Sequence != 1 || len(snapshot.Messages) != 0 {
		t.Fatalf("creation projection: %+v", snapshot)
	}
	for _, slot := range []model.ActorID{model.ActorSlot1, model.ActorSlot2} {
		binding := snapshot.Bindings[slot]
		if engine.Runtimes()[slot] != model.RuntimeAwaitingBinding || snapshot.TargetGenerations[slot] != 1 || binding.Active || binding.BindID != "" || binding.SessionID != "" {
			t.Fatalf("slot %s inferred a binding: %+v runtimes=%v targets=%v", slot, binding, engine.Runtimes(), snapshot.TargetGenerations)
		}
	}
	if got := rowCounts3c(t, admin, "room-1"); got != before {
		t.Fatalf("opening creation wrote state: %v -> %v", before, got)
	}
}

// craftHumanSendCandidate builds a kernel-admissible candidate fact for the
// fixture-driven kernel path (the real producer is exercised in the parity
// test; this mirrors it for direct store commits).
func craftHumanSendCandidate(userID, clientID, to, text string) model.Event {
	now := time.Now().UTC().Truncate(time.Microsecond)
	payload, err := json.Marshal(map[string]any{
		"id":                "relay-" + clientID,
		"from":              "user",
		"to":                to,
		"text":              text,
		"state":             "queued",
		"target_generation": 1,
		"created_at":        now,
		"updated_at":        now,
		"source":            "send",
		"client_key":        "user/" + userID + "/" + clientID,
	})
	if err != nil {
		panic(err)
	}
	return model.Event{ID: "evt-" + userID + "-" + clientID, RoomID: "room-1", Kind: "native.message.updated", Actor: model.ActorUser, CreatedAt: now, Data: payload}
}

// The real-PG happy path, canonical replay, per-command principal isolation
// and live authorization — all on ONE engine (one shared gate).
func TestEnterpriseHumanSendOverRealPostgres(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedEnterpriseRoom(t, dsn, schema, "u-owner2")
	beforeCounts := rowCounts3c(t, admin, "room-1")
	engine := openRoom(t, openSQLStore(t, dsn, schema), relay.Config{})
	ctx := context.Background()

	message, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "e1", Text: "hello", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	if message.ID == "" || message.From != model.ActorUser || message.To != model.ActorSlot2 || message.State != "queued" || message.TargetGeneration != 1 {
		t.Fatalf("message: %+v", message)
	}

	replayed, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "e1", Text: "hello", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != message.ID {
		t.Fatalf("replay returned %q, want the canonical %q", replayed.ID, message.ID)
	}

	var events, operations, audits int64
	if err := admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM room_events), (SELECT count(*) FROM operations), (SELECT count(*) FROM audit)`).Scan(&events, &operations, &audits); err != nil {
		t.Fatal(err)
	}
	if events != beforeCounts[0]+1 || operations != beforeCounts[1]+1 || audits != beforeCounts[2]+1 {
		t.Fatalf("rows: events=%d operations=%d audits=%d", events, operations, audits)
	}

	// Same engine, same client ID, different principal: an independent fact.
	other, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner2"), relay.SendRequest{ID: "e1", Text: "hello", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == message.ID {
		t.Fatal("two principals shared one message identity")
	}

	// A viewer may read but not write; a disabled member is refused while the
	// other live member keeps writing.
	_, err = engine.ExecuteHumanSend(ctx, executeAuth("u-viewer"), relay.SendRequest{ID: "e2", Text: "x", To: model.ActorSlot2})
	var commandErr *relay.EnterpriseCommandError
	if !errors.As(err, &commandErr) || commandErr.Class != "authorization" {
		t.Fatalf("viewer write: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE users SET disabled = true WHERE user_id = 'u-owner2'`); err != nil {
		t.Fatal(err)
	}
	_, err = engine.ExecuteHumanSend(ctx, executeAuth("u-owner2"), relay.SendRequest{ID: "e3", Text: "x", To: model.ActorSlot2})
	if !errors.As(err, &commandErr) || commandErr.Class != "authorization" {
		t.Fatalf("disabled write: %v", err)
	}
	if _, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "e4", Text: "still live", To: model.ActorSlot2}); err != nil {
		t.Fatalf("a live member was blocked by another member's disable: %v", err)
	}
}

// Closing the Room engine leaves the shared store untouched: a second engine
// over the same Store keeps committing.
func TestEnterpriseCloseLeavesTheStoreUsable(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	sqltest.SeedEnterpriseRoom(t, dsn, schema)
	dbStore := openSQLStore(t, dsn, schema)
	ctx := context.Background()

	first := openRoom(t, dbStore, relay.Config{})
	if _, err := first.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "c1", Text: "one", To: model.ActorSlot2}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "c2", Text: "after close", To: model.ActorSlot2}); err == nil {
		t.Fatal("closed engine accepted a command")
	}

	second := openRoom(t, dbStore, relay.Config{})
	message, err := second.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "c3", Text: "two", To: model.ActorSlot2})
	if err != nil {
		t.Fatalf("store was disturbed by the Room close: %v", err)
	}
	if message.ID == "" {
		t.Fatalf("message: %+v", message)
	}
}

// A lost commit response over the real protocol proxy: the engine fails
// closed, read-only verification proves the accepted result, and the engine
// still refuses writes afterwards — recovery-write capability is explicitly
// not implemented in this slice.
func TestEnterpriseUnknownFailsClosedWithProxy(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	sqltest.SeedEnterpriseRoom(t, dsn, schema)
	proxy := sqltest.StartProxy(t, "drop-response")
	dbStore := openSQLStore(t, proxy.DSN(dsn), schema)
	engine := openRoom(t, dbStore, relay.Config{})
	ctx := context.Background()

	proxy.Arm()
	if _, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "x1", Text: "lost body", To: model.ActorSlot2}); err == nil {
		t.Fatal("lost outcome reported success")
	}
	if engine.Fatal() == nil {
		t.Fatal("lost outcome did not fail the engine closed")
	}

	port := sqlstore.NewRoomPort(dbStore)
	resolved, err := port.ResolveHumanSend(ctx, executeAuth("u-owner"), "room-1", "x1", relay.EnterpriseSendIntent{To: "slot2", Text: "lost body"})
	if err != nil {
		t.Fatalf("read-only verification failed: %v", err)
	}
	if resolved.Outcome != relay.EnterpriseOutcomeReplayed {
		t.Fatalf("verification outcome = %q, want replayed", resolved.Outcome)
	}

	if _, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "x2", Text: "later", To: model.ActorSlot2}); err == nil {
		t.Fatal("the engine accepted a write after a lost outcome")
	}
}

// gatedPort decorates the real port with test-side callbacks around the real
// CommitHumanSend: enter runs before the SQL call, after runs after its real
// result returns — the deterministic post-commit window hook (test-only, no
// production surface).
type gatedPort struct {
	inner     relay.EnterprisePort
	entered   atomic.Int64
	after     func()
	firstOnce sync.Once
}

func (g *gatedPort) CommitHumanSend(ctx context.Context, cmd relay.EnterpriseSendCommand) (relay.EnterpriseSendResult, error) {
	g.entered.Add(1)
	result, err := g.inner.CommitHumanSend(ctx, cmd)
	if err == nil && g.after != nil {
		g.firstOnce.Do(func() { g.after() })
	}
	return result, err
}

func (g *gatedPort) ResolveHumanSend(ctx context.Context, auth relay.EnterpriseAuth, roomID, clientID string, intent relay.EnterpriseSendIntent) (relay.EnterpriseSendResult, error) {
	return g.inner.ResolveHumanSend(ctx, auth, roomID, clientID, intent)
}

func (g *gatedPort) ReadonlySnapshot(ctx context.Context, auth relay.EnterpriseAuth, roomID string) (relay.EnterpriseRoomSnapshot, error) {
	return g.inner.ReadonlySnapshot(ctx, auth, roomID)
}

// The deterministic post-commit window: while command A is held after its
// real (successful) commit returned and before its apply, command B queues on
// the gate and never enters SQL; removing the gate makes this test fail (the
// fault-injection log records that sensitivity experiment).
func TestEnterprisePostcommitWindowHoldsTheGate(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	sqltest.SeedEnterpriseRoom(t, dsn, schema, "u-owner2")
	dbStore := openSQLStore(t, dsn, schema)

	port := &gatedPort{inner: sqlstore.NewRoomPort(dbStore)}
	releaseA := make(chan struct{})
	aArrived := make(chan struct{})
	port.after = func() {
		close(aArrived)
		<-releaseA
	}
	engine, err := relay.OpenEnterprise(context.Background(), relay.Config{RoomID: "room-1"}, port, executeAuth("u-owner"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	baseSeq := engine.Snapshot().Sequence

	aDone := make(chan error, 1)
	go func() {
		_, err := engine.ExecuteHumanSend(context.Background(), executeAuth("u-owner"), relay.SendRequest{ID: "w1", Text: "A body", To: model.ActorSlot2})
		aDone <- err
	}()
	<-aArrived // A committed for real and is parked before apply; the gate is held

	bDone := make(chan error, 1)
	go func() {
		_, err := engine.ExecuteHumanSend(context.Background(), executeAuth("u-owner2"), relay.SendRequest{ID: "w2", Text: "B body", To: model.ActorSlot2})
		bDone <- err
	}()
	time.Sleep(200 * time.Millisecond) // B had ample time to queue (or to break the gate)
	if got := port.entered.Load(); got != 1 {
		t.Fatalf("B entered the SQL port during A's post-commit window: %d", got)
	}
	select {
	case <-bDone:
		t.Fatal("B completed while A was parked after commit")
	default:
	}

	close(releaseA)
	if err := <-aDone; err != nil {
		t.Fatalf("A: %v", err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("B: %v", err)
	}
	if got := port.entered.Load(); got != 2 {
		t.Fatalf("port entries after both commands: %d", got)
	}
	if snapshot := engine.Snapshot(); len(snapshot.Messages) != 2 || snapshot.Sequence != baseSeq+2 {
		t.Fatalf("snapshot: messages=%d sequence=%d", len(snapshot.Messages), snapshot.Sequence)
	}
}

// Replaying an operation whose result is older than the current cursor
// returns the canonical original message, never rewinds the cursor and never
// notifies again.
func TestEnterpriseReplayDoesNotRewindOrNotify(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	sqltest.SeedEnterpriseRoom(t, dsn, schema)

	var appends atomic.Int64
	var attentions atomic.Int64
	engine := openRoom(t, openSQLStore(t, dsn, schema), relay.Config{
		OnAppend:    func(model.Event) { appends.Add(1) },
		OnAttention: func(relay.Attention) { attentions.Add(1) },
	})
	ctx := context.Background()
	baseSeq := engine.Snapshot().Sequence

	first, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "h1", Text: "one", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "h2", Text: "two", To: model.ActorSlot2}); err != nil {
		t.Fatal(err)
	}
	appendsBefore, attentionsBefore := appends.Load(), attentions.Load()

	replayed, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "h1", Text: "one", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != first.ID {
		t.Fatalf("replay returned %q, want %q", replayed.ID, first.ID)
	}
	if appends.Load() != appendsBefore || attentions.Load() != attentionsBefore {
		t.Fatalf("replay notified: appends %d->%d attentions %d->%d", appendsBefore, appends.Load(), attentionsBefore, attentions.Load())
	}
	if snapshot := engine.Snapshot(); snapshot.Sequence != baseSeq+2 {
		t.Fatalf("replay rewound the cursor: %d", snapshot.Sequence)
	}
}

// A real stale CAS: the store advances behind the engine's back, the kernel
// refuses the stale anchor, and the one read-only rebuild + replan commits on
// the fresh revision.
func TestEnterpriseStaleCASRebuildsFromSnapshot(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	sqltest.SeedEnterpriseRoom(t, dsn, schema, "u-owner2")
	dbStore := openSQLStore(t, dsn, schema)
	engine := openRoom(t, dbStore, relay.Config{})
	ctx := context.Background()
	baseSeq := engine.Snapshot().Sequence

	if _, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "s1", Text: "one", To: model.ActorSlot2}); err != nil {
		t.Fatal(err)
	}
	// Advance the store directly (deployment-owner path), stale for the engine.
	direct := sqlstore.HumanSendCommand{
		Auth:     sqlstore.AuthContext{Deployment: "dep-test", UserID: "u-owner"},
		RoomID:   "room-1",
		ClientID: "direct-1",
		Intent:   sqlstore.HumanSendIntent{To: "slot2", Text: "direct body"},
		Fact:     craftHumanSendCandidate("u-owner", "direct-1", "slot2", "direct body"),
	}
	direct.ExpectedRevision = baseSeq + 1
	if result, err := dbStore.CommitHumanSend(ctx, direct); err != nil || result.Seq != baseSeq+2 {
		t.Fatalf("direct kernel commit: %+v %v", result, err)
	}

	message, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner2"), relay.SendRequest{ID: "stale-1", Text: "replanned", To: model.ActorSlot2})
	if err != nil {
		t.Fatalf("stale CAS was not rebuilt and replanned: %v", err)
	}
	if message.ID == "" {
		t.Fatalf("message: %+v", message)
	}
	snapshot := engine.Snapshot()
	if snapshot.Sequence != baseSeq+3 || len(snapshot.Messages) != 3 {
		t.Fatalf("after rebuild+commit: messages=%d sequence=%d", len(snapshot.Messages), snapshot.Sequence)
	}
}

// Local and SQL engines produce the same normalized human-send fact for the
// same request; the only declared differences are the generated artifacts and
// the principal-scoped client key form.
func TestEnterpriseLocalSQLFactParity(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	sqltest.SeedEnterpriseRoom(t, dsn, schema)

	// Local producer.
	dir := t.TempDir()
	jsonl, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	localEngine, err := relay.Open(relay.Config{RoomID: "room-1", Store: jsonl, Runtimes: roomRuntimes()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = localEngine.Close() })
	if _, err := localEngine.SendUser(relay.SendRequest{ID: "parity", Text: "same body", To: model.ActorSlot2}); err != nil {
		t.Fatal(err)
	}
	localFact, err := jsonl.ReadEvent(1)
	if err != nil {
		t.Fatal(err)
	}

	// SQL producer (the real kernel path through the engine).
	sqlEngine := openRoom(t, openSQLStore(t, dsn, schema), relay.Config{})
	if _, err := sqlEngine.ExecuteHumanSend(context.Background(), executeAuth("u-owner"), relay.SendRequest{ID: "parity", Text: "same body", To: model.ActorSlot2}); err != nil {
		t.Fatal(err)
	}
	admin := sqltest.Connect(t, dsn, schema)
	var encoding []byte
	if err := admin.QueryRow(context.Background(), `SELECT event_encoding FROM room_events WHERE room_id = 'room-1' AND seq = 2`).Scan(&encoding); err != nil {
		t.Fatal(err)
	}
	var sqlFact model.Event
	if err := json.Unmarshal(encoding, &sqlFact); err != nil {
		t.Fatal(err)
	}

	normalize := func(event model.Event) map[string]any {
		var payload map[string]any
		if err := json.Unmarshal(event.Data, &payload); err != nil {
			t.Fatal(err)
		}
		delete(payload, "id")
		delete(payload, "created_at")
		delete(payload, "updated_at")
		clientKey, _ := payload["client_key"].(string)
		payload["client_key"] = clientKey
		return map[string]any{
			"kind":    event.Kind,
			"actor":   string(event.Actor),
			"payload": payload,
		}
	}
	localNorm, sqlNorm := normalize(localFact), normalize(sqlFact)
	if localNorm["kind"] != sqlNorm["kind"] || localNorm["actor"] != sqlNorm["actor"] {
		t.Fatalf("envelope parity: %v vs %v", localNorm, sqlNorm)
	}
	localPayload := localNorm["payload"].(map[string]any)
	sqlPayload := sqlNorm["payload"].(map[string]any)
	if localPayload["from"] != sqlPayload["from"] || localPayload["to"] != sqlPayload["to"] ||
		localPayload["text"] != sqlPayload["text"] || localPayload["state"] != sqlPayload["state"] ||
		localPayload["source"] != sqlPayload["source"] || localPayload["target_generation"] != sqlPayload["target_generation"] {
		t.Fatalf("payload parity: %v vs %v", localPayload, sqlPayload)
	}
	if localPayload["client_key"] != "user/parity" || sqlPayload["client_key"] != "user/u-owner/parity" {
		t.Fatalf("declared client-key forms: %v vs %v", localPayload["client_key"], sqlPayload["client_key"])
	}
}

// Fixture-produced delivering history (kernel append_fact, not Binding
// integration): the enterprise constructor replays it without reaping, and
// CloseContext performs no reap write either.
func TestEnterpriseDeliveringHistoryReplayAndCloseNoReap(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedEnterpriseRoom(t, dsn, schema)
	dbStore := openSQLStore(t, dsn, schema)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Microsecond)
	payload, err := json.Marshal(map[string]any{
		"id": "relay-delivering", "from": "user", "to": "slot2", "text": "in flight",
		"state": "delivering", "target_generation": 1, "created_at": now, "updated_at": now,
		"claimed_at": now, "source": "stop", "client_key": "user/u-owner/fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := sqlstore.Command{
		Auth:     sqlstore.AuthContext{Deployment: "dep-test", UserID: "u-owner"},
		Key:      sqlstore.OpKey{Namespace: "human", Deployment: "dep-test", RoomID: "room-1", OpKind: "append_fact", Identifier: "fixture-1", Principal: "u-owner"},
		Mutation: sqlstore.Mutation{Version: 1, Fact: model.Event{ID: "evt-fixture-1", RoomID: "room-1", Kind: "native.message.updated", Actor: model.ActorUser, CreatedAt: now, Data: payload}, Audit: sqlstore.AuditRecord{Kind: "fact.append", Outcome: "accepted"}},
	}
	fixture.ExpectedRevision = 1 // room.created is the existing prefix.
	if result, err := dbStore.Commit(ctx, fixture); err != nil || result.Seq != 2 {
		t.Fatalf("fixture commit: %+v %v", result, err)
	}

	counts := func() (int64, int64, int64) {
		var events, operations, audits int64
		if err := admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM room_events), (SELECT count(*) FROM operations), (SELECT count(*) FROM audit)`).Scan(&events, &operations, &audits); err != nil {
			t.Fatal(err)
		}
		return events, operations, audits
	}
	beforeEvents, beforeOps, beforeAudits := counts()

	engine := openRoom(t, dbStore, relay.Config{})
	messages := engine.Snapshot().Messages
	if len(messages) != 1 || messages[0].State != "delivering" {
		t.Fatalf("replayed history: %+v", messages)
	}
	if events, operations, audits := counts(); events != beforeEvents || operations != beforeOps || audits != beforeAudits {
		t.Fatalf("constructor reaped or wrote: %d/%d/%d -> %d/%d/%d", beforeEvents, beforeOps, beforeAudits, events, operations, audits)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if events, operations, audits := counts(); events != beforeEvents || operations != beforeOps || audits != beforeAudits {
		t.Fatalf("CloseContext reaped or wrote: %d/%d/%d -> %d/%d/%d", beforeEvents, beforeOps, beforeAudits, events, operations, audits)
	}

	second := openRoom(t, dbStore, relay.Config{})
	if messages := second.Snapshot().Messages; len(messages) != 1 || messages[0].State != "delivering" {
		t.Fatalf("reopened history: %+v", messages)
	}
}

// conflictHookPort decorates the real port: once armed, the first revision
// conflict returned by a real commit triggers the test hook — the
// deterministic mid-command revocation point (test-only, no production
// surface).
type conflictHookPort struct {
	inner relay.EnterprisePort
	armed atomic.Bool
	hook  func()
}

func (p *conflictHookPort) CommitHumanSend(ctx context.Context, cmd relay.EnterpriseSendCommand) (relay.EnterpriseSendResult, error) {
	result, err := p.inner.CommitHumanSend(ctx, cmd)
	if err != nil && p.armed.Load() {
		var commandErr *relay.EnterpriseCommandError
		if errors.As(err, &commandErr) && commandErr.Class == relay.EnterpriseClassRevisionConflict {
			p.armed.Store(false)
			p.hook()
		}
	}
	return result, err
}

func (p *conflictHookPort) ResolveHumanSend(ctx context.Context, auth relay.EnterpriseAuth, roomID, clientID string, intent relay.EnterpriseSendIntent) (relay.EnterpriseSendResult, error) {
	return p.inner.ResolveHumanSend(ctx, auth, roomID, clientID, intent)
}

func (p *conflictHookPort) ReadonlySnapshot(ctx context.Context, auth relay.EnterpriseAuth, roomID string) (relay.EnterpriseRoomSnapshot, error) {
	return p.inner.ReadonlySnapshot(ctx, auth, roomID)
}

// A per-request authorization refusal during the conflict rebuild must not
// poison the shared Room: the revoked user is refused, the engine stays
// healthy, nothing extra is written or disclosed, and another live member
// rebuilds through its own auth and commits on the same engine.
func TestEnterpriseRebuildAuthorizationRefusalDoesNotPoisonRoom(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedEnterpriseRoom(t, dsn, schema, "u-owner2")
	dbStore := openSQLStore(t, dsn, schema)
	ctx := context.Background()

	port := &conflictHookPort{inner: sqlstore.NewRoomPort(dbStore)}
	port.hook = func() {
		if _, err := admin.Exec(ctx, `UPDATE users SET disabled = true WHERE user_id = 'u-owner2'`); err != nil {
			t.Error(err)
		}
	}
	port.armed.Store(true)
	engine, err := relay.OpenEnterprise(ctx, relay.Config{RoomID: "room-1"}, port, executeAuth("u-owner"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	baseSeq := engine.Snapshot().Sequence

	if _, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "p1", Text: "first", To: model.ActorSlot2}); err != nil {
		t.Fatal(err)
	}
	// Advance the store behind the engine so the next command meets a real
	// stale CAS, whose rebuild will run under the revoked user.
	direct := sqlstore.HumanSendCommand{
		Auth:             sqlstore.AuthContext{Deployment: "dep-test", UserID: "u-owner"},
		RoomID:           "room-1",
		ClientID:         "direct-2",
		Intent:           sqlstore.HumanSendIntent{To: "slot2", Text: "direct body"},
		ExpectedRevision: baseSeq + 1,
		Fact:             craftHumanSendCandidate("u-owner", "direct-2", "slot2", "direct body"),
	}
	if result, err := dbStore.CommitHumanSend(ctx, direct); err != nil || result.Seq != baseSeq+2 {
		t.Fatalf("direct kernel commit: %+v %v", result, err)
	}

	countRows := func() (int64, int64, int64) {
		var events, operations, audits int64
		if err := admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM room_events), (SELECT count(*) FROM operations), (SELECT count(*) FROM audit)`).Scan(&events, &operations, &audits); err != nil {
			t.Fatal(err)
		}
		return events, operations, audits
	}
	before := func() [3]int64 { e, o, a := countRows(); return [3]int64{e, o, a} }()

	_, err = engine.ExecuteHumanSend(ctx, executeAuth("u-owner2"), relay.SendRequest{ID: "p2", Text: "revoked mid command", To: model.ActorSlot2})
	var commandErr *relay.EnterpriseCommandError
	if !errors.As(err, &commandErr) || commandErr.Class != relay.EnterpriseClassAuthorization {
		t.Fatalf("revoked user's rebuild refusal: %v", err)
	}
	if engine.Fatal() != nil {
		t.Fatalf("a per-request authorization refusal poisoned the shared Room: %v", engine.Fatal())
	}
	if after := func() [3]int64 { e, o, a := countRows(); return [3]int64{e, o, a} }(); after != before {
		t.Fatalf("refusal wrote state: %v -> %v", before, after)
	}

	// A still-live member continues on the same engine: its own auth rebuilds
	// and its command commits on the fresh revision.
	message, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "p3", Text: "still alive", To: model.ActorSlot2})
	if err != nil {
		t.Fatalf("a live member could not continue after a peer's revocation: %v", err)
	}
	if message.ID == "" || engine.Snapshot().Sequence != baseSeq+3 {
		t.Fatalf("continuation: message=%+v sequence=%d", message, engine.Snapshot().Sequence)
	}
}

// Two real concurrent commands from two users share one engine gate: apply
// and notification order follow the commit order.
func TestEnterpriseDoubleConcurrencyOrderedByGate(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	sqltest.SeedEnterpriseRoom(t, dsn, schema, "u-owner2")

	var mu sync.Mutex
	var applied []uint64
	engine := openRoom(t, openSQLStore(t, dsn, schema), relay.Config{OnAppend: func(event model.Event) {
		mu.Lock()
		applied = append(applied, event.Seq)
		mu.Unlock()
	}})
	baseSeq := engine.Snapshot().Sequence

	var wg sync.WaitGroup
	errs := make([]error, 2)
	users := []string{"u-owner", "u-owner2"}
	requests := []relay.SendRequest{
		{ID: "d1", Text: "first body", To: model.ActorSlot2},
		{ID: "d2", Text: "second body", To: model.ActorSlot2},
	}
	for i, req := range requests {
		wg.Add(1)
		go func(i int, req relay.SendRequest) {
			defer wg.Done()
			_, errs[i] = engine.ExecuteHumanSend(context.Background(), executeAuth(users[i]), req)
		}(i, req)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent errors: %v %v", errs[0], errs[1])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(applied) != 2 || applied[0] != baseSeq+1 || applied[1] != baseSeq+2 {
		t.Fatalf("apply order = %v, want [2 3] after room.created", applied)
	}
	if snapshot := engine.Snapshot(); len(snapshot.Messages) != 2 || snapshot.Sequence != baseSeq+2 {
		t.Fatalf("snapshot: messages=%d sequence=%d", len(snapshot.Messages), snapshot.Sequence)
	}
}
