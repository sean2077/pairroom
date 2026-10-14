package sqlstore

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func exactAuthorityWork(t *testing.T, f *fixture) *Store {
	t.Helper()
	work := schemaConn(t, f.dsn, f.schema)
	if configureSession(t.Context(), work, f.schema) != nil {
		t.Fatal("independent work session unavailable")
	}
	return &Store{cfg: f.store.cfg, lease: f.store.lease, work: work, sem: make(chan struct{}, 1)}
}

func exactAuthorityRooms(t *testing.T) (*bindingPGState, []HumanMessageCommand) {
	t.Helper()
	b := bindingPGFixture(t, nil)
	intent, err := model.PrepareEnterpriseRoomIntent("Second exact authority Room", model.Collaboration{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := b.f.store.CommitRoomCreate(t.Context(), RoomCreateCommand{Auth: b.owner.Auth, RoomID: "authority-room-b", ClientID: "create-exact-b", Intent: intent})
	if err != nil || r.Outcome != OutcomeCommitted {
		t.Fatal("second Room creation failed")
	}
	r, err = b.f.store.CommitRoomMembership(t.Context(), RoomMembershipCommand{Auth: b.owner.Auth, RoomID: "authority-room-b", ClientID: "admit-exact-b", ExpectedRevision: r.Revision, Intent: RoomMembershipIntent{TargetUserID: b.member.Auth.UserID, Action: MembershipAdmit, Role: RoomMember}})
	if err != nil || r.Outcome != OutcomeCommitted {
		t.Fatal("second Room Member admission failed")
	}
	commands := []HumanMessageCommand{}
	for i, room := range []string{"binding-room", "authority-room-b"} {
		var revision uint64
		if b.f.admin.QueryRow(t.Context(), `SELECT revision FROM rooms WHERE room_id=$1`, room).Scan(&revision) != nil {
			t.Fatal("Room revision unavailable")
		}
		commands = append(commands, HumanMessageCommand{Auth: b.member.Auth, RoomID: room, ClientID: fmt.Sprintf("exact-multi-write-%d", i), ExpectedRevision: revision, Intent: HumanSendIntent{To: "slot1", Text: fmt.Sprintf("complete multi-Room write %d", i)}})
	}
	return b, commands
}

func exactAuthorityEffect(t *testing.T, b *bindingPGState, c HumanMessageCommand) [3]int64 {
	t.Helper()
	var n [3]int64
	key, class := humanSendKey(HumanSendCommand{Auth: c.Auth, RoomID: c.RoomID, ClientID: c.ClientID, Intent: c.Intent})
	if class != "" {
		t.Fatal("invalid test key")
	}
	if b.f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM operations WHERE room_id=$1 AND identifier=$2 AND principal=$3),(SELECT count(*) FROM room_events WHERE room_id=$1 AND kind='native.message.updated'),(SELECT count(*) FROM audit WHERE room_id=$1 AND operation_id=$4)`, c.RoomID, c.ClientID, c.Auth.UserID, mutationOperationID(key)).Scan(&n[0], &n[1], &n[2]) != nil {
		t.Fatal("exact multi-Room effect read failed")
	}
	return n
}

// Whole original rows include intent/result hashes, identifier/principal,
// cursor and encoded fact. Counts alone would miss replacement/relabeling.
func exactAuthorityOriginal(t *testing.T, b *bindingPGState, c HumanMessageCommand) [2]string {
	t.Helper()
	var digest [2]string
	if b.f.admin.QueryRow(t.Context(), `SELECT (SELECT md5(COALESCE(jsonb_agg(to_jsonb(o) ORDER BY to_jsonb(o)::text),'[]'::jsonb)::text) FROM operations o WHERE room_id=$1 AND identifier=$2 AND principal=$3),(SELECT md5(COALESCE(jsonb_agg(to_jsonb(e) ORDER BY to_jsonb(e)::text),'[]'::jsonb)::text) FROM room_events e WHERE room_id=$1)`, c.RoomID, c.ClientID, c.Auth.UserID).Scan(&digest[0], &digest[1]) != nil {
		t.Fatal("original accepted row digest unavailable")
	}
	return digest
}

func TestMultiRoomWritesUserDisableAndMemberRemovalExactLockChains(t *testing.T) {
	for _, disableFirst := range []bool{false, true} {
		name := "writes-first"
		if disableFirst {
			name = "disable-first"
		}
		t.Run(name, func(t *testing.T) {
			b, commands := exactAuthorityRooms(t)
			writers := []*Store{b.f.store, exactAuthorityWork(t, b.f)}
			disable, remover := exactAuthorityWork(t, b.f), exactAuthorityWork(t, b.f)
			blocker := schemaConn(t, b.f.dsn, b.f.schema)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			tx, err := blocker.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			defer func() { _ = tx.Rollback(context.Background()); cancel(); wg.Wait() }()
			if disableFirst {
				_, err = tx.Exec(ctx, `SELECT user_id FROM users WHERE user_id=$1 FOR UPDATE`, b.member.Auth.UserID)
			} else {
				_, err = tx.Exec(ctx, `SELECT room_id FROM rooms WHERE room_id IN ('binding-room','authority-room-b') ORDER BY room_id FOR UPDATE`)
			}
			if err != nil {
				t.Fatal("actual database barrier failed")
			}
			results := make([]Result, 2)
			errs := make([]error, 2)
			var disabled, removed Result
			var disableErr, removeErr error
			launchWrites := func() {
				for i := range writers {
					wg.Add(1)
					go func(i int) { defer wg.Done(); results[i], errs[i] = writers[i].CommitHumanMessage(ctx, commands[i]) }(i)
				}
			}
			launchDisable := func() {
				wg.Add(1)
				go func() {
					defer wg.Done()
					disabled, disableErr = disable.SetUserEnabled(ctx, b.admin.Auth, "exact-multi-disable", b.member.Auth.UserID, false)
				}()
			}
			removeRevision := commands[0].ExpectedRevision
			if !disableFirst {
				removeRevision++
			}
			remove := RoomMembershipCommand{Auth: b.owner.Auth, RoomID: commands[0].RoomID, ClientID: "exact-multi-remove-a", ExpectedRevision: removeRevision, Intent: RoomMembershipIntent{TargetUserID: b.member.Auth.UserID, Action: MembershipRemove}}
			if disableFirst {
				launchDisable()
				waitForControlLockWait(t, b.f.admin, disable.work.PgConn().PID(), blocker.PgConn().PID())
				launchWrites()
			} else {
				launchWrites()
				for _, writer := range writers {
					waitForControlLockWait(t, b.f.admin, writer.work.PgConn().PID(), blocker.PgConn().PID())
				}
				launchDisable()
			}
			wg.Add(1)
			go func() { defer wg.Done(); removed, removeErr = remover.CommitRoomMembership(ctx, remove) }()
			pids := []uint32{writers[0].work.PgConn().PID(), writers[1].work.PgConn().PID(), disable.work.PgConn().PID(), remover.work.PgConn().PID()}
			waitIdentityControlGuardChain(t, b.f, pids, blocker.PgConn().PID())
			for _, c := range commands {
				if exactAuthorityEffect(t, b, c) != ([3]int64{}) {
					t.Fatal("blocked operation released partial multi-Room effect")
				}
			}
			started := time.Now()
			if tx.Commit(ctx) != nil {
				t.Fatal("database barrier release failed")
			}
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("multi-Room authority lock composition exceeded caller budget")
			}
			if disableErr != nil || disabled.Outcome != OutcomeCommitted || removeErr != nil || removed.Outcome != OutcomeCommitted {
				t.Fatalf("typed authority combination failed: disable=%s/%s remove=%s/%s", disabled.Outcome, disabled.Class, removed.Outcome, removed.Class)
			}
			for i, c := range commands {
				if disableFirst {
					if k := kernelErr(t, results[i], errs[i]); k.Class != ClassAuthorization || results[i].Relay != nil || len(results[i].Facts) != 0 || exactAuthorityEffect(t, b, c) != ([3]int64{}) {
						t.Fatal("disable-first multi-Room writer disclosed body or committed effect")
					}
				} else {
					if errs[i] != nil || results[i].Outcome != OutcomeCommitted || exactAuthorityEffect(t, b, c) != ([3]int64{1, 1, 1}) {
						t.Fatalf("writes-first exact effect failed: Room%d outcome=%s class=%s effect=%v", i, results[i].Outcome, results[i].Class, exactAuthorityEffect(t, b, c))
					}
					snapshot, err := b.f.store.ReadonlySnapshot(t.Context(), b.owner.Auth, c.RoomID)
					if err != nil {
						t.Fatal("live Owner cannot read retained history")
					}
					matches := 0
					for _, event := range snapshot.Events {
						if reflect.DeepEqual(event, results[i].Fact) {
							matches++
						}
					}
					payload, err := decodeHumanSendPayload(results[i].Fact.Data)
					if err != nil || matches != 1 || payload.Text != c.Intent.Text || payload.Author != relay.EnterpriseHumanAuthor(c.Auth.UserID) || payload.ClientKey != humanSendClientKey(c.Auth.UserID, c.ClientID) {
						t.Fatal("original accepted fact changed or missing from Owner history")
					}
				}
				before := exactAuthorityEffect(t, b, c)
				original := exactAuthorityOriginal(t, b, c)
				resolved, e := b.f.store.ResolveHumanSend(t.Context(), c.Auth, c.RoomID, c.ClientID, c.Intent)
				if k := kernelErr(t, resolved, e); k.Class != ClassAuthorization || resolved.Relay != nil || len(resolved.Facts) != 0 || !reflect.DeepEqual(resolved.Fact, model.Event{}) || exactAuthorityEffect(t, b, c) != before || exactAuthorityOriginal(t, b, c) != original {
					t.Fatal("disabled principal recovered body/receipt or replayed an effect")
				}
			}
			requireMembership(t, b.f, commands[0].RoomID, b.member.Auth.UserID, "member", true)
			requireMembership(t, b.f, commands[1].RoomID, b.member.Auth.UserID, "member", false)
			t.Logf("actual four-backend lock chain across two Rooms; disable_first=%t; settlement_ms=%d; expected business effect per Room=%d; original disabled-principal Resolve denied without effect", disableFirst, time.Since(started).Milliseconds(), map[bool]int{false: 1, true: 0}[disableFirst])
		})
	}
}

func TestMultiRoomMemberRemovalDoesNotInvalidateOtherRoomOriginalReceipt(t *testing.T) {
	b, commands := exactAuthorityRooms(t)
	writer := exactAuthorityWork(t, b.f)
	remover := exactAuthorityWork(t, b.f)
	blocker := schemaConn(t, b.f.dsn, b.f.schema)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(ctx, `SELECT user_id FROM users WHERE user_id=$1 FOR UPDATE`, b.member.Auth.UserID); err != nil {
		t.Fatal(err)
	}
	remove := RoomMembershipCommand{Auth: b.owner.Auth, RoomID: commands[0].RoomID, ClientID: "remove-a-independent-b", ExpectedRevision: commands[0].ExpectedRevision, Intent: RoomMembershipIntent{TargetUserID: b.member.Auth.UserID, Action: MembershipRemove}}
	var wrote, removed Result
	var writeErr, removeErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); wrote, writeErr = writer.CommitHumanMessage(ctx, commands[1]) }()
	go func() { defer wg.Done(); removed, removeErr = remover.CommitRoomMembership(ctx, remove) }()
	defer func() { _ = tx.Rollback(context.Background()); cancel(); wg.Wait() }()
	waitIdentityControlGuardChain(t, b.f, []uint32{writer.work.PgConn().PID(), remover.work.PgConn().PID()}, blocker.PgConn().PID())
	if tx.Commit(ctx) != nil {
		t.Fatal("independent Room barrier release failed")
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("independent multi-Room composition exceeded budget")
	}
	if writeErr != nil || wrote.Outcome != OutcomeCommitted || removeErr != nil || removed.Outcome != OutcomeCommitted {
		t.Fatal("legal different-Room write/removal did not both settle")
	}
	before := exactAuthorityEffect(t, b, commands[1])
	original := exactAuthorityOriginal(t, b, commands[1])
	replayed, err := b.f.store.ResolveHumanSend(ctx, commands[1].Auth, commands[1].RoomID, commands[1].ClientID, commands[1].Intent)
	if err != nil || replayed.Outcome != OutcomeReplayed || !reflect.DeepEqual(replayed.Fact, wrote.Fact) || exactAuthorityEffect(t, b, commands[1]) != before || exactAuthorityOriginal(t, b, commands[1]) != original {
		t.Fatal("RoomA removal changed RoomB original accepted receipt/effects")
	}
	refused, e := b.f.store.CommitHumanMessage(ctx, commands[0])
	if k := kernelErr(t, refused, e); k.Class != ClassAuthorization || exactAuthorityEffect(t, b, commands[0]) != ([3]int64{}) {
		t.Fatal("removed RoomA still accepted business effect")
	}
	requireMembership(t, b.f, commands[1].RoomID, b.member.Auth.UserID, "member", false)
	t.Log("two actual Room transactions sharing target User: removeA and writeB commit; original B receipt replays exactly once; A new write refused")
}
