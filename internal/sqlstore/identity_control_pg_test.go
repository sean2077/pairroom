package sqlstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// Completions below are trusted synthetic verifier outputs. These tests use
// real PostgreSQL and real session candidates, not a live identity provider
// or browser cookie exchange (those are separate HTTP/OIDC acceptance tests).
func identityPGSubject(subject string) enterpriseauth.VerifiedIdentity {
	return enterpriseauth.VerifiedIdentity{ConnectionID: "corporate", Issuer: "https://idp.example.test/issuer", Subject: subject}
}

func identityPGConfig() IdentityConfig {
	other := identityPGSubject("member-one")
	other.ConnectionID = "other-connection"
	return IdentityConfig{
		Connections: []TrustConnectionConfig{{"corporate", "https://idp.example.test/issuer"}, {"other-connection", "https://idp.example.test/issuer"}},
		Bootstrap:   identityPGSubject("operator-selected"),
		Admitted:    []enterpriseauth.VerifiedIdentity{identityPGSubject("member-one"), identityPGSubject("member-two"), other},
	}
}

func identityPGFixture(t *testing.T, transform func(string) string) *fixture {
	t.Helper()
	f := newRawFixture(t)
	f.cfg.RequireSessionAuth = true
	cfg := f.cfg
	if transform != nil {
		cfg.DSN = transform(cfg.DSN)
	}
	var err error
	f.store, err = Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	result, err := f.store.InitializeIdentity(context.Background(), identityPGConfig())
	if err != nil || result.Outcome != OutcomeCommitted {
		t.Fatalf("identity fixture configuration: %+v %v", result, err)
	}
	return f
}

func identityPGLoginCommand(identity enterpriseauth.VerifiedIdentity, id string) IdentityLoginCommand {
	return IdentityLoginCommand{ClientID: "login-" + id, Completion: enterpriseauth.Completion{Purpose: enterpriseauth.PurposeLogin, Identity: identity}, Session: BrowserSessionCandidate{
		SessionID: "session-" + id, TokenHash: sha256.Sum256([]byte("synthetic-browser-token-" + id)), CSRFHash: sha256.Sum256([]byte("synthetic-csrf-token-" + id)), ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond),
	}}
}

func identityPGLogin(t *testing.T, f *fixture, subject, id string) (IdentityLoginCommand, BrowserSession) {
	t.Helper()
	cmd := identityPGLoginCommand(identityPGSubject(subject), id)
	result, err := f.store.AdmitIdentity(context.Background(), cmd)
	if err != nil || result.Outcome != OutcomeCommitted || result.Control == nil {
		t.Fatalf("login: %+v %v", result, err)
	}
	session, err := f.store.AuthenticateSession(context.Background(), cmd.Session.TokenHash)
	if err != nil || session.Auth.UserID != result.Control.UserID || session.Auth.SessionID != cmd.Session.SessionID || session.CSRFHash != cmd.Session.CSRFHash {
		t.Fatalf("authenticate accepted session: %+v %v", session, err)
	}
	return cmd, session
}

