package relay_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
	"github.com/sean2077/pairroom/internal/sqlstore/sqltest"
)

func roomRuntimes() map[model.ActorID]model.RuntimeKind {
	return map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex}
}

func openSQLStore(t *testing.T, dsn, schema string) *sqlstore.Store {
	t.Helper()
	store, err := sqlstore.Open(context.Background(), sqlstore.Config{DSN: dsn, Schema: schema, DeploymentID: "dep-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func openRoom(t *testing.T, store *sqlstore.Store, userID string, cfg relay.Config) *relay.Engine {
	t.Helper()
	cfg.RoomID = "room-1"
	if cfg.Runtimes == nil {
		cfg.Runtimes = roomRuntimes()
	}
	engine, err := relay.OpenEnterprise(context.Background(), cfg, sqlstore.NewRoomPort(store), relay.EnterpriseAuth{Deployment: "dep-test", UserID: userID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return engine
}

// The real-PG happy path, canonical replay, principal isolation and live
// authorization over one borrowed Store.
func TestEnterpriseHumanSendOverRealPostgres(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedRoom(t, dsn, schema, "u-owner2")
	store := openSQLStore(t, dsn, schema)
	engine := openRoom(t, store, "u-owner", relay.Config{})
	ctx := context.Background()

	message, err := engine.ExecuteHumanSend(ctx, relay.SendRequest{ID: "e1", Text: "hello", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	if message.ID == "" || message.From != model.ActorUser || message.To != model.ActorSlot2 || message.State != "queued" || message.TargetGeneration != 1 {
		t.Fatalf("message: %+v", message)
	}

	replayed, err := engine.ExecuteHumanSend(ctx, relay.SendRequest{ID: "e1", Text: "hello", To: model.ActorSlot2})
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
	if events != 1 || operations != 1 || audits != 1 {
		t.Fatalf("rows: events=%d operations=%d audits=%d", events, operations, audits)
	}

	// Same client ID, different principal: an independent fact.
	second := openRoom(t, store, "u-owner2", relay.Config{})
	other, err := second.ExecuteHumanSend(ctx, relay.SendRequest{ID: "e1", Text: "hello", To: model.ActorSlot2})
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == message.ID {
		t.Fatal("two principals shared one message identity")
	}

	// A viewer may read but not write; a disabled member is refused.
	viewer := openRoom(t, store, "u-viewer", relay.Config{})
	_, err = viewer.ExecuteHumanSend(ctx, relay.SendRequest{ID: "e2", Text: "x", To: model.ActorSlot2})
	var commandErr *relay.EnterpriseCommandError
	if !errors.As(err, &commandErr) || commandErr.Class != "authorization" {
		t.Fatalf("viewer write: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE users SET disabled = true WHERE user_id = 'u-owner2'`); err != nil {
		t.Fatal(err)
	}
	_, err = second.ExecuteHumanSend(ctx, relay.SendRequest{ID: "e3", Text: "x", To: model.ActorSlot2})
	if !errors.As(err, &commandErr) || commandErr.Class != "authorization" {
		t.Fatalf("disabled write: %v", err)
	}
}

// Closing the Room engine leaves the shared store untouched: a second engine
// over the same Store keeps committing.
func TestEnterpriseCloseLeavesTheStoreUsable(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	sqltest.SeedRoom(t, dsn, schema)
	store := openSQLStore(t, dsn, schema)
	ctx := context.Background()

	first := openRoom(t, store, "u-owner", relay.Config{})
	if _, err := first.ExecuteHumanSend(ctx, relay.SendRequest{ID: "c1", Text: "one", To: model.ActorSlot2}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.ExecuteHumanSend(ctx, relay.SendRequest{ID: "c2", Text: "after close", To: model.ActorSlot2}); err == nil {
		t.Fatal("closed engine accepted a command")
	}

	second := openRoom(t, store, "u-owner", relay.Config{})
	message, err := second.ExecuteHumanSend(ctx, relay.SendRequest{ID: "c3", Text: "two", To: model.ActorSlot2})
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
	sqltest.SeedRoom(t, dsn, schema)
	proxy := sqltest.StartProxy(t, "drop-response")
	store := openSQLStore(t, proxy.DSN(dsn), schema)
	engine := openRoom(t, store, "u-owner", relay.Config{})
	ctx := context.Background()

	proxy.Arm()
	if _, err := engine.ExecuteHumanSend(ctx, relay.SendRequest{ID: "x1", Text: "lost body", To: model.ActorSlot2}); err == nil {
		t.Fatal("lost outcome reported success")
	}
	if engine.Fatal() == nil {
		t.Fatal("lost outcome did not fail the engine closed")
	}

	// Read-only verification at the port level proves the server had accepted
	// the commit (the replacement connection performs the check).
	port := sqlstore.NewRoomPort(store)
	resolved, err := port.ResolveHumanSend(ctx, relay.EnterpriseAuth{Deployment: "dep-test", UserID: "u-owner"}, "room-1", "x1", relay.EnterpriseSendIntent{To: "slot2", Text: "lost body"})
	if err != nil {
		t.Fatalf("read-only verification failed: %v", err)
	}
	if resolved.Outcome != relay.EnterpriseOutcomeReplayed {
		t.Fatalf("verification outcome = %q, want replayed", resolved.Outcome)
	}

	// The engine stays closed for writes: read-only verification never
	// restores write capability.
	if _, err := engine.ExecuteHumanSend(ctx, relay.SendRequest{ID: "x2", Text: "later", To: model.ActorSlot2}); err == nil {
		t.Fatal("the engine accepted a write after a lost outcome")
	}
}

// Two real concurrent commands serialize on the gate: apply and notification
// order follow the commit order.
func TestEnterpriseDoubleConcurrencyOrderedByGate(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	sqltest.SeedRoom(t, dsn, schema)
	store := openSQLStore(t, dsn, schema)

	var mu sync.Mutex
	var applied []uint64
	cfg := relay.Config{OnAppend: func(event model.Event) {
		mu.Lock()
		applied = append(applied, event.Seq)
		mu.Unlock()
	}}
	engine := openRoom(t, store, "u-owner", cfg)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	requests := []relay.SendRequest{
		{ID: "d1", Text: "first body", To: model.ActorSlot2},
		{ID: "d2", Text: "second body", To: model.ActorSlot2},
	}
	for i, req := range requests {
		wg.Add(1)
		go func(i int, req relay.SendRequest) {
			defer wg.Done()
			_, errs[i] = engine.ExecuteHumanSend(context.Background(), req)
		}(i, req)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent errors: %v %v", errs[0], errs[1])
	}
	mu.Lock()
	defer mu.Unlock()
	if len(applied) != 2 || applied[0] != 1 || applied[1] != 2 {
		t.Fatalf("apply order = %v, want [1 2]", applied)
	}
	if snapshot := engine.Snapshot(); len(snapshot.Messages) != 2 || snapshot.Sequence != 2 {
		t.Fatalf("snapshot: messages=%d sequence=%d", len(snapshot.Messages), snapshot.Sequence)
	}
}
