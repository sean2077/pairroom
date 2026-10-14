package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type writerDeadlineTrace struct {
	mu                 sync.Mutex
	deadlines          []time.Time
	started, completed []string
	commitSent         bool
}
type writerDeadlineTraceKey struct{}

func (r *writerDeadlineTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.EqualFold(strings.TrimSpace(q.SQL), "commit") {
		r.commitSent = true
	}
	if strings.HasPrefix(q.SQL, "INSERT INTO ") {
		for _, table := range []string{"room_events", "operations", "audit"} {
			if strings.Contains(q.SQL, "."+table) {
				r.started = append(r.started, table)
				deadline, _ := ctx.Deadline()
				r.deadlines = append(r.deadlines, deadline)
				return context.WithValue(ctx, writerDeadlineTraceKey{}, table)
			}
		}
	}
	return ctx
}
func (r *writerDeadlineTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryEndData) {
	if table, ok := ctx.Value(writerDeadlineTraceKey{}).(string); ok && q.Err == nil && q.CommandTag.RowsAffected() == 1 {
		r.mu.Lock()
		r.completed = append(r.completed, table)
		r.mu.Unlock()
	}
}

func writerCumulativeDeadline(t *testing.T, internal bool) {
	t.Helper()
	f := newRawFixture(t)
	trace := &writerDeadlineTrace{}
	store := openStoreWithTracer(t, f, trace)
	f.store = store
	var transactionLimit, statementLimit string
	if err := store.work.QueryRow(t.Context(), `SELECT current_setting('transaction_timeout'),current_setting('statement_timeout')`).Scan(&transactionLimit, &statementLimit); err != nil {
		t.Fatal(err)
	}
	if transactionLimit != "55s" || statementLimit != "30s" {
		t.Fatal("fixture did not begin with production session settings")
	}
	delay, parentBudget := 0.35, 900*time.Millisecond
	if internal {
		delay, parentBudget = 22, 80*time.Second
		// Explicit test-only override isolates Go's real 60s internal budget:
		// the production PostgreSQL 55s transaction guard would win otherwise.
		if _, err := store.work.Exec(t.Context(), `SELECT set_config('transaction_timeout','90s',false)`); err != nil {
			t.Fatal(err)
		}
	}
	fn := qualified(f.schema, "test_cumulative_effect_delay")
	if _, err := f.admin.Exec(t.Context(), `CREATE FUNCTION `+fn+`() RETURNS trigger LANGUAGE plpgsql AS $body$ BEGIN PERFORM pg_catalog.pg_sleep(`+fmt.Sprintf("%.2f", delay)+`); RETURN NEW; END $body$`); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"room_events", "operations", "audit"} {
		if _, err := f.admin.Exec(t.Context(), `CREATE TRIGGER test_cumulative_effect_delay BEFORE INSERT ON `+qualified(f.schema, table)+` FOR EACH ROW EXECUTE FUNCTION `+fn+`() `); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), parentBudget)
	defer cancel()
	cmd := f.command("cumulative-running-deadline")
	started := time.Now()
	result, err := store.Commit(ctx, cmd)
	elapsed := time.Since(started)
	k := kernelErr(t, result, err)
	if k.Outcome != OutcomeNotCommitted || (k.Class != ClassCanceled && k.Class != ClassStatementFailed) {
		t.Fatal("pre-COMMIT deadline was not conservatively definite rollback", k.Class)
	}
	trace.mu.Lock()
	deadlines := append([]time.Time{}, trace.deadlines...)
	phases := append([]string{}, trace.started...)
	completed := append([]string{}, trace.completed...)
	commitSent := trace.commitSent
	trace.mu.Unlock()
	if strings.Join(phases, ",") != "room_events,operations,audit" || strings.Join(completed, ",") != "room_events,operations" || commitSent {
		t.Fatalf("deadline did not occur in third effect after two successful writes: phases=%v completed=%v commit=%v", phases, completed, commitSent)
	}
	if len(deadlines) != 3 || deadlines[0].IsZero() || !deadlines[0].Equal(deadlines[1]) || !deadlines[0].Equal(deadlines[2]) {
		t.Fatal("effect statements did not share one operation deadline")
	}
	parentDeadline, _ := ctx.Deadline()
	if internal {
		if ctx.Err() != nil || elapsed < 59*time.Second || elapsed > 68*time.Second || !deadlines[0].Before(parentDeadline) {
			t.Fatal("real 60s internal budget did not beat still-live caller deadline")
		}
		t.Log("real internal 60s deadline; test-only transaction_timeout=90s; original statement_timeout=30s retained")
	} else if !errors.Is(ctx.Err(), context.DeadlineExceeded) || !deadlines[0].Equal(parentDeadline) || elapsed < 700*time.Millisecond || elapsed > 4*time.Second {
		t.Fatal("earlier caller deadline did not accumulate across the whole running mutation")
	}
	if events, ops, audits, seq, rev := f.roomCounts(t); events != 0 || ops != 0 || audits != 0 || seq != 0 || rev != 0 {
		t.Fatal("deadline left partial visible mutation state")
	}
	// Explicit original-key verification uses a fresh caller context after the
	// running transaction failed; neither cancellation nor reconnect retries it.
	resolved, err := store.Resolve(t.Context(), cmd.Auth, cmd.Key, cmd.Mutation)
	if err != nil || resolved.Outcome != OutcomeNotCommitted || resolved.Fact.ID != "" {
		t.Fatal("original Resolve did not prove full absence", err)
	}
	if events, ops, audits, seq, rev := f.roomCounts(t); events != 0 || ops != 0 || audits != 0 || seq != 0 || rev != 0 {
		t.Fatal("explicit Resolve or transport replacement resumed expired mutation")
	}
}

func TestWriterCallerDeadlineRollsBackCumulativeRunningMutation(t *testing.T) {
	writerCumulativeDeadline(t, false)
}

func TestWriterInternalBudgetRollsBackCumulativeRunningMutation(t *testing.T) {
	if os.Getenv("PAIRROOM_WRITER_INTERNAL_BUDGET_TEST") != "1" {
		t.Skip("explicit real 60s internal-budget gate is off; skip is not a pass")
	}
	writerCumulativeDeadline(t, true)
}
