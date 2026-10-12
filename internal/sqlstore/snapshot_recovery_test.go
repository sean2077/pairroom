package sqlstore

import (
	"context"
	"testing"
)

// The full lost-response chain over a real protocol proxy: the human-send
// commit response is dropped (Unknown, work connection gone), Resolve
// recovers the accepted result through its bounded replacement connection,
// and ReadonlySnapshot serves the same Room through its own bounded read-only
// replacement — same fence and authorization, unchanged epoch, no retried
// effect.
func TestAcceptedResponseLossChainEndsWithSnapshot(t *testing.T) {
	proxy := startProxy(t, "drop-response")
	f := newFixtureProxy(t, func(dsn string) string { return proxy.dsn(dsn) })
	ctx := context.Background()
	cmd := humanSendCommand("u-owner", "chain-1", "slot2", "chain body")
	proxy.arm()

	res, err := f.store.CommitHumanSend(ctx, cmd)
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeUnknown {
		t.Fatalf("lost response must be unknown: %+v", res)
	}
	resolved, err := f.store.ResolveHumanSend(ctx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
	if err != nil || resolved.Outcome != OutcomeReplayed || resolved.Seq != 1 {
		t.Fatalf("resolve after loss: %+v %v", resolved, err)
	}
	snap, err := f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-1")
	if err != nil {
		t.Fatalf("snapshot after the chain: %v", err)
	}
	if snap.Seq != 1 || snap.Revision != 1 || len(snap.Events) != 1 || snap.Events[0].ID != cmd.Fact.ID {
		t.Fatalf("snapshot must show the complete committed state: %+v", snap)
	}
	if events, _, _, seq, revision := f.roomCounts(t); events != 1 || seq != 1 || revision != 1 {
		t.Fatalf("chain wrote extra state: events=%d seq=%d revision=%d", events, seq, revision)
	}
}

// The broken-before-server chain: nothing was committed, Resolve proves the
// absence, and the snapshot still serves the (empty) Room through its
// replacement connection.
func TestBrokenCommitChainEndsWithNotCommittedAndEmptySnapshot(t *testing.T) {
	proxy := startProxy(t, "break-on-commit")
	f := newFixtureProxy(t, func(dsn string) string { return proxy.dsn(dsn) })
	ctx := context.Background()
	cmd := humanSendCommand("u-owner", "chain-2", "slot2", "broken body")
	proxy.arm()

	res, err := f.store.CommitHumanSend(ctx, cmd)
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeUnknown {
		t.Fatalf("broken commit must be unknown: %+v", res)
	}
	resolved, err := f.store.ResolveHumanSend(ctx, cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
	if err != nil || resolved.Outcome != OutcomeNotCommitted {
		t.Fatalf("resolve must prove the absence: %+v %v", resolved, err)
	}
	snap, err := f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-1")
	if err != nil {
		t.Fatalf("snapshot after the chain: %v", err)
	}
	if snap.Seq != 0 || snap.Revision != 0 || len(snap.Events) != 0 {
		t.Fatalf("snapshot must show the empty Room: %+v", snap)
	}
	if events, _, _, _, _ := f.roomCounts(t); events != 0 {
		t.Fatalf("broken chain wrote %d events", events)
	}
}

// If the replacement connection cannot be established either, the read stays
// unknown with zero disclosure.
func TestSnapshotUnknownWhenReplacementCannotVerify(t *testing.T) {
	proxy := startProxy(t, "drop-response")
	f := newFixtureProxy(t, func(dsn string) string { return proxy.dsn(dsn) })
	ctx := context.Background()
	cmd := humanSendCommand("u-owner", "chain-3", "slot2", "unreachable body")
	proxy.arm()
	res, err := f.store.CommitHumanSend(ctx, cmd)
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeUnknown {
		t.Fatalf("setup loss: %+v", res)
	}
	// The replacement target is unreachable; only configuration data changes.
	f.store.cfg.DSN = "postgres://postgres@127.0.0.1:1/pairroom_test?sslmode=disable&connect_timeout=1"
	_, err = f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-1")
	expectKernel(t, err, OutcomeUnknown, ClassConnectionFailed)
}

// Authorization still gates the replacement path: a disabled principal gets a
// refusal with zero disclosure even though the work connection is gone.
func TestSnapshotReplacementStillEnforcesAuthorization(t *testing.T) {
	proxy := startProxy(t, "drop-response")
	f := newFixtureProxy(t, func(dsn string) string { return proxy.dsn(dsn) })
	ctx := context.Background()
	cmd := humanSendCommand("u-owner", "chain-4", "slot2", "auth body")
	proxy.arm()
	res, err := f.store.CommitHumanSend(ctx, cmd)
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeUnknown {
		t.Fatalf("setup loss: %+v", res)
	}
	if _, err := f.admin.Exec(ctx, `UPDATE users SET disabled = true WHERE user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.ReadonlySnapshot(ctx, AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-1")
	expectKernel(t, err, OutcomeRefused, ClassAuthorization)
}
