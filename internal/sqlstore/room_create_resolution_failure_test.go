package sqlstore

import (
	"bytes"
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
)

// cancelCreationReadTracer cancels the caller at a real driver query boundary.
// Its gate is armed only after the setup transaction is accepted, so this
// exercises interrupted verification rather than an interrupted creation.
type cancelCreationReadTracer struct {
	gate   atomic.Bool
	fired  atomic.Bool
	match  func(string) bool
	cancel context.CancelFunc
}

func (c *cancelCreationReadTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if c.gate.Load() && c.match(data.SQL) && c.fired.CompareAndSwap(false, true) {
		c.cancel()
	}
	return ctx
}

func (*cancelCreationReadTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A transport or cancellation failure reading the accepted creation is not
// evidence of corrupt bytes. Resolve must retain Unknown, or recover the
// original receipt through its existing bounded read-only fallback. The two
// query boundaries independently cover loading the fact and checking its
// configured-organization invariant after successful decoding.
func TestRoomCreateResolveInterruptedStoredReads(t *testing.T) {
	stages := []struct {
		name  string
		match func(string) bool
	}{
		{"fact", func(query string) bool {
			return strings.Contains(query, "SELECT event_id, fact_version, kind, actor, event_encoding FROM")
		}},
		{"configured-organization", func(query string) bool {
			return strings.Contains(query, "SELECT org_id FROM") && strings.Contains(query, "deployment_id = $1")
		}},
	}
	for _, stage := range stages {
		for _, mode := range []string{"replacement-recovers", "replacement-unavailable", "canceled"} {
			t.Run(stage.name+"/"+mode, func(t *testing.T) {
				f := newRawFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				killer := &killOnQueryTracer{t: t, admin: f.admin, match: stage.match}
				canceler := &cancelCreationReadTracer{match: stage.match, cancel: cancel}
				var tracer pgx.QueryTracer = killer
				if mode == "canceled" {
					tracer = canceler
				}
				f.store = openStoreWithTracer(t, f, tracer)
				killer.target.Store(f.store.work.PgConn().PID())
				cmd := roomCreateCommand(t)
				original, err := f.store.CommitRoomCreate(ctx, cmd)
				if err != nil || original.Outcome != OutcomeCommitted {
					t.Fatalf("setup creation: %+v %v", original, err)
				}
				before := creationCounts(t, f, cmd.RoomID)
				var epochBefore int64
				if err := f.admin.QueryRow(ctx, `SELECT epoch FROM ownership WHERE id`).Scan(&epochBefore); err != nil {
					t.Fatal(err)
				}
				if mode == "replacement-unavailable" {
					// Only the fallback configuration changes. The accepted
					// operation and its original work connection are untouched.
					f.store.cfg.DSN = "postgres://postgres@127.0.0.1:1/pairroom_test?sslmode=disable&connect_timeout=1"
				}
				killer.gate.Store(true)
				canceler.gate.Store(true)
				result, err := f.store.ResolveRoomCreate(ctx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
				if mode == "canceled" && !canceler.fired.Load() || mode != "canceled" && !killer.killed.Load() {
					t.Fatal("fault did not reach the selected stored-read boundary")
				}
				if mode == "replacement-recovers" {
					if err != nil || result.Outcome != OutcomeReplayed || result.Seq != original.Seq || result.Revision != original.Revision ||
						result.Fact.ID != original.Fact.ID || !result.Fact.CreatedAt.Equal(original.Fact.CreatedAt) || !bytes.Equal(result.Fact.Data, original.Fact.Data) {
						t.Fatalf("interrupted read must recover the accepted result: %+v %v", result, err)
					}
				} else {
					wantClass := ClassConnectionFailed
					if mode == "canceled" {
						wantClass = ClassCanceled
					}
					if got := kernelErr(t, result, err); got.Outcome != OutcomeUnknown || got.Class != wantClass || result.Fact.ID != "" || len(result.Fact.Data) != 0 {
						t.Fatalf("incomplete stored read claimed corruption or disclosed a fact: %+v %v", result, err)
					}
				}
				if after := creationCounts(t, f, cmd.RoomID); after != before {
					t.Fatalf("read-only recovery changed durable rows: %v -> %v", before, after)
				}
				var epochAfter int64
				if err := f.admin.QueryRow(context.Background(), `SELECT epoch FROM ownership WHERE id`).Scan(&epochAfter); err != nil {
					t.Fatal(err)
				}
				if epochAfter != epochBefore {
					t.Fatalf("read-only recovery changed ownership: %d -> %d", epochBefore, epochAfter)
				}
			})
		}
	}
}
