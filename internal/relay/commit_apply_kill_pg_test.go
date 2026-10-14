package relay_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
	"github.com/sean2077/pairroom/internal/sqlstore/sqltest"
)

const commitApplyChildEnv = "PAIRROOM_COMMIT_APPLY_CHILD"
const commitApplyText = "ordinary producer committed before derived projection apply"
const commitApplyID = "original-commit-apply"

type commitApplyReport struct {
	Phase            string
	PID              int
	Epoch            int64
	Instance         string
	Result           relay.EnterpriseSendResult
	Snapshot         relay.Snapshot
	Commits, Appends int
	Message          relay.Message
}

// This test-only port delegates to the real typed SQL port before stopping.
// It never changes the outcome, simulates a COMMIT, or injects a production hook.
type commitApplyPort struct {
	relay.EnterprisePort
	commits     int
	afterCommit func(relay.EnterpriseSendResult)
}

func (p *commitApplyPort) CommitHumanSend(ctx context.Context, cmd relay.EnterpriseSendCommand) (relay.EnterpriseSendResult, error) {
	p.commits++
	result, err := p.EnterprisePort.CommitHumanSend(ctx, cmd)
	if err == nil && p.afterCommit != nil {
		p.afterCommit(result)
	}
	return result, err
}

func TestEnterpriseCommitApplyKillChild(t *testing.T) {
	mode := os.Getenv(commitApplyChildEnv)
	if mode == "" {
		return
	}
	ctx := t.Context()
	schema := os.Getenv("PAIRROOM_COMMIT_APPLY_SCHEMA")
	cfg := sqlstore.Config{DSN: os.Getenv("PAIRROOM_SQLSTORE_TEST_DSN"), Schema: schema, DeploymentID: "commit-apply-deployment", RequireSessionAuth: true}
	db, err := sqlstore.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	admin := sqltest.Connect(t, cfg.DSN, schema)
	var epoch int64
	var instance string
	if err := admin.QueryRow(ctx, `SELECT epoch,instance_id FROM ownership WHERE id`).Scan(&epoch, &instance); err != nil {
		t.Fatal(err)
	}
	auth := relay.EnterpriseAuth{Deployment: cfg.DeploymentID, UserID: os.Getenv("PAIRROOM_COMMIT_APPLY_USER"), SessionID: "fixture-owner-session"}
	port := &commitApplyPort{EnterprisePort: sqlstore.NewRoomPort(db)}
	appends := 0
	engine, err := relay.OpenEnterprise(ctx, relay.Config{RoomID: "room-1", OnAppend: func(model.Event) { appends++ }}, port, auth)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	report := func(phase string, result relay.EnterpriseSendResult, message relay.Message) {
		if err := json.NewEncoder(os.Stdout).Encode(commitApplyReport{Phase: phase, PID: os.Getpid(), Epoch: epoch, Instance: instance, Result: result, Snapshot: engine.Snapshot(), Commits: port.commits, Appends: appends, Message: message}); err != nil {
			t.Fatal(err)
		}
	}
	input := bufio.NewReader(os.Stdin)
	request := relay.SendRequest{ID: commitApplyID, Text: commitApplyText, To: model.ActorSlot2}
	if mode == "commit" {
		port.afterCommit = func(result relay.EnterpriseSendResult) {
			if result.Outcome != relay.EnterpriseOutcomeCommitted {
				t.Fatal("barrier must follow an actual committed result")
			}
			report("committed-before-apply", result, relay.Message{})
			// The parent never releases this barrier: it kills this OS process.
			if _, err := input.ReadString('\n'); err != nil {
				t.Fatal("parent closed barrier instead of killing child", err)
			}
		}
		if _, err := engine.ExecuteHumanSend(ctx, auth, request); err != nil {
			t.Fatal(err)
		}
		t.Fatal("first producer unexpectedly returned beyond the kill barrier")
	}
	if mode != "rebuild" {
		t.Fatal("invalid child mode")
	}
	result, err := port.ResolveHumanSend(ctx, auth, "room-1", commitApplyID, relay.EnterpriseSendIntent{To: "slot2", Text: commitApplyText})
	if err != nil {
		t.Fatal(err)
	}
	report("cold-rebuilt-and-resolved", result, relay.Message{})
	line, err := input.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "explicit-original-send" {
		t.Fatal("missing explicit recovery instruction", err)
	}
	message, err := engine.ExecuteHumanSend(ctx, auth, request)
	if err != nil {
		t.Fatal(err)
	}
	report("explicit-original-replay", result, message)
}

type commitApplyProcess struct {
	cmd   *exec.Cmd
	stdin interface{ Write([]byte) (int, error) }
	lines chan commitApplyReport
}