func identityPGCounts(t *testing.T, f *fixture) [8]int64 {
	t.Helper()
	var got [8]int64
	err := f.admin.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM external_identities), (SELECT count(*) FROM browser_sessions), (SELECT count(*) FROM organization_admins WHERE enabled), (SELECT count(*) FROM control_events), (SELECT count(*) FROM control_operations), (SELECT count(*) FROM audit), revision FROM identity_policy WHERE id`).Scan(&got[0], &got[1], &got[2], &got[3], &got[4], &got[5], &got[6], &got[7])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func assertIdentityAuthorization(t *testing.T, result Result, err error) {
	t.Helper()
	if kerr := kernelErr(t, result, err); kerr.Class != ClassAuthorization || result.Control != nil || result.Fact.ID != "" {
		t.Fatalf("authorization failure disclosed metadata: %+v %v", result, err)
	}
}

func assertSessionRefused(t *testing.T, f *fixture, token [32]byte) {
	t.Helper()
	session, err := f.store.AuthenticateSession(context.Background(), token)
	var kerr *KernelError
	if !errors.As(err, &kerr) || kerr.Class != ClassAuthorization || session != (BrowserSession{}) {
		t.Fatalf("invalid credential disclosed a session: %+v %v", session, err)
	}
}

func TestIdentityControlExactAdmissionAndExplicitBootstrap(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	before := identityPGCounts(t, f)
	unadmitted := identityPGLoginCommand(identityPGSubject("unlisted-admin@example.test"), "unadmitted")
	result, err := f.store.AdmitIdentity(ctx, unadmitted)
	assertIdentityAuthorization(t, result, err)
	if got := identityPGCounts(t, f); got != before {
		t.Fatalf("unadmitted identity wrote rows: %v -> %v", before, got)
	}
	first, member := identityPGLogin(t, f, "member-one", "first-login")
	if member.Administrator {
		t.Fatal("the first admitted login became administrator")
	}
	_, admin := identityPGLogin(t, f, "operator-selected", "bootstrap")
	if !admin.Administrator || admin.Auth.UserID == member.Auth.UserID {
		t.Fatal("explicit bootstrap identity did not get its own administrator role")
	}
	other := identityPGLoginCommand(first.Completion.Identity, "other-connection")
	other.Completion.Identity.ConnectionID = "other-connection"
	result, err = f.store.AdmitIdentity(ctx, other)
	if err != nil || result.Control == nil || result.Control.UserID == member.Auth.UserID || result.Control.Administrator {
		t.Fatalf("equal subject across configured connections merged identities: %+v %v", result, err)
	}
	wrongIssuer := identityPGLoginCommand(first.Completion.Identity, "wrong-issuer")
	wrongIssuer.Completion.Identity.Issuer = "https://idp.example.test/other-issuer"
	result, err = f.store.AdmitIdentity(ctx, wrongIssuer)
	assertIdentityAuthorization(t, result, err)
	before = identityPGCounts(t, f)
	result, err = f.store.AdmitIdentity(ctx, first)
	if err != nil || result.Outcome != OutcomeReplayed || result.Control.UserID != member.Auth.UserID || result.Control.SessionID != first.Session.SessionID {
		t.Fatalf("original login replay: %+v %v", result, err)
	}
	if got := identityPGCounts(t, f); got != before {
		t.Fatal("login replay duplicated persistent state")
	}
}

func TestIdentityControlPersistsOnlySessionVerifiersAndRedactedReceipts(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	cmd, session := identityPGLogin(t, f, "member-one", "hashes")
	var token, csrf []byte
	if err := f.admin.QueryRow(ctx, `SELECT token_hash,csrf_hash FROM browser_sessions WHERE session_id=$1`, cmd.Session.SessionID).Scan(&token, &csrf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(token, cmd.Session.TokenHash[:]) || !bytes.Equal(csrf, cmd.Session.CSRFHash[:]) {
		t.Fatal("stored verifiers disagree with accepted candidate")
	}
	var data string
	if err := f.admin.QueryRow(ctx, `SELECT COALESCE(string_agg(convert_from(receipt_encoding,'UTF8'),' '),'') || ' ' || (SELECT COALESCE(string_agg(row_to_json(a)::text,' '),'') FROM audit a) FROM control_events`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"synthetic-browser-token-hashes", "synthetic-csrf-token-hashes", hex.EncodeToString(token), hex.EncodeToString(csrf), "TokenHash", "CSRFHash", "csrf_hash", "token_hash"} {
		if strings.Contains(data, forbidden) {
			t.Fatalf("receipt/audit leaked verifier %q", forbidden)
		}
	}
	encoded, _ := json.Marshal(session)
	if strings.Contains(string(encoded), "CSRFHash") {
		t.Fatal("session projection serialized CSRF verifier")
	}
	changed := cmd
	changed.Session.CSRFHash = sha256.Sum256([]byte("different-csrf"))
	result, err := f.store.AdmitIdentity(ctx, changed)
	if kerr := kernelErr(t, result, err); kerr.Outcome != OutcomeConflict || result.Control != nil {
		t.Fatalf("changed candidate replay: %+v", result)
	}
	assertSessionRefused(t, f, sha256.Sum256([]byte("unknown-browser-token")))
}

func TestIdentityControlSessionRevocationCannotCrossUserOrReplayMetadata(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	a, aSession := identityPGLogin(t, f, "member-one", "user-a-1")
	a2, aSecond := identityPGLogin(t, f, "member-one", "user-a-2")
	b, bSession := identityPGLogin(t, f, "member-two", "user-b")
	forged := aSession.Auth
	forged.SessionID = bSession.Auth.SessionID
	before := identityPGCounts(t, f)
	result, err := f.store.RevokeSession(ctx, forged, "forged-revoke")
	assertIdentityAuthorization(t, result, err)
	if got := identityPGCounts(t, f); got != before {
		t.Fatal("cross-user session revoke wrote rows")
	}
	result, err = f.store.RevokeSession(ctx, aSession.Auth, "own-revoke")
	if err != nil || result.Outcome != OutcomeCommitted || result.Control.SessionID != a.Session.SessionID {
		t.Fatalf("own revoke: %+v %v", result, err)
	}
	assertSessionRefused(t, f, a.Session.TokenHash)
	for _, token := range [][32]byte{a2.Session.TokenHash, b.Session.TokenHash} {
		if _, err := f.store.AuthenticateSession(ctx, token); err != nil {
			t.Fatalf("own revoke spilled into another session: %v", err)
		}
	}
	before = identityPGCounts(t, f)
	result, err = f.store.RevokeSession(ctx, aSession.Auth, "own-revoke")
	assertIdentityAuthorization(t, result, err)
	result, err = f.store.ResolveIdentityLogin(ctx, a)
	assertIdentityAuthorization(t, result, err)
	result, err = f.store.AdmitIdentity(ctx, a)
	assertIdentityAuthorization(t, result, err)
	result, err = f.store.RevokeSession(ctx, aSecond.Auth, "own-revoke")
	if kerr := kernelErr(t, result, err); kerr.Outcome != OutcomeConflict || result.Control != nil {
		t.Fatalf("same key changed session target: %+v", result)
	}
	if got := identityPGCounts(t, f); got != before {
		t.Fatal("revoked receipt lookup changed persistent state")
	}
}

func TestIdentityControlSessionRecheckedInsideRoomPorts(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, session := identityPGLogin(t, f, "member-one", "room-session")
	create := roomCreateCommand(t)
	create.Auth = session.Auth
	create.RoomID = "room-session-test"
	if _, err := f.store.CommitRoomCreate(ctx, create); err != nil {
		t.Fatal(err)
	}
	port := NewRoomPort(f.store)
	auth := relay.EnterpriseAuth{Deployment: session.Auth.Deployment, UserID: session.Auth.UserID, SessionID: session.Auth.SessionID}
	engine, err := relay.OpenEnterprise(ctx, relay.Config{RoomID: create.RoomID}, port, auth)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close() }()
	message, err := engine.ExecuteHumanSend(ctx, auth, relay.SendRequest{ID: "accepted", To: model.ActorSlot2, Text: "private text"})
	if err != nil || message.ID == "" {
		t.Fatalf("authenticated send: %+v %v", message, err)
	}
	if _, err := f.store.RevokeSession(ctx, session.Auth, "logout"); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := f.admin.QueryRow(ctx, `SELECT count(*) FROM room_events WHERE room_id=$1`, create.RoomID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// The caller deliberately retains the earlier middleware-authenticated
	// context. Each actual transaction must reject it after logout commits.
	if _, err := engine.ExecuteHumanSend(ctx, auth, relay.SendRequest{ID: "accepted", To: model.ActorSlot2, Text: "private text"}); err == nil {
		t.Fatal("stale session recovered an accepted message")
	}
	resolved, err := port.ResolveHumanSend(ctx, auth, create.RoomID, "accepted", relay.EnterpriseSendIntent{To: "slot2", Text: "private text"})
	if err == nil || resolved.Fact.ID != "" {
		t.Fatalf("stale session recovered a receipt: %+v %v", resolved, err)
	}
	if snapshot, err := port.ReadonlySnapshot(ctx, auth, create.RoomID); err == nil || len(snapshot.Events) != 0 {
		t.Fatal("stale session read private history")
	}
	result, err := f.store.CommitRoomCreate(ctx, create)
	assertIdentityAuthorization(t, result, err)
	userOnly := session.Auth
	userOnly.SessionID = ""
	result, err = f.store.CommitRoomCreate(ctx, RoomCreateCommand{Auth: userOnly, RoomID: "user-only", ClientID: "without-session", Intent: create.Intent})
	assertIdentityAuthorization(t, result, err)
	var after int64
	if err := f.admin.QueryRow(ctx, `SELECT count(*) FROM room_events WHERE room_id=$1`, create.RoomID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("revoked context appended a Room fact")
	}
}

func TestIdentityControlRoomTransactionWaitsForConcurrentSessionRevoke(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, session := identityPGLogin(t, f, "member-one", "race-session")
	blocker := schemaConn(t, f.dsn, f.schema)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE browser_sessions SET revoked_at=pg_catalog.clock_timestamp() WHERE session_id=$1`, session.Auth.SessionID); err != nil {
		t.Fatal(err)
	}
	cmd := roomCreateCommand(t)
	cmd.Auth = session.Auth
	type answer struct {
		result Result
		err    error
	}
	done := make(chan answer, 1)
	go func() { result, err := f.store.CommitRoomCreate(ctx, cmd); done <- answer{result, err} }()
	waitForControlLockWait(t, f.admin, f.store.work.PgConn().PID(), blocker.PgConn().PID())
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-done
	assertIdentityAuthorization(t, got.result, got.err)
	if got := creationCounts(t, f, cmd.RoomID); got != [7]int64{} {
		t.Fatalf("transaction used stale pre-lock session: %v", got)
	}
}

