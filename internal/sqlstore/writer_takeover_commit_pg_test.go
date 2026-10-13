package sqlstore

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type passedFenceWriterTrace struct {
	mu                       sync.Mutex
	fenced, event, operation bool
	auditStarted             chan struct{}
	once                     sync.Once
}
type passedFenceTraceKey struct{}

func (r *passedFenceWriterTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(q.SQL, "INSERT INTO ") && strings.Contains(q.SQL, ".audit") {
		r.once.Do(func() { close(r.auditStarted) })
	}
	return context.WithValue(ctx, passedFenceTraceKey{}, q.SQL)
}
func (r *passedFenceWriterTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(passedFenceTraceKey{}).(string)
	if q.Err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.Contains(sql, "pg_catalog.pg_locks") && strings.Contains(sql, "a.backend_start") {
		r.fenced = true
	}
	if strings.HasPrefix(sql, "INSERT INTO ") && q.CommandTag.RowsAffected() == 1 {
		if strings.Contains(sql, ".room_events") {
			r.event = true
		}
		if strings.Contains(sql, ".operations") {
			r.operation = true
		}
	}
}

func TestWriterPassedFenceCommitsBeforeSuccessorOwnershipAdvance(t *testing.T) {
	f := newRawFixture(t)
	trace := &passedFenceWriterTrace{auditStarted: make(chan struct{})}
	old := openStoreWithTracer(t, f, trace)
	f.store = old
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	blocker := schemaConn(t, f.dsn, f.schema)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { c, cancel := cleanupCtx(); defer cancel(); _ = tx.Rollback(c) }()
	if _, err = tx.Exec(ctx, `LOCK TABLE `+qualified(f.schema, "audit")+` IN SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	cmd := f.command("passed-fence-normal-commit")
	type committed struct {
		result Result
		err    error
	}
	writerDone := make(chan committed, 1)
	go func() { r, e := old.Commit(ctx, cmd); writerDone <- committed{r, e} }()
	select {
	case <-trace.auditStarted:
	case <-ctx.Done():
		t.Fatal("writer never reached downstream audit INSERT")
	}
	// This is an actual database statement lock wait after the fence and two
	// successful effect statements, not a fabricated paused transaction.
	waitIdentityControlGuardChain(t, f, []uint32{old.work.PgConn().PID()}, blocker.PgConn().PID())
	trace.mu.Lock()
	passed, wroteEvent, wroteOperation := trace.fenced, trace.event, trace.operation
	trace.mu.Unlock()
	if !passed || !wroteEvent || !wroteOperation {
		t.Fatal("normal writer did not pass fence and stage its fact/operation")
	}
	if events, ops, audits, seq, rev := f.roomCounts(t); events != 0 || ops != 0 || audits != 0 || seq != 0 || rev != 0 {
		t.Fatal("uncommitted writer released partial state")
	}
	f.terminate(t, old.lease.pid, "passed-fence old lease")
	type opened struct {
		store *Store
		err   error
	}
	successorDone := make(chan opened, 1)
	go func() { s, e := Open(ctx, f.cfg); successorDone <- opened{s, e} }()
	var successorPID int32
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for successorPID == 0 {
		err = f.admin.QueryRow(ctx, `SELECT a.pid FROM pg_catalog.pg_stat_activity a JOIN pg_catalog.pg_locks l ON l.pid=a.pid
 WHERE l.locktype='advisory' AND l.granted AND l.mode='ExclusiveLock' AND l.objsubid=1
 AND l.database=(SELECT oid FROM pg_catalog.pg_database WHERE datname=pg_catalog.current_database())
 AND l.classid::bigint=$1 AND l.objid::bigint=$2
 AND a.state='active' AND a.wait_event_type='Lock' AND a.query LIKE '%ownership%FOR UPDATE%'
 AND $3=ANY(pg_catalog.pg_blocking_pids(a.pid))`, int64(old.lease.classid), int64(old.lease.objid), int32(old.work.PgConn().PID())).Scan(&successorPID)
		if err != nil && err != pgx.ErrNoRows {
			t.Fatal("successor lock-state query failed", err)
		}
		if successorPID == 0 {
			select {
			case <-ctx.Done():
				t.Fatal("successor did not acquire advisory and wait on writer ownership guard")
			case <-ticker.C:
			}
		}
	}
	var epoch int64
	if err = f.admin.QueryRow(ctx, `SELECT epoch FROM ownership WHERE id`).Scan(&epoch); err != nil || epoch != old.lease.epoch {
		t.Fatal("successor advanced epoch while writer held guard", err)
	}
	select {
	case s := <-successorDone:
		if s.store != nil {
			_ = s.store.Close()
		}
		t.Fatal("Open finished before guarded writer COMMIT", s.err)
	default:
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	writer := <-writerDone
	successor := <-successorDone
	if successor.store != nil {
		defer successor.store.Close()
	}
	if writer.err != nil || writer.result.Outcome != OutcomeCommitted || writer.result.Seq != 1 || writer.result.Revision != 1 {
		t.Fatal("passed-fence writer did not normally COMMIT", writer.err)
	}
	if successor.err != nil || successor.store == nil || successor.store.lease.pid != uint32(successorPID) || successor.store.lease.epoch != old.lease.epoch+1 {
		t.Fatal("successor did not advance only after writer completion", successor.err)
	}
	if events, ops, audits, seq, rev := f.roomCounts(t); events != 1 || ops != 1 || audits != 1 || seq != 1 || rev != 1 {
		t.Fatal("writer did not atomically commit exactly one complete effect")
	}
	recovered, err := successor.store.Resolve(ctx, cmd.Auth, cmd.Key, cmd.Mutation)
	if err != nil || recovered.Outcome != OutcomeReplayed || recovered.Fact.ID != cmd.Mutation.Fact.ID || recovered.Seq != 1 || recovered.Revision != 1 {
		t.Fatal("successor did not recover normal committed original identity", err)
	}
	stale := f.command("old-store-after-takeover")
	stale.ExpectedRevision = 1
	r, err := old.Commit(ctx, stale)
	if k := kernelErr(t, r, err); k.Outcome != OutcomeRefused || k.Class != ClassLeaseLost {
		t.Fatal("old incarnation wrote after successor takeover")
	}
	if events, ops, audits, seq, rev := f.roomCounts(t); events != 1 || ops != 1 || audits != 1 || seq != 1 || rev != 1 {
		t.Fatal("stale writer changed accepted complete effect")
	}
}