func startCommitApplyProcess(t *testing.T, schema, user, mode string) *commitApplyProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEnterpriseCommitApplyKillChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), commitApplyChildEnv+"="+mode, "PAIRROOM_COMMIT_APPLY_SCHEMA="+schema, "PAIRROOM_COMMIT_APPLY_USER="+user)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Child errors never contain a DSN; the standard test error is captured.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	process := &commitApplyProcess{cmd: cmd, stdin: stdin, lines: make(chan commitApplyReport, 4)}
	go func() {
		defer close(process.lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			if !strings.HasPrefix(scanner.Text(), "{") {
				continue
			}
			var report commitApplyReport
			if json.Unmarshal(scanner.Bytes(), &report) == nil {
				process.lines <- report
			}
		}
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return process
}

func (p *commitApplyProcess) next(t *testing.T) commitApplyReport {
	t.Helper()
	select {
	case report, ok := <-p.lines:
		if !ok {
			t.Fatal("child exited without the expected barrier/report")
		}
		return report
	case <-time.After(20 * time.Second):
		t.Fatal("child did not reach expected barrier/report")
		return commitApplyReport{}
	}
}

func TestEnterpriseActualProcessKillAfterCommitBeforeProjectionApply(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	cfg := sqlstore.Config{DSN: dsn, Schema: schema, DeploymentID: "commit-apply-deployment", RequireSessionAuth: true}
	ctx := t.Context()
	if err := sqlstore.InitializeDeployment(ctx, cfg, "org-commit-apply"); err != nil {
		t.Fatal(err)
	}
	setup, err := sqlstore.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setup.Close() })
	// Trusted synthetic verifier output is only fixture authentication; no OIDC
	// or vendor/model acceptance is asserted by this SQL/process boundary.
	identity := enterpriseauth.VerifiedIdentity{ConnectionID: "fixture", Issuer: "https://fixture-idp.example.invalid", Subject: "owner"}
	if _, err := setup.InitializeIdentity(ctx, sqlstore.IdentityConfig{Connections: []sqlstore.TrustConnectionConfig{{ConnectionID: identity.ConnectionID, Issuer: identity.Issuer}}, Bootstrap: identity}); err != nil {
		t.Fatal(err)
	}
	token := sha256.Sum256([]byte("fixture-browser-token"))
	if _, err := setup.AdmitIdentity(ctx, sqlstore.IdentityLoginCommand{ClientID: "fixture-login", Completion: enterpriseauth.Completion{Purpose: enterpriseauth.PurposeLogin, Identity: identity}, Session: sqlstore.BrowserSessionCandidate{SessionID: "fixture-owner-session", TokenHash: token, CSRFHash: sha256.Sum256([]byte("fixture-csrf")), ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}}); err != nil {
		t.Fatal(err)
	}
	browser, err := setup.AuthenticateSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := model.PrepareEnterpriseRoomIntent("Commit/apply crash boundary", model.Collaboration{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.CommitRoomCreate(ctx, sqlstore.RoomCreateCommand{Auth: browser.Auth, RoomID: "room-1", ClientID: "fixture-create", Intent: intent}); err != nil {
		t.Fatal(err)
	}
	if err := setup.Close(); err != nil {
		t.Fatal(err)
	}
	admin := sqltest.Connect(t, dsn, schema)
	before := rowCounts3c(t, admin, "room-1")
	old := startCommitApplyProcess(t, schema, browser.Auth.UserID, "commit")
	barrier := old.next(t)
	if barrier.Phase != "committed-before-apply" || barrier.Result.Outcome != relay.EnterpriseOutcomeCommitted || barrier.Result.Seq != 2 || barrier.Result.Revision != 2 || barrier.Snapshot.Sequence != 1 || len(barrier.Snapshot.Messages) != 0 || barrier.Commits != 1 || barrier.Appends != 0 {
		t.Fatal("wrong cut: COMMIT must be durable and Engine projection still unchanged")
	}
	// The external observer proves visibility before the actual process kill.
	assertCommitApplySQL(t, admin, barrier.Result, browser.Auth.UserID, before)
	if err := old.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := old.cmd.Wait(); err == nil {
		t.Fatal("producer exited gracefully instead of OS kill")
	}
	t.Logf("actual OS kill at post-COMMIT/pre-apply barrier: pid=%d epoch=%d seq=%d revision=%d", barrier.PID, barrier.Epoch, barrier.Result.Seq, barrier.Result.Revision)
	assertCommitApplySQL(t, admin, barrier.Result, browser.Auth.UserID, before)
	next := startCommitApplyProcess(t, schema, browser.Auth.UserID, "rebuild")
	rebuilt := next.next(t)
	if rebuilt.Phase != "cold-rebuilt-and-resolved" || rebuilt.PID == barrier.PID || rebuilt.Epoch != barrier.Epoch+1 || rebuilt.Instance == barrier.Instance || rebuilt.Commits != 0 || rebuilt.Appends != 0 || rebuilt.Result.Outcome != relay.EnterpriseOutcomeReplayed || rebuilt.Result.Seq != barrier.Result.Seq || rebuilt.Result.Revision != barrier.Result.Revision || !reflect.DeepEqual(rebuilt.Result.Fact, barrier.Result.Fact) || rebuilt.Snapshot.Sequence != 2 || len(rebuilt.Snapshot.Messages) != 1 {
		t.Fatalf("cold rebuild/Resolve gate: phase=%s pid=%d epoch=%d commits=%d appends=%d outcome=%s sequence=%d messages=%d", rebuilt.Phase, rebuilt.PID, rebuilt.Epoch, rebuilt.Commits, rebuilt.Appends, rebuilt.Result.Outcome, rebuilt.Snapshot.Sequence, len(rebuilt.Snapshot.Messages))
	}
	var original relay.Message
	if json.Unmarshal(barrier.Result.Fact.Data, &original) != nil {
		t.Fatal("invalid original fact")
	}
	// ExecuteHumanSend is the legacy text-only producer: its optional public
	// Author stays empty; the immutable stored origin below names the typed Owner.
	// sqlstore.TestHumanEvidenceAuthorMustBeOriginalAuthenticatedPrincipal owns
	// that compatibility. The newer rich producer's stable public Author is
	// separately covered by sqlstore/message_evidence_pg_test.go and
	// enterpriseservice/capacity_observation_test.go; this cut does not certify it.
	if original.Author != "" || original.From != model.ActorUser || original.FromRuntime != "" || original.FromGeneration != 0 || original.TargetGeneration != 1 || original.Text != commitApplyText || !reflect.DeepEqual(rebuilt.Snapshot.Messages[0], original) {
		t.Fatalf("cold rebuild identity: original author=%q from=%s runtime=%q fromgen=%d targetgen=%d; rebuilt author=%q runtime=%q fromgen=%d targetgen=%d; equal=%t", original.Author, original.From, original.FromRuntime, original.FromGeneration, original.TargetGeneration, rebuilt.Snapshot.Messages[0].Author, rebuilt.Snapshot.Messages[0].FromRuntime, rebuilt.Snapshot.Messages[0].FromGeneration, rebuilt.Snapshot.Messages[0].TargetGeneration, reflect.DeepEqual(rebuilt.Snapshot.Messages[0], original))
	}
	assertCommitApplySQL(t, admin, barrier.Result, browser.Auth.UserID, before)
	if _, err := fmt.Fprintln(next.stdin, "explicit-original-send"); err != nil {
		t.Fatal(err)
	}
	replayed := next.next(t)
	if replayed.Phase != "explicit-original-replay" || replayed.Commits != 1 || replayed.Appends != 0 || !reflect.DeepEqual(replayed.Message, original) || replayed.Snapshot.Sequence != 2 || len(replayed.Snapshot.Messages) != 1 {
		t.Fatal("explicit original-ID replay duplicated durable or derived effects")
	}
	if err := next.cmd.Wait(); err != nil {
		t.Fatal("replacement process failed", err)
	}
	assertCommitApplySQL(t, admin, barrier.Result, browser.Auth.UserID, before)
	t.Logf("independent cold rebuild: pid=%d epoch=%d original receipt=%s commits_before_explicit=0", rebuilt.PID, rebuilt.Epoch, rebuilt.Result.Outcome)
}

func assertCommitApplySQL(t *testing.T, admin *pgx.Conn, result relay.EnterpriseSendResult, user string, before [3]int64) {
	t.Helper()
	if got := rowCounts3c(t, admin, "room-1"); got != [3]int64{before[0] + 1, before[1] + 1, before[2] + 1} {
		t.Fatalf("durable fact/operation/audit count changed: %v", got)
	}
	var encoding []byte
	var seq, revision, opSeq, opRevision uint64
	var count int
	if err := admin.QueryRow(t.Context(), `SELECT r.current_seq,r.revision,o.result_seq,o.result_revision,e.event_encoding,(SELECT count(*) FROM audit WHERE room_id='room-1' AND kind='fact.append' AND user_id=$1) FROM rooms r JOIN operations o ON o.room_id=r.room_id AND o.op_kind='human_send' AND o.identifier=$2 AND o.principal=$1 JOIN room_events e ON e.room_id=o.room_id AND e.seq=o.result_seq WHERE r.room_id='room-1'`, user, commitApplyID).Scan(&seq, &revision, &opSeq, &opRevision, &encoding, &count); err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Event      model.Event `json:"event"`
		OriginKind string      `json:"origin_kind"`
		OriginUser string      `json:"origin_user"`
	}
	if json.Unmarshal(encoding, &stored) != nil || seq != result.Seq || revision != result.Revision || opSeq != seq || opRevision != revision || count != 1 || stored.OriginKind != "user" || stored.OriginUser != user || !reflect.DeepEqual(stored.Event, result.Fact) {
		t.Fatal("canonical fact, original receipt, audit or Room cursor disagrees after kill/rebuild")
	}
}