func TestIdentityControlBrowserSessionSurvivesOwnershipRestartNotRestoreEpoch(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	cmd, old := identityPGLogin(t, f, "member-one", "restart")
	var epoch int64
	var restore string
	if err := f.admin.QueryRow(ctx, `SELECT epoch,(SELECT restore_epoch FROM identity_policy) FROM ownership`).Scan(&epoch, &restore); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.store, err = Open(ctx, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := f.store.AuthenticateSession(ctx, cmd.Session.TokenHash)
	if err != nil || fresh.Auth != old.Auth {
		t.Fatalf("ownership restart invalidated durable browser login: %+v %v", fresh, err)
	}
	var newEpoch int64
	var sameRestore string
	if err := f.admin.QueryRow(ctx, `SELECT epoch,(SELECT restore_epoch FROM identity_policy) FROM ownership`).Scan(&newEpoch, &sameRestore); err != nil {
		t.Fatal(err)
	}
	if newEpoch != epoch+1 || sameRestore != restore {
		t.Fatal("ownership and restore epochs were conflated")
	}
	if _, err := f.admin.Exec(ctx, `UPDATE identity_policy SET restore_epoch='restored-snapshot-epoch' WHERE id`); err != nil {
		t.Fatal(err)
	}
	assertSessionRefused(t, f, cmd.Session.TokenHash)
	result, err := f.store.ResolveIdentityLogin(ctx, cmd)
	assertIdentityAuthorization(t, result, err)
}

func TestIdentityControlEnableAndBootstrapReplayNeverReviveRevokedAuthority(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	aCmd, a := identityPGLogin(t, f, "operator-selected", "operator")
	_, b := identityPGLogin(t, f, "member-one", "future-admin")
	if _, err := f.store.SetAdministrator(ctx, a.Auth, "promote", b.Auth.UserID, true); err != nil {
		t.Fatal(err)
	}
	_, b = identityPGLogin(t, f, "member-one", "active-admin")
	if !b.Administrator {
		t.Fatal("explicit promotion not effective on new session")
	}
	if _, err := f.store.SetAdministrator(ctx, b.Auth, "demote-original", a.Auth.UserID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetUserEnabled(ctx, b.Auth, "disable-original", a.Auth.UserID, false); err != nil {
		t.Fatal(err)
	}
	before := identityPGCounts(t, f)
	result, err := f.store.InitializeIdentity(ctx, identityPGConfig())
	if err != nil || result.Outcome != OutcomeReplayed {
		t.Fatalf("startup config replay: %+v %v", result, err)
	}
	if got := identityPGCounts(t, f); got != before {
		t.Fatal("startup configuration duplicated effects")
	}
	var disabled, administrator bool
	var consumed string
	if err := f.admin.QueryRow(ctx, `SELECT u.disabled,a.enabled,p.bootstrap_consumed_by FROM users u JOIN organization_admins a USING(user_id) CROSS JOIN identity_policy p WHERE u.user_id=$1`, a.Auth.UserID).Scan(&disabled, &administrator, &consumed); err != nil {
		t.Fatal(err)
	}
	if !disabled || administrator || consumed != a.Auth.UserID {
		t.Fatal("startup replay restored disabled or demoted bootstrap authority")
	}
	assertSessionRefused(t, f, aCmd.Session.TokenHash)
	if _, err := f.store.SetUserEnabled(ctx, b.Auth, "enable-original", a.Auth.UserID, true); err != nil {
		t.Fatal(err)
	}
	assertSessionRefused(t, f, aCmd.Session.TokenHash)
	_, reenabled := identityPGLogin(t, f, "operator-selected", "enabled-new-login")
	if reenabled.Administrator || reenabled.Auth.UserID != a.Auth.UserID {
		t.Fatal("reenabling revived old admin role or relabeled User")
	}
}

func TestIdentityControlLastAdministratorRaceUsesDatabaseGuard(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, a := identityPGLogin(t, f, "operator-selected", "admin-one")
	_, b := identityPGLogin(t, f, "member-one", "admin-two-before")
	if _, err := f.store.SetAdministrator(ctx, a.Auth, "promote-two", b.Auth.UserID, true); err != nil {
		t.Fatal(err)
	}
	_, b = identityPGLogin(t, f, "member-one", "admin-two")
	// Two work connections share the admitted deployment lease in this test;
	// the in-process Store semaphore cannot serialize them. A real ownership
	// row barrier releases both competing demotions together.
	work := schemaConn(t, f.dsn, f.schema)
	if err := configureSession(ctx, work, f.schema); err != nil {
		t.Fatal(err)
	}
	other := &Store{cfg: f.cfg, lease: f.store.lease, work: work, sem: make(chan struct{}, 1)}
	blocker := schemaConn(t, f.dsn, f.schema)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT epoch FROM ownership WHERE id FOR SHARE`); err != nil {
		t.Fatal(err)
	}
	var results [2]Result
	var errs [2]error
	var wg sync.WaitGroup
	stores, auths := []*Store{f.store, other}, []AuthContext{a.Auth, b.Auth}
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = stores[i].SetAdministrator(ctx, auths[i], fmt.Sprintf("demote-self-%d", i), auths[i].UserID, false)
		}(i)
	}
	defer func() { _ = tx.Rollback(ctx); wg.Wait() }()
	waitIdentityControlGuardChain(t, f, []uint32{f.store.work.PgConn().PID(), other.work.PgConn().PID()}, blocker.PgConn().PID())
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	committed := 0
	for i, result := range results {
		if errs[i] == nil && result.Outcome == OutcomeCommitted {
			committed++
		} else if kerr := kernelErr(t, result, errs[i]); kerr.Class != "last_administrator" {
			t.Fatalf("unexpected losing race: %+v", result)
		}
	}
	var admins int
	if err := f.admin.QueryRow(ctx, `SELECT count(*) FROM organization_admins a JOIN users u USING(user_id) WHERE a.enabled AND NOT u.disabled`).Scan(&admins); err != nil {
		t.Fatal(err)
	}
	if committed != 1 || admins != 1 {
		t.Fatalf("last administrator lost: commits=%d activeadmins=%d", committed, admins)
	}
}

// A queued row-lock requester can be blocked by an earlier requester, which
// itself waits on the original SHARE holder. Observe that exact PostgreSQL
// blocking chain rather than requiring both waiters to name the holder directly.
func waitIdentityControlGuardChain(t *testing.T, f *fixture, waiters []uint32, holder uint32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		all := true
		for _, pid := range waiters {
			var blocked bool
			err := f.admin.QueryRow(ctx, `WITH RECURSIVE blockers(pid,depth) AS (
				SELECT unnest(pg_catalog.pg_blocking_pids($1)),1
				UNION ALL
				SELECT next.pid,b.depth+1 FROM blockers b CROSS JOIN LATERAL unnest(pg_catalog.pg_blocking_pids(b.pid)) next(pid) WHERE b.depth < 8
			) SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity WHERE pid=$1 AND state='active' AND wait_event_type='Lock') AND EXISTS(SELECT 1 FROM blockers WHERE pid=$2)`, int32(pid), int32(holder)).Scan(&blocked)
			if err != nil {
				t.Fatalf("control backend %d did not wait behind %d: %v", pid, holder, err)
			}
			all = all && blocked
		}
		if all {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("both control backends did not reach the held ownership guard")
		case <-ticker.C:
		}
	}
}

func TestIdentityControlLastAdministratorCountsLoginAdmission(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, a := identityPGLogin(t, f, "operator-selected", "sole-admin")
	before := identityPGCounts(t, f)
	result, err := f.store.SetIdentityAdmission(ctx, a.Auth, "remove-sole-admission", identityPGSubject("operator-selected"), false)
	if kerr := kernelErr(t, result, err); kerr.Class != "last_administrator" {
		t.Fatalf("last login-eligible administrator lost admission: %+v", result)
	}
	if got := identityPGCounts(t, f); got != before {
		t.Fatal("refused last-admin admission removal wrote facts")
	}
	_, b := identityPGLogin(t, f, "member-one", "second-admin-before")
	if _, err := f.store.SetAdministrator(ctx, a.Auth, "make-second-admin", b.Auth.UserID, true); err != nil {
		t.Fatal(err)
	}
	_, b = identityPGLogin(t, f, "member-one", "second-admin-after")
	if _, err := f.store.SetIdentityAdmission(ctx, b.Auth, "remove-first-admission", identityPGSubject("operator-selected"), false); err != nil {
		t.Fatal(err)
	}
	// A retains enabled User/admin rows but has no admitted identity. B is now
	// the last administrator able to log in, so counting role rows is unsafe.
	result, err = f.store.SetAdministrator(ctx, b.Auth, "demote-last-login-admin", b.Auth.UserID, false)
	if kerr := kernelErr(t, result, err); kerr.Class != "last_administrator" {
		t.Fatalf("role-only count let last admitted admin demote itself: %+v", result)
	}
	result, err = f.store.SetUserEnabled(ctx, b.Auth, "disable-last-login-admin", b.Auth.UserID, false)
	if kerr := kernelErr(t, result, err); kerr.Class != "last_administrator" {
		t.Fatalf("role-only count let last admitted admin disable itself: %+v", result)
	}
}

func TestIdentityControlAuditFailureRollsBackIdentityAndSession(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	before := identityPGCounts(t, f)
	if _, err := f.admin.Exec(ctx, `ALTER TABLE audit ADD CONSTRAINT test_reject_identity_login CHECK(kind <> 'identity.login')`); err != nil {
		t.Fatal(err)
	}
	cmd := identityPGLoginCommand(identityPGSubject("operator-selected"), "audit-fail")
	result, err := f.store.AdmitIdentity(ctx, cmd)
	if kerr := kernelErr(t, result, err); kerr.Outcome != OutcomeNotCommitted || kerr.Class != ClassConstraintViolation {
		t.Fatalf("audit fault: %+v", result)
	}
	if got := identityPGCounts(t, f); got != before {
		t.Fatalf("audit failure left partial identities/session/admin/receipt: %v -> %v", before, got)
	}
	var consumed *string
	if err := f.admin.QueryRow(ctx, `SELECT bootstrap_consumed_by FROM identity_policy`).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != nil {
		t.Fatal("audit failure consumed bootstrap")
	}
	assertSessionRefused(t, f, cmd.Session.TokenHash)
	result, err = f.store.ResolveIdentityLogin(ctx, cmd)
	if err != nil || result.Outcome != OutcomeNotCommitted || result.Control != nil {
		t.Fatalf("rolled-back login resolve: %+v %v", result, err)
	}
}

func TestIdentityControlAuditFailureRollsBackUserDisableAndSessionRevocation(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, admin := identityPGLogin(t, f, "operator-selected", "audit-admin")
	memberCmd, member := identityPGLogin(t, f, "member-one", "audit-member")
	before := identityPGCounts(t, f)
	if _, err := f.admin.Exec(ctx, `ALTER TABLE audit ADD CONSTRAINT test_reject_user_status CHECK(kind <> 'user.status')`); err != nil {
		t.Fatal(err)
	}
	result, err := f.store.SetUserEnabled(ctx, admin.Auth, "failed-disable", member.Auth.UserID, false)
	if kerr := kernelErr(t, result, err); kerr.Outcome != OutcomeNotCommitted || kerr.Class != ClassConstraintViolation {
		t.Fatalf("privileged audit failure: %+v", result)
	}
	if got := identityPGCounts(t, f); got != before {
		t.Fatalf("failed audited status left partial receipts: %v -> %v", before, got)
	}
	var disabled, revoked bool
	if err := f.admin.QueryRow(ctx, `SELECT u.disabled,s.revoked_at IS NOT NULL FROM users u JOIN browser_sessions s USING(user_id) WHERE session_id=$1`, member.Auth.SessionID).Scan(&disabled, &revoked); err != nil {
		t.Fatal(err)
	}
	if disabled || revoked {
		t.Fatal("audit failure changed user/session authority")
	}
	if _, err := f.store.AuthenticateSession(ctx, memberCmd.Session.TokenHash); err != nil {
		t.Fatalf("failed disable revoked the browser token: %v", err)
	}
}

func TestIdentityControlUnknownLoginBothRealCommitOutcomes(t *testing.T) {
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			proxy := startProxy(t, mode)
			f := identityPGFixture(t, proxy.dsn)
			ctx := context.Background()
			cmd := identityPGLoginCommand(identityPGSubject("operator-selected"), "unknown-login")
			before := identityPGCounts(t, f)
			proxy.arm()
			result, err := f.store.AdmitIdentity(ctx, cmd)
			if kerr := kernelErr(t, result, err); kerr.Outcome != OutcomeUnknown || result.Control != nil {
				t.Fatalf("lost COMMIT disclosed success/session: %+v", result)
			}
			resolved, err := f.store.ResolveIdentityLogin(ctx, cmd)
			if err != nil {
				t.Fatal(err)
			}
			after := identityPGCounts(t, f)
			if mode == "drop-response" {
				if resolved.Outcome != OutcomeReplayed || resolved.Control == nil || resolved.Control.SessionID != cmd.Session.SessionID || !resolved.Control.Administrator || after[2] != before[2]+1 || after[4] != before[4]+1 {
					t.Fatalf("accepted login verification: %+v counts=%v", resolved, after)
				}
				for i := range before {
					if after[i] != before[i]+1 {
						t.Fatalf("accepted first login has partial control state: %v -> %v", before, after)
					}
				}
			} else if resolved.Outcome != OutcomeNotCommitted || resolved.Control != nil || after != before {
				t.Fatalf("absent login verification: %+v counts=%v", resolved, after)
			}
			// SQL uncertainty must not secretly replay the login. Only a separate
			// explicit caller instruction after a new Store may issue it again.
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			f.store, err = Open(ctx, f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			explicit, err := f.store.AdmitIdentity(ctx, cmd)
			if err != nil || explicit.Control == nil || explicit.Control.SessionID != cmd.Session.SessionID {
				t.Fatalf("explicit login recovery: %+v %v", explicit, err)
			}
			if mode == "drop-response" && !reflect.DeepEqual(explicit.Control, resolved.Control) {
				t.Fatal("explicit recovery changed original accepted login receipt")
			}
			if _, err := f.store.AuthenticateSession(ctx, cmd.Session.TokenHash); err != nil {
				t.Fatalf("accepted candidate did not authenticate after recovery: %v", err)
			}
			var sessions int
			if err := f.admin.QueryRow(ctx, `SELECT count(*) FROM browser_sessions WHERE session_id=$1`, cmd.Session.SessionID).Scan(&sessions); err != nil {
				t.Fatal(err)
			}
			if sessions != 1 {
				t.Fatal("uncertain recovery duplicated sessions")
			}
		})
	}
}

func TestIdentityControlUserDisableWithLiveBindingIsFailClosedUntilRevocationSlice(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, admin := identityPGLogin(t, f, "operator-selected", "binding-admin")
	memberCmd, member := identityPGLogin(t, f, "member-one", "binding-member")
	cmd := roomCreateCommand(t)
	cmd.Auth = member.Auth
	cmd.RoomID = "bound-member-room"
	if _, err := f.store.CommitRoomCreate(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`INSERT INTO devices(device_id,org_id,user_id) VALUES('test-device','org-test',$1)`,
		`INSERT INTO agents(agent_id,org_id,device_id,stable_source_key,runtime_kind,runtime_recorded_at) VALUES('test-agent','org-test','test-device','opaque-source','claude',now())`,
		`INSERT INTO bindings(binding_id,org_id,room_id,slot,generation,agent_id) VALUES('test-binding','org-test','bound-member-room','slot1',1,'test-agent')`,
	} {
		var err error
		if strings.Contains(statement, "$1") {
			_, err = f.admin.Exec(ctx, statement, member.Auth.UserID)
		} else {
			_, err = f.admin.Exec(ctx, statement)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	before := identityPGCounts(t, f)
	result, err := f.store.SetUserEnabled(ctx, admin.Auth, "blocked-binding-disable", member.Auth.UserID, false)
	if kerr := kernelErr(t, result, err); kerr.Class != ClassUnsupportedControlState {
		t.Fatalf("unsupported Binding revocation was simulated: %+v", result)
	}
	if got := identityPGCounts(t, f); got != before {
		t.Fatal("unsupported revocation changed control facts")
	}
	if _, err := f.store.AuthenticateSession(ctx, memberCmd.Session.TokenHash); err != nil {
		t.Fatalf("unsupported revocation partially killed browser session: %v", err)
	}
}

func TestIdentityControlAdmissionChangeDoesNotGrantAdminOrRevokeExistingSession(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, admin := identityPGLogin(t, f, "operator-selected", "admission-admin")
	memberCmd, member := identityPGLogin(t, f, "member-one", "admission-member")
	target := identityPGSubject("newly-admitted")
	result, err := f.store.SetIdentityAdmission(ctx, member.Auth, "unauthorized-admission", target, true)
	assertIdentityAuthorization(t, result, err)
	if _, err := f.store.SetIdentityAdmission(ctx, admin.Auth, "admit-new", target, true); err != nil {
		t.Fatal(err)
	}
	_, newMember := identityPGLogin(t, f, "newly-admitted", "admitted-login")
	if newMember.Administrator {
		t.Fatal("admission granted administrator")
	}
	if _, err := f.store.SetIdentityAdmission(ctx, admin.Auth, "remove-admission", identityPGSubject("member-one"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AuthenticateSession(ctx, memberCmd.Session.TokenHash); err != nil {
		t.Fatalf("admission removal implicitly revoked existing session: %v", err)
	}
	result, err = f.store.AdmitIdentity(ctx, identityPGLoginCommand(identityPGSubject("member-one"), "blocked-new-login"))
	assertIdentityAuthorization(t, result, err)
}

func TestIdentityControlExpiredSessionAndDisabledOrganizationAreRefused(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	cmd, session := identityPGLogin(t, f, "member-one", "live-before-expiry")
	expiredHash := sha256.Sum256([]byte("synthetic-expired-token"))
	if _, err := f.admin.Exec(ctx, `INSERT INTO browser_sessions(session_id,user_id,token_hash,csrf_hash,restore_epoch,created_at,expires_at) SELECT 'expired-session',$1,$2,$3,restore_epoch,pg_catalog.clock_timestamp()-interval '2 hours',pg_catalog.clock_timestamp()-interval '1 hour' FROM identity_policy`, session.Auth.UserID, expiredHash[:], cmd.Session.CSRFHash[:]); err != nil {
		t.Fatal(err)
	}
	assertSessionRefused(t, f, expiredHash)
	if _, err := f.admin.Exec(ctx, `UPDATE organizations SET disabled=true WHERE org_id='org-test'`); err != nil {
		t.Fatal(err)
	}
	assertSessionRefused(t, f, cmd.Session.TokenHash)
}

// Until Binding retirement is implemented in this transaction, neither an
// active member's downgrade nor re-admission of a disabled member as viewer
// may leave that user's original active Binding usable or restorable.
func TestIdentityControlViewerTransitionWithLiveBindingIsFailClosed(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("disabled=%v", disabled), func(t *testing.T) {
			f, cmd := membershipFixture(t)
			commitMembership(t, f, cmd)
			membershipExec(t, f.admin, `INSERT INTO devices(device_id,org_id,user_id) VALUES('device-target','org-test','u-target')`)
			membershipExec(t, f.admin, `INSERT INTO agents(agent_id,org_id,device_id,stable_source_key) VALUES('agent-target','org-test','device-target','source-target')`)
			membershipExec(t, f.admin, `INSERT INTO bindings(binding_id,org_id,room_id,slot,generation,agent_id) VALUES('binding-target','org-test',$1,'slot1',1,'agent-target')`, cmd.RoomID)
			if disabled {
				membershipExec(t, f.admin, `UPDATE room_members SET disabled=true WHERE room_id=$1 AND user_id='u-target'`, cmd.RoomID)
			}
			cmd.ClientID, cmd.ExpectedRevision, cmd.Intent.Role = "viewer-transition", 2, RoomViewer
			cmd.Intent.Action = MembershipChangeRole
			if disabled {
				cmd.Intent.Action = MembershipAdmit
			}
			before := creationCounts(t, f, cmd.RoomID)
			result, err := f.store.CommitRoomMembership(context.Background(), cmd)
			if kerr := kernelErr(t, result, err); kerr.Class != ClassUnsupportedControlState || result.Fact.ID != "" {
				t.Fatalf("viewer transition left an active Binding under reduced authority: %+v", result)
			}
			requireMembership(t, f, cmd.RoomID, "u-target", "member", disabled)
			if got := creationCounts(t, f, cmd.RoomID); got != before {
				t.Fatalf("unsupported viewer transition wrote control/history/audit: %v -> %v", before, got)
			}
			var retired bool
			var generation int64
			if err := f.admin.QueryRow(context.Background(), `SELECT retired_at IS NOT NULL,generation FROM bindings WHERE binding_id='binding-target'`).Scan(&retired, &generation); err != nil {
				t.Fatal(err)
			}
			if retired || generation != 1 {
				t.Fatal("unsupported viewer transition partially changed Binding authority")
			}
		})
	}
}
