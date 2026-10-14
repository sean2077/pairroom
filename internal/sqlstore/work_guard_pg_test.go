package sqlstore

import (
	"context"
	"github.com/jackc/pgx/v5"
	"sync"
	"testing"
)

func TestClosedWorkGuardRejectsDeadHolderBeforeInstallingCandidate(t *testing.T) {
	f := newFixture(t)
	oldPID := cancelBlockedWorkRead(t, f)
	oldWork := f.store.work
	f.terminate(t, f.store.lease.pid, "lease")
	res, err := f.store.CommitHumanSend(t.Context(), humanSendCommand("u-owner", "guard-dead-holder", "slot2", "must refuse"))
	if k := kernelErr(t, res, err); k.Outcome != OutcomeRefused || k.Class != ClassLeaseLost {
		t.Fatal(k)
	}
	if f.store.work != oldWork || f.store.work.PgConn().PID() != oldPID || !f.store.work.IsClosed() {
		t.Fatal("dead holder installed a replacement work connection")
	}
	if f.store.poisoned == nil {
		t.Fatal("dead holder was not permanently retired")
	}
}

func TestClosedWorkGuardRestorePendingBeforeInstallingCandidate(t *testing.T) {
	f := newFixture(t)
	cancelBlockedWorkRead(t, f)
	oldWork := f.store.work
	if _, err := f.admin.Exec(t.Context(), `INSERT INTO restore_runs(restore_id,manifest_hash,restore_epoch,started_at) VALUES('restore-guard',decode(repeat('00',32),'hex'),'fresh-guard',pg_catalog.clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	res, err := f.store.CommitHumanSend(t.Context(), humanSendCommand("u-owner", "guard-restore-pending", "slot2", "must refuse"))
	if k := kernelErr(t, res, err); k.Outcome != OutcomeRefused || k.Class != ClassRestorePending {
		t.Fatal(k)
	}
	if f.store.work != oldWork || !f.store.work.IsClosed() {
		t.Fatal("pending restore installed ordinary work transport")
	}
	if events, ops, _, _, _ := f.roomCounts(t); events != 0 || ops != 0 {
		t.Fatal("pending restore wrote ordinary effect")
	}
}

func TestClosedWorkReplacementConfiguresEveryServerBudget(t *testing.T) {
	f := newFixture(t)
	cancelBlockedWorkRead(t, f)
	res, err := f.store.CommitHumanSend(t.Context(), humanSendCommand("u-owner", "guard-settings", "slot2", "explicit command"))
	if err != nil || res.Outcome != OutcomeCommitted {
		t.Fatal(res.Outcome, err)
	}
	expected := map[string]string{"search_path": f.schema + ", pg_catalog", "idle_in_transaction_session_timeout": "15000", "transaction_timeout": "55000", "lock_timeout": "10000", "statement_timeout": "30000"}
	rows, err := f.store.work.Query(t.Context(), `SELECT name,setting FROM pg_catalog.pg_settings WHERE name=ANY($1)`, []string{"search_path", "idle_in_transaction_session_timeout", "transaction_timeout", "lock_timeout", "statement_timeout"})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			t.Fatal(err)
		}
		if expected[name] != value {
			t.Fatalf("replacement setting %s=%s", name, value)
		}
		delete(expected, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(expected) != 0 {
		t.Fatal("replacement configuration omitted server settings")
	}
}

func TestClosedWorkReplacementIsSerializedAcrossNewCommands(t *testing.T) {
	f := newFixture(t)
	oldPID := cancelBlockedWorkRead(t, f)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var results [2]Result
	var failures [2]error
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], failures[i] = f.store.CommitHumanSend(t.Context(), humanSendCommand("u-owner", []string{"guard-a", "guard-b"}[i], "slot2", "explicit racing requests"))
		}(i)
	}
	close(start)
	wg.Wait()
	wins := 0
	for i, r := range results {
		if failures[i] == nil && r.Outcome == OutcomeCommitted {
			wins++
		} else if k := kernelErr(t, r, failures[i]); k.Outcome != OutcomeRefused || k.Class != ClassRevisionConflict {
			t.Fatal(k)
		}
	}
	if wins != 1 || f.store.work.PgConn().PID() == oldPID {
		t.Fatal("serialized replacement did not retain strict command CAS")
	}
	if events, ops, _, _, _ := f.roomCounts(t); events != 1 || ops != 1 {
		t.Fatal("replacement repeated a concurrent effect")
	}
}

func TestCanceledWriteNextExplicitRequestReplacesTransport(t *testing.T) {
	f := newFixture(t)
	blocker := schemaConn(t, f.dsn, f.schema)
	tx, err := blocker.BeginTx(t.Context(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { ctx, cancel := cleanupCtx(); defer cancel(); _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(t.Context(), `SELECT room_id FROM rooms WHERE room_id='room-1' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	cmd := humanSendCommand("u-owner", "guard-canceled-write", "slot2", "explicit original write")
	pid := f.store.work.PgConn().PID()
	go func() {
		r, e := f.store.CommitHumanSend(ctx, cmd)
		if r.Outcome != OutcomeNotCommitted {
			t.Error("canceled pre-COMMIT request was not definite")
		}
		done <- e
	}()
	waitForLockWait(t, f.admin, pid)
	cancel()
	if err := <-done; err == nil {
		t.Fatal("blocked canceled write succeeded")
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !f.store.work.IsClosed() {
		t.Fatal("actual write cancellation did not close transport")
	}
	if events, ops, _, _, _ := f.roomCounts(t); events != 0 || ops != 0 {
		t.Fatal("canceled write left effects")
	}
	r, e := f.store.CommitHumanSend(t.Context(), cmd)
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatal(r.Outcome, e)
	}
	if events, ops, _, _, _ := f.roomCounts(t); events != 1 || ops != 1 {
		t.Fatal("explicit recovery duplicated canceled write")
	}
}
