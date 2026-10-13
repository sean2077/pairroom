package sqlstore

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/model"
)

// These counts cover every table a creation may modify. The pre-existing
// primitive fixture Room is deliberately excluded from the assertions.
func creationCounts(t *testing.T, f *fixture, roomID string) [7]int64 {
	t.Helper()
	var got [7]int64
	err := f.admin.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM rooms WHERE room_id = $1),
		(SELECT count(*) FROM room_members WHERE room_id = $1),
		(SELECT count(*) FROM slot_generations WHERE room_id = $1),
		(SELECT count(*) FROM room_slots WHERE room_id = $1),
		(SELECT count(*) FROM room_events WHERE room_id = $1),
		(SELECT count(*) FROM operations WHERE room_id = $1),
		(SELECT count(*) FROM audit WHERE room_id = $1)`, roomID).
		Scan(&got[0], &got[1], &got[2], &got[3], &got[4], &got[5], &got[6])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRoomCreateAtomicInitialStateAndOriginalReplay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cmd := roomCreateCommand(t)
	first, err := f.store.CommitRoomCreate(ctx, cmd)
	if err != nil || first.Outcome != OutcomeCommitted || first.Seq != 1 || first.Revision != 1 || first.Fact.Kind != "room.created" || first.Fact.Actor != model.ActorSystem || first.Fact.RoomID != cmd.RoomID {
		t.Fatalf("create: %+v %v", first, err)
	}
	if got := creationCounts(t, f, cmd.RoomID); got != [7]int64{1, 1, 2, 2, 1, 1, 1} {
		t.Fatalf("creation is incomplete: %v", got)
	}
	var org, owner, role, hostMode, origin, opKind, auditKind string
	var seq, revision, liveHeads, bindings int64
	var created time.Time
	err = f.admin.QueryRow(ctx, `SELECT r.org_id,r.owner_user_id,m.role,r.host_mode,r.current_seq,r.revision,r.created_at,
		(SELECT count(*) FROM room_slots WHERE room_id = r.room_id AND current_generation = 1),
		(SELECT count(*) FROM bindings WHERE room_id = r.room_id),
		e.origin_user,o.op_kind,a.kind
		FROM rooms r JOIN room_members m ON m.room_id = r.room_id
		JOIN room_events e ON e.room_id = r.room_id
		JOIN operations o ON o.room_id = r.room_id
		JOIN audit a ON a.room_id = r.room_id WHERE r.room_id = $1`, cmd.RoomID).
		Scan(&org, &owner, &role, &hostMode, &seq, &revision, &created, &liveHeads, &bindings, &origin, &opKind, &auditKind)
	if err != nil {
		t.Fatal(err)
	}
	if org != "org-test" || owner != "u-owner" || role != "owner" || hostMode != "native" || seq != 1 || revision != 1 || liveHeads != 2 || bindings != 0 || origin != "u-owner" || opKind != "room_create" || auditKind != "room.create" {
		t.Fatalf("initial metadata org=%q owner=%q role=%q host=%q seq=%d rev=%d heads=%d bindings=%d origin=%q operation=%q audit=%q", org, owner, role, hostMode, seq, revision, liveHeads, bindings, origin, opKind, auditKind)
	}
	if !created.Equal(first.Fact.CreatedAt.Round(time.Microsecond)) && !created.Equal(first.Fact.CreatedAt.Truncate(time.Microsecond)) {
		t.Fatalf("row/event creation time mismatch: %s / %s", created, first.Fact.CreatedAt)
	}
	creation, err := model.DecodeEnterpriseRoomCreation(first.Fact)
	if err != nil {
		t.Fatal(err)
	}
	if creation.Name != cmd.Intent.Name || creation.Collaboration == nil || *creation.Collaboration != cmd.Intent.Collaboration || len(creation.Enterprise.InitialSlots) != 2 || creation.Enterprise.InitialSlots[0].State != "vacant" || creation.Enterprise.InitialSlots[1].State != "vacant" {
		t.Fatalf("creation does not preserve name/policy/vacancy: %+v", creation)
	}
	// Later accepted work must not change a creation replay's result cursor.
	later := f.command("after-create")
	later.Key.RoomID, later.Mutation.Fact.RoomID, later.ExpectedRevision = cmd.RoomID, cmd.RoomID, 1
	if res, err := f.store.Commit(ctx, later); err != nil || res.Seq != 2 {
		t.Fatalf("later event: %+v %v", res, err)
	}
	before := creationCounts(t, f, cmd.RoomID)
	for _, resolve := range []bool{false, true} {
		var got Result
		if resolve {
			got, err = f.store.ResolveRoomCreate(ctx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
		} else {
			got, err = f.store.CommitRoomCreate(ctx, cmd)
		}
		if err != nil || got.Outcome != OutcomeReplayed || got.Seq != 1 || got.Revision != 1 || got.Fact.ID != first.Fact.ID || !got.Fact.CreatedAt.Equal(first.Fact.CreatedAt) || !bytes.Equal(got.Fact.Data, first.Fact.Data) {
			t.Fatalf("original replay resolve=%v: %+v %v", resolve, got, err)
		}
	}
	if got := creationCounts(t, f, cmd.RoomID); got != before {
		t.Fatalf("replay wrote rows: %v -> %v", before, got)
	}
}

func TestRoomCreateChangedIntentAndTakenRoomNeverTakeOver(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cmd := roomCreateCommand(t)
	if _, err := f.store.CommitRoomCreate(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	before := creationCounts(t, f, cmd.RoomID)
	changedName := cmd
	changedName.Intent.Name = "other name"
	changedPolicy := cmd
	changedPolicy.Intent.Collaboration = model.Collaboration{Version: 2, Mode: "custom", Instructions: "other policy"}
	for _, changed := range []RoomCreateCommand{changedName, changedPolicy} {
		res, err := f.store.CommitRoomCreate(ctx, changed)
		if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeConflict || kerr.Class != ClassPayloadConflict {
			t.Fatalf("changed intent: %+v", res)
		}
		res, err = f.store.ResolveRoomCreate(ctx, changed.Auth, changed.RoomID, changed.ClientID, changed.Intent)
		if kerr := kernelErr(t, res, err); kerr.Class != ClassPayloadConflict {
			t.Fatalf("changed resolve: %+v", res)
		}
	}
	for _, auth := range []AuthContext{cmd.Auth, {Deployment: "dep-test", UserID: "u-viewer"}} {
		other := cmd
		other.Auth, other.ClientID = auth, "different-key"
		res, err := f.store.CommitRoomCreate(ctx, other)
		if kerr := kernelErr(t, res, err); kerr.Class != ClassRoomTaken {
			t.Fatalf("taken room: %+v", res)
		}
		if res.Fact.ID != "" {
			t.Fatal("taken Room disclosed another operation's original fact")
		}
		res, err = f.store.ResolveRoomCreate(ctx, other.Auth, other.RoomID, other.ClientID, other.Intent)
		if err != nil || res.Outcome != OutcomeNotCommitted || res.Seq != 0 || res.Revision != 0 || !reflect.DeepEqual(res.Fact, model.Event{}) {
			t.Fatalf("absent-key resolve disclosed unrelated Room state: %+v %v", res, err)
		}
	}
	if got := creationCounts(t, f, cmd.RoomID); got != before {
		t.Fatalf("refusals modified creation: %v -> %v", before, got)
	}
}

// Resolving an absent exact creation key reports only that operation's
// absence. Even a valid Room ID reveals no result metadata to a nonmember.
func TestRoomCreateResolveAbsentKeyHidesTakenRoom(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cmd := roomCreateCommand(t)
	if _, err := f.store.CommitRoomCreate(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	// Make the unrelated Room revision nonzero and distinguish it from its
	// birth result; neither cursor may be copied into an absent-key result.
	later := f.command("after-create-absence-proof")
	later.Key.RoomID, later.Mutation.Fact.RoomID, later.ExpectedRevision = cmd.RoomID, cmd.RoomID, 1
	if result, err := f.store.Commit(ctx, later); err != nil || result.Revision != 2 {
		t.Fatalf("advance existing Room: %+v %v", result, err)
	}
	var viewerMemberships int
	if err := f.admin.QueryRow(ctx, `SELECT count(*) FROM room_members WHERE room_id = $1 AND user_id = 'u-viewer'`, cmd.RoomID).Scan(&viewerMemberships); err != nil {
		t.Fatal(err)
	}
	if viewerMemberships != 0 {
		t.Fatal("nonmember counterexample unexpectedly has Room access")
	}
	before := creationCounts(t, f, cmd.RoomID)
	for _, userID := range []string{"u-owner", "u-viewer"} {
		auth := AuthContext{Deployment: cmd.Auth.Deployment, UserID: userID}
		resolved, err := f.store.ResolveRoomCreate(ctx, auth, cmd.RoomID, "absent-creation-key", cmd.Intent)
		if err != nil || resolved.Outcome != OutcomeNotCommitted || resolved.Class != "" || resolved.Seq != 0 || resolved.Revision != 0 || !reflect.DeepEqual(resolved.Fact, model.Event{}) {
			t.Fatalf("absent key for %s disclosed unrelated Room metadata: %+v %v", userID, resolved, err)
		}
		if got := creationCounts(t, f, cmd.RoomID); got != before {
			t.Fatalf("absent-key resolve mutated state for %s: %v -> %v", userID, before, got)
		}
		// Submitting that new key is a different question: the occupied ID
		// is definitely unavailable, and still discloses no original fact.
		other := cmd
		other.Auth, other.ClientID = auth, "absent-creation-key"
		result, err := f.store.CommitRoomCreate(ctx, other)
		if got := kernelErr(t, result, err); got.Outcome != OutcomeRefused || got.Class != ClassRoomTaken || result.Seq != 0 || result.Revision != 0 || !reflect.DeepEqual(result.Fact, model.Event{}) {
			t.Fatalf("new-key commit for %s: %+v", userID, result)
		}
		if got := creationCounts(t, f, cmd.RoomID); got != before {
			t.Fatalf("taken-room commit mutated state for %s: %v -> %v", userID, before, got)
		}
	}
}

func TestRoomCreateRequiresLiveUserAndCurrentReplayACL(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cmd := roomCreateCommand(t)
	if _, err := f.admin.Exec(ctx, `UPDATE users SET disabled = true WHERE user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	res, err := f.store.CommitRoomCreate(ctx, cmd)
	if kerr := kernelErr(t, res, err); kerr.Class != ClassAuthorization {
		t.Fatalf("disabled create: %+v", res)
	}
	if got := creationCounts(t, f, cmd.RoomID); got != [7]int64{} {
		t.Fatalf("disabled user created state: %v", got)
	}
	if _, err := f.admin.Exec(ctx, `UPDATE users SET disabled = false WHERE user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CommitRoomCreate(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	for _, revoke := range []string{
		`UPDATE users SET disabled = true WHERE user_id = 'u-owner'`,
		`UPDATE users SET disabled = false WHERE user_id = 'u-owner'; DELETE FROM room_members WHERE room_id = 'room-created' AND user_id = 'u-owner'`,
	} {
		if _, err := f.admin.Exec(ctx, revoke); err != nil {
			t.Fatal(err)
		}
		before := creationCounts(t, f, cmd.RoomID)
		res, err := f.store.CommitRoomCreate(ctx, cmd)
		if kerr := kernelErr(t, res, err); kerr.Class != ClassAuthorization || res.Fact.ID != "" {
			t.Fatalf("revoked replay disclosed result: %+v", res)
		}
		res, err = f.store.ResolveRoomCreate(ctx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
		if kerr := kernelErr(t, res, err); kerr.Class != ClassAuthorization || res.Fact.ID != "" {
			t.Fatalf("revoked resolve disclosed result: %+v", res)
		}
		if got := creationCounts(t, f, cmd.RoomID); got != before {
			t.Fatalf("revoked operation wrote rows: %v -> %v", before, got)
		}
	}
}

func TestRoomCreateAuditFailureRollsBackEveryInitialRow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.admin.Exec(ctx, `ALTER TABLE audit ADD CONSTRAINT test_refuse_room_create CHECK (kind <> 'room.create')`); err != nil {
		t.Fatal(err)
	}
	cmd := roomCreateCommand(t)
	res, err := f.store.CommitRoomCreate(ctx, cmd)
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeNotCommitted || kerr.Class != ClassConstraintViolation {
		t.Fatalf("audit failure: %+v", res)
	}
	if got := creationCounts(t, f, cmd.RoomID); got != [7]int64{} {
		t.Fatalf("audit failure left partial creation: %v", got)
	}
	resolved, err := f.store.ResolveRoomCreate(ctx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
	if err != nil || resolved.Outcome != OutcomeNotCommitted {
		t.Fatalf("rolled-back creation resolve: %+v %v", resolved, err)
	}
}

func TestRoomCreateConcurrentKeysHaveOneWinner(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cmd := roomCreateCommand(t)
	commands := [2]RoomCreateCommand{cmd, cmd}
	commands[1].Auth.UserID, commands[1].ClientID = "u-viewer", "other-create"
	var wg sync.WaitGroup
	var results [2]Result
	var errs [2]error
	start := make(chan struct{})
	for i := range commands {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.store.CommitRoomCreate(ctx, commands[i])
		}(i)
	}
	close(start)
	wg.Wait()
	winners := 0
	for i, res := range results {
		if res.Outcome == OutcomeCommitted && errs[i] == nil {
			winners++
		} else if kerr := kernelErr(t, res, errs[i]); kerr.Class != ClassRoomTaken {
			t.Fatalf("loser: %+v", res)
		}
	}
	if winners != 1 {
		t.Fatalf("winners=%d results=%+v", winners, results)
	}
	if got := creationCounts(t, f, cmd.RoomID); got != [7]int64{1, 1, 2, 2, 1, 1, 1} {
		t.Fatalf("concurrent create duplicated rows: %v", got)
	}
}

func TestRoomCreateUnknownCommitBothRealProxyOutcomes(t *testing.T) {
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			proxy := startProxy(t, mode)
			f := newFixtureProxy(t, proxy.dsn)
			cmd := roomCreateCommand(t)
			proxy.arm()
			res, err := f.store.CommitRoomCreate(context.Background(), cmd)
			if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeUnknown {
				t.Fatalf("unknown creation: %+v", res)
			}
			before := creationCounts(t, f, cmd.RoomID)
			// Verification is explicitly read-only and never submits a create.
			resolved, err := f.store.ResolveRoomCreate(context.Background(), cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "drop-response" {
				if resolved.Outcome != OutcomeReplayed || resolved.Seq != 1 || resolved.Revision != 1 || resolved.Fact.ID == "" || before != [7]int64{1, 1, 2, 2, 1, 1, 1} {
					t.Fatalf("accepted creation: %+v counts=%v", resolved, before)
				}
			} else if resolved.Outcome != OutcomeNotCommitted || resolved.Fact.ID != "" || before != [7]int64{} {
				t.Fatalf("absent creation: %+v counts=%v", resolved, before)
			}
			if got := creationCounts(t, f, cmd.RoomID); got != before {
				t.Fatalf("verification mutated state: %v -> %v", before, got)
			}

			// An explicit same-key retry happens only after the old Store is
			// retired and a new incarnation opens. Stop fault injection before
			// that Open's ownership transaction; reuse the exact Store config.
			oldStore := f.store
			var oldInstance string
			var oldEpoch int64
			if err := f.admin.QueryRow(context.Background(), `SELECT instance_id, epoch FROM ownership WHERE id`).Scan(&oldInstance, &oldEpoch); err != nil {
				t.Fatal(err)
			}
			if err := oldStore.Close(); err != nil {
				t.Fatal(err)
			}
			proxy.armed.Store(false)
			reopened, err := Open(context.Background(), oldStore.cfg)
			if err != nil {
				t.Fatal(err)
			}
			// This cleanup closes only the new incarnation; the fixture still
			// owns its original Store, admin connection and unique test schema.
			t.Cleanup(func() { _ = reopened.Close() })
			var newInstance string
			var newEpoch int64
			if err := f.admin.QueryRow(context.Background(), `SELECT instance_id, epoch FROM ownership WHERE id`).Scan(&newInstance, &newEpoch); err != nil {
				t.Fatal(err)
			}
			if newInstance == oldInstance || newEpoch != oldEpoch+1 || reopened.lease.epoch != newEpoch || reopened.lease.pid == oldStore.lease.pid && reopened.lease.start.Equal(oldStore.lease.start) {
				t.Fatalf("restart reused its lease identity: epoch %d -> %d, instance changed=%v", oldEpoch, newEpoch, newInstance != oldInstance)
			}
			if got := creationCounts(t, f, cmd.RoomID); got != before {
				t.Fatalf("Store restart retried an effect: %v -> %v", before, got)
			}
			retried, err := reopened.CommitRoomCreate(context.Background(), cmd)
			want := OutcomeCommitted
			if mode == "drop-response" {
				want = OutcomeReplayed
			}
			if err != nil || retried.Outcome != want || retried.Seq != 1 || retried.Revision != 1 || retried.Fact.Seq != 1 || retried.Fact.ID == "" {
				t.Fatalf("explicit same-key retry after restart: %+v %v", retried, err)
			}
			if mode == "drop-response" && (retried.Fact.ID != resolved.Fact.ID || !retried.Fact.CreatedAt.Equal(resolved.Fact.CreatedAt) || !bytes.Equal(retried.Fact.Data, resolved.Fact.Data)) {
				t.Fatal("explicit retry replaced an already accepted creation")
			}
			replayed, err := reopened.CommitRoomCreate(context.Background(), cmd)
			if err != nil || replayed.Outcome != OutcomeReplayed || replayed.Seq != 1 || replayed.Revision != 1 || replayed.Fact.ID != retried.Fact.ID || !replayed.Fact.CreatedAt.Equal(retried.Fact.CreatedAt) || !bytes.Equal(replayed.Fact.Data, retried.Fact.Data) {
				t.Fatalf("repeated explicit same-key retry: %+v %v", replayed, err)
			}
			if got := creationCounts(t, f, cmd.RoomID); got != [7]int64{1, 1, 2, 2, 1, 1, 1} {
				t.Fatalf("explicit retries duplicated the Room birth: %v", got)
			}
			var seq, revision int64
			if err := f.admin.QueryRow(context.Background(), `SELECT current_seq, revision FROM rooms WHERE room_id = $1`, cmd.RoomID).Scan(&seq, &revision); err != nil {
				t.Fatal(err)
			}
			if seq != 1 || revision != 1 {
				t.Fatalf("explicit retries moved Room cursors: seq=%d revision=%d", seq, revision)
			}
		})
	}
}

// A missing Room has no row to lock. The ownership row is the mandatory
// serialization boundary even for that absence, on both creation and resolve.
func TestRoomCreateAbsentRoomWaitsForOwnershipGuard(t *testing.T) {
	for _, resolve := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "resolve"}[resolve], func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			blocker := schemaConn(t, f.dsn, f.schema)
			tx, err := blocker.BeginTx(ctx, pgx.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			// Existing appends take FOR SHARE. A create/resolve must take the
			// exclusive guard and therefore cannot pass even a shared owner lock.
			if _, err := tx.Exec(ctx, `SELECT epoch FROM ownership WHERE id FOR SHARE`); err != nil {
				t.Fatal(err)
			}
			cmd := roomCreateCommand(t)
			type reply struct {
				result Result
				err    error
			}
			done := make(chan reply, 1)
			go func() {
				var result Result
				var err error
				if resolve {
					result, err = f.store.ResolveRoomCreate(ctx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
				} else {
					result, err = f.store.CommitRoomCreate(ctx, cmd)
				}
				done <- reply{result, err}
			}()
			waitForLockWait(t, f.admin, f.store.work.PgConn().PID())
			select {
			case early := <-done:
				t.Fatalf("passed unsettled ownership: %+v", early)
			default:
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			got := <-done
			want := OutcomeCommitted
			if resolve {
				want = OutcomeNotCommitted
			}
			if got.err != nil || got.result.Outcome != want {
				t.Fatalf("settled guard: %+v %v", got.result, got.err)
			}
		})
	}
}

func TestRoomCreateResolveGuardTimeoutIsUnknownNeverAbsence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	blocker := schemaConn(t, f.dsn, f.schema)
	tx, err := blocker.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT epoch FROM ownership WHERE id FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	cmd := roomCreateCommand(t)
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	type reply struct {
		result Result
		err    error
	}
	done := make(chan reply, 1)
	go func() {
		result, err := f.store.ResolveRoomCreate(checkCtx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
		done <- reply{result, err}
	}()
	waitForLockWait(t, f.admin, f.store.work.PgConn().PID())
	got := <-done
	if kerr := kernelErr(t, got.result, got.err); kerr.Outcome != OutcomeUnknown {
		t.Fatalf("interrupted verification claimed absence: %+v", got.result)
	}
	if counts := creationCounts(t, f, cmd.RoomID); counts != [7]int64{} {
		t.Fatalf("interrupted verification wrote: %v", counts)
	}
}

// A driver trace parks the actual creation transaction at COMMIT, after all
// its rows have been inserted. An independent connection models verification
// after a lost work connection; it shares the already-admitted lease identity
// and cannot be serialized merely by the Store's in-process connection gate.
// This is a test-only composition, not an additional deployment lease holder.
type creationCommitBarrier struct {
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *creationCommitBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.EqualFold(strings.TrimSpace(data.SQL), "commit") {
		b.once.Do(func() {
			close(b.arrived)
			select {
			case <-b.release:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}

func (*creationCommitBarrier) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRoomCreatePendingTransactionSettlesBeforeResolveOrCompetitor(t *testing.T) {
	for _, resolve := range []bool{true, false} {
		t.Run(map[bool]string{true: "resolve", false: "competing-key"}[resolve], func(t *testing.T) {
			f := newRawFixture(t)
			ctx := context.Background()
			barrier := &creationCommitBarrier{arrived: make(chan struct{}), release: make(chan struct{})}
			f.store = openStoreWithTracer(t, f, barrier)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
			t.Cleanup(release)
			work := schemaConn(t, f.dsn, f.schema)
			if err := configureSession(ctx, work, f.schema); err != nil {
				t.Fatal(err)
			}
			observer := &Store{cfg: f.cfg, lease: f.store.lease, work: work, sem: make(chan struct{}, 1)}
			cmd := roomCreateCommand(t)
			type reply struct {
				result Result
				err    error
			}
			created := make(chan reply, 1)
			go func() { result, err := f.store.CommitRoomCreate(ctx, cmd); created <- reply{result, err} }()
			select {
			case <-barrier.arrived:
			case early := <-created:
				t.Fatalf("creation did not reach COMMIT: %+v %v", early.result, early.err)
			case <-time.After(10 * time.Second):
				t.Fatal("creation did not reach COMMIT")
			}
			if got := creationCounts(t, f, cmd.RoomID); got != [7]int64{} {
				t.Fatalf("uncommitted creation became visible: %v", got)
			}
			checked := make(chan reply, 1)
			go func() {
				var result Result
				var err error
				if resolve {
					result, err = observer.ResolveRoomCreate(ctx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
				} else {
					other := cmd
					other.ClientID, other.Auth.UserID = "concurrent-key", "u-viewer"
					result, err = observer.CommitRoomCreate(ctx, other)
				}
				checked <- reply{result, err}
			}()
			waitForControlLockWait(t, f.admin, observer.work.PgConn().PID(), f.store.work.PgConn().PID())
			select {
			case early := <-checked:
				t.Fatalf("overtook pending create: %+v", early)
			default:
			}
			release()
			first := <-created
			if first.err != nil || first.result.Outcome != OutcomeCommitted {
				t.Fatalf("creation: %+v %v", first.result, first.err)
			}
			got := <-checked
			if resolve {
				if got.err != nil || got.result.Outcome != OutcomeReplayed || got.result.Fact.ID != first.result.Fact.ID || !got.result.Fact.CreatedAt.Equal(first.result.Fact.CreatedAt) {
					t.Fatalf("resolved before original outcome settled: %+v %v", got.result, got.err)
				}
			} else if kerr := kernelErr(t, got.result, got.err); kerr.Class != ClassRoomTaken {
				t.Fatalf("competing create took over: %+v", got.result)
			}
			if got := creationCounts(t, f, cmd.RoomID); got != [7]int64{1, 1, 2, 2, 1, 1, 1} {
				t.Fatalf("pending creation settled incorrectly: %v", got)
			}
		})
	}
}

// A syntactically valid stored encoding is still untrusted if its creation
// identity or frozen intent disagrees with the accepted operation receipt.
func TestRoomCreateCorruptStoredCreationCannotReplay(t *testing.T) {
	changes := []struct {
		name   string
		change func(*model.EnterpriseRoomCreation)
	}{
		{"codec version", func(c *model.EnterpriseRoomCreation) { c.Enterprise.Version = 999 }},
		{"initial owner", func(c *model.EnterpriseRoomCreation) { c.Enterprise.InitialOwnerUserID = "u-viewer" }},
		{"configured organization", func(c *model.EnterpriseRoomCreation) { c.Enterprise.OrgID = "org-other" }},
		{"deployment", func(c *model.EnterpriseRoomCreation) { c.Enterprise.DeploymentID = "dep-other" }},
		{"name", func(c *model.EnterpriseRoomCreation) { c.Name = "changed stored name" }},
		{"collaboration", func(c *model.EnterpriseRoomCreation) {
			c.Collaboration = &model.Collaboration{Version: 2, Mode: "custom", Instructions: "different policy"}
		}},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			cmd := roomCreateCommand(t)
			first, err := f.store.CommitRoomCreate(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			creation, err := model.DecodeEnterpriseRoomCreation(first.Fact)
			if err != nil {
				t.Fatal(err)
			}
			change.change(&creation)
			first.Fact.Data, err = json.Marshal(creation)
			if err != nil {
				t.Fatal(err)
			}
			encoding, err := json.Marshal(first.Fact)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.admin.Exec(ctx, `UPDATE room_events SET event_encoding = $1 WHERE room_id = $2 AND seq = 1`, encoding, cmd.RoomID); err != nil {
				t.Fatal(err)
			}
			before := creationCounts(t, f, cmd.RoomID)
			for _, resolve := range []bool{false, true} {
				var res Result
				if resolve {
					res, err = f.store.ResolveRoomCreate(ctx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
				} else {
					res, err = f.store.CommitRoomCreate(ctx, cmd)
				}
				if kerr := kernelErr(t, res, err); kerr.Class != ClassStoredFactUnreadable || res.Fact.ID != "" || len(res.Fact.Data) != 0 {
					t.Fatalf("corrupt creation disclosed, resolve=%v: %+v", resolve, res)
				}
			}
			if got := creationCounts(t, f, cmd.RoomID); got != before {
				t.Fatalf("corrupt replay wrote rows: %v -> %v", before, got)
			}
		})
	}
}
