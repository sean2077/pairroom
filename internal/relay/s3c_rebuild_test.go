package relay_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
	"github.com/sean2077/pairroom/internal/sqlstore/sqltest"
)

// This file implements the S3c-R controlled deployment rebuild contract, and
// only that: tests, test-only helpers and the facts their evidence rests on.
//
// A caller that observed an Unknown (connection_exception / commit_uncertain)
// keeps the original typed request OUTSIDE the failed service process — its
// deployment, Room, principal, client identity, target and full original text.
// The failed service process is replaced by a SECOND real child process on the
// same deployment: that replacement opens the deployment, verifies the pending
// outcome read-only with the original identity, key and intent, builds the
// Room engine from an authenticated read-only snapshot (relay.OpenEnterprise,
// never the reap-writing relay.Open), and only then executes a separately
// received, explicit send with the same key and intent. The parent keeps the
// original request and observes PostgreSQL; it never takes over the test's
// service store.
//
// Nothing here claims server-side persistence of a pending outcome, recovery
// from an arbitrary crash, a durable caller outbox, production service
// orchestration, restored logins, Bindings, collectors or HA. Production
// behavior is untouched; there is no new production API, control or hook.

// s3cAutoSendDemonstration is the test-side flaw injection for the
// no-auto-send sensitivity experiment: when true, the replacement process
// sends the original request once BEFORE it verifies anything — the forbidden
// automatic retry. The commit-call counter and the row counts must reject it
// with red in both lost-response shapes. It is never enabled in normal runs;
// the experiment is run by flipping it, and the red and green runs are
// recorded as evidence.
const s3cAutoSendDemonstration = false

const (
	s3cHelperEnv     = "PAIRROOM_S3C_HELPER"
	s3cStoreDSNEnv   = "PAIRROOM_S3C_STORE_DSN"
	s3cDirectDSNEnv  = "PAIRROOM_S3C_DIRECT_DSN"
	s3cSchemaEnv     = "PAIRROOM_S3C_SCHEMA"
	s3cDeploymentEnv = "PAIRROOM_S3C_DEPLOYMENT"
)

// s3cScanLimit bounds one IPC line: a legal body is at most MaxBodyBytes
// (256 KiB) and JSON escaping can double it, so the default 64 KiB scanner is
// not enough — both directions use this explicit bound and check scanner.Err.
const s3cScanLimit = 4 << 20

// s3cLargeText builds a legal body larger than one scanner default (64 KiB)
// with newlines, quotes, backslashes and non-ASCII text, so the verbatim
// round trip is real.
func s3cLargeText(prefix string) string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteString("\nline with \"quotes\" and \\backslashes\\ and 中文 and \t tabs\n")
	for b.Len() < 96<<10 {
		b.WriteString("0123456789abcdef")
	}
	b.WriteString("\nend")
	return b.String()
}

// s3cOutcomeAccepted is the child's report for a human send its engine
// accepted; the committed/replayed distinction is asserted parent-side through
// the read-only verification, not guessed in the child.
const s3cOutcomeAccepted = "accepted"

// s3cCommand is one instruction from the parent. The deployment, principal,
// Room, client identity, target and the complete original text travel here
// verbatim: the child hard-codes no user, no target and no text, so a wrong
// identity or target cannot be masked by the fixture.
type s3cCommand struct {
	Op         string `json:"op"`
	Deployment string `json:"deployment,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	Room       string `json:"room,omitempty"`
	ClientID   string `json:"client_id,omitempty"`
	To         string `json:"to,omitempty"`
	Text       string `json:"text,omitempty"`
}

// s3cMessage is one report from the child over stdout. Ops names what the
// child actually executed, and the counters are read from the child's own
// counting port, so "no command was sent before verification" is measured.
type s3cMessage struct {
	Ready     bool   `json:"ready,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Epoch     int64  `json:"epoch,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Ops       string `json:"ops,omitempty"`
	Room      string `json:"room,omitempty"`
	Outcome   string `json:"outcome,omitempty"`
	Class     string `json:"class,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Text      string `json:"text,omitempty"`
	To        string `json:"to,omitempty"`
	Created   string `json:"created,omitempty"`
	Commits   int64  `json:"commits,omitempty"`
	Resolves  int64  `json:"resolves,omitempty"`
	Error     string `json:"error,omitempty"`
	Exited    bool   `json:"exited,omitempty"`
}

// TestS3cHelperProcess is the child half of the rebuild tests. It runs only
// when re-executed with the private helper environment; the plain suite run
// returns without doing anything (no skip, no side effect).
func TestS3cHelperProcess(t *testing.T) {
	if os.Getenv(s3cHelperEnv) != "1" {
		return
	}
	runS3cHelperProcess()
	os.Exit(0)
}

// countingPort counts the commands each recovery step really issues, so "we
// verified before sending" is a measurement rather than a claim.
type countingPort struct {
	inner    relay.EnterprisePort
	commits  atomic.Int64
	resolves atomic.Int64
}

func (c *countingPort) CommitHumanSend(ctx context.Context, cmd relay.EnterpriseSendCommand) (relay.EnterpriseSendResult, error) {
	c.commits.Add(1)
	return c.inner.CommitHumanSend(ctx, cmd)
}

func (c *countingPort) ResolveHumanSend(ctx context.Context, auth relay.EnterpriseAuth, roomID, clientID string, intent relay.EnterpriseSendIntent) (relay.EnterpriseSendResult, error) {
	c.resolves.Add(1)
	return c.inner.ResolveHumanSend(ctx, auth, roomID, clientID, intent)
}

func (c *countingPort) ReadonlySnapshot(ctx context.Context, auth relay.EnterpriseAuth, roomID string) (relay.EnterpriseRoomSnapshot, error) {
	return c.inner.ReadonlySnapshot(ctx, auth, roomID)
}

// runS3cHelperProcess is one service process: it opens the deployment store
// (its DSN reaches it only through a private environment variable), reports
// readiness with its own PID, the ownership epoch and instance it read, and
// then executes the instructions it receives. Room engines are built lazily by
// an explicit open instruction, so the replacement process really performs
// Open → verify → authorized read-only open → explicit send in that order.
func runS3cHelperProcess() {
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	report := func(message s3cMessage) {
		encoded, _ := json.Marshal(message)
		fmt.Fprintln(out, string(encoded))
		out.Flush()
	}
	fail := func(err error) {
		report(s3cMessage{Error: err.Error()})
		os.Exit(1)
	}
	ctx := context.Background()
	schema := os.Getenv(s3cSchemaEnv)
	dbStore, err := sqlstore.Open(ctx, sqlstore.Config{DSN: os.Getenv(s3cStoreDSNEnv), Schema: schema, DeploymentID: os.Getenv(s3cDeploymentEnv)})
	if err != nil {
		fail(err)
	}
	port := &countingPort{inner: sqlstore.NewRoomPort(dbStore)}
	engines := map[string]*relay.Engine{}
	openEngine := func(command s3cCommand) (*relay.Engine, error) {
		if engine, ok := engines[command.Room]; ok {
			return engine, nil
		}
		auth := relay.EnterpriseAuth{Deployment: command.Deployment, UserID: command.UserID}
		engine, err := relay.OpenEnterprise(ctx, relay.Config{RoomID: command.Room}, port, auth)
		if err != nil {
			return nil, err
		}
		engines[command.Room] = engine
		return engine, nil
	}
	admin, err := pgx.Connect(ctx, os.Getenv(s3cDirectDSNEnv))
	if err != nil {
		fail(err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	if _, err := admin.Exec(ctx, `SELECT pg_catalog.set_config('search_path', $1, false)`, schema+", pg_catalog"); err != nil {
		fail(err)
	}
	var epoch int64
	var instance string
	if err := admin.QueryRow(ctx, `SELECT epoch, instance_id FROM ownership WHERE id`).Scan(&epoch, &instance); err != nil {
		fail(err)
	}
	report(s3cMessage{Ready: true, PID: os.Getpid(), Epoch: epoch, Instance: instance, Ops: "open-store"})

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64<<10), s3cScanLimit)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var command s3cCommand
		if err := json.Unmarshal([]byte(line), &command); err != nil {
			fail(fmt.Errorf("stdin command: %w", err))
		}
		auth := relay.EnterpriseAuth{Deployment: command.Deployment, UserID: command.UserID}
		switch command.Op {
		case "resolve":
			if s3cAutoSendDemonstration {
				// The flaw: send before verifying anything, with the same
				// identity, key and intent the caller retained.
				if engine, err := openEngine(command); err == nil {
					_, _ = engine.ExecuteHumanSend(ctx, auth, relay.SendRequest{ID: command.ClientID, Text: command.Text, To: model.ActorID(command.To)})
				}
			}
			result, err := port.ResolveHumanSend(ctx, auth, command.Room, command.ClientID, relay.EnterpriseSendIntent{To: command.To, Text: command.Text})
			message := s3cMessage{Ops: "resolve", Room: command.Room, Outcome: result.Outcome, Commits: port.commits.Load(), Resolves: port.resolves.Load()}
			if err != nil {
				var commandErr *relay.EnterpriseCommandError
				if !errors.As(err, &commandErr) {
					fail(err)
				}
				message.Class = commandErr.Class
				if commandErr.Outcome != "" {
					message.Outcome = commandErr.Outcome
				}
			}
			report(message)
		case "open":
			engine, err := openEngine(command)
			if err != nil {
				fail(err)
			}
			_ = engine
			report(s3cMessage{Ops: "open", Room: command.Room, Commits: port.commits.Load(), Resolves: port.resolves.Load()})
		case "send":
			engine, err := openEngine(command)
			if err != nil {
				fail(err)
			}
			message, err := engine.ExecuteHumanSend(ctx, auth, relay.SendRequest{ID: command.ClientID, Text: command.Text, To: model.ActorID(command.To)})
			if err == nil {
				report(s3cMessage{Ops: "send", Room: command.Room, Outcome: s3cOutcomeAccepted, MessageID: message.ID, Text: message.Text, To: string(message.To), Created: message.CreatedAt.UTC().Format(time.RFC3339Nano), Commits: port.commits.Load(), Resolves: port.resolves.Load()})
				continue
			}
			var commandErr *relay.EnterpriseCommandError
			if errors.As(err, &commandErr) {
				report(s3cMessage{Ops: "send", Room: command.Room, Outcome: commandErr.Outcome, Class: commandErr.Class, Commits: port.commits.Load(), Resolves: port.resolves.Load()})
				continue
			}
			report(s3cMessage{Ops: "send", Room: command.Room, Outcome: "error", Error: err.Error()})
		case "exit":
			for _, engine := range engines {
				_ = engine.Close()
			}
			if err := dbStore.Close(); err != nil {
				fail(err)
			}
			report(s3cMessage{Ops: "exit", Exited: true})
			return
		default:
			fail(fmt.Errorf("unknown command %q", command.Op))
		}
	}
	if err := scanner.Err(); err != nil {
		fail(fmt.Errorf("stdin scan: %w", err))
	}
}

type s3cChild struct {
	cmd    *exec.Cmd
	stdin  *bufio.Writer
	lines  chan s3cMessage
	stderr *bytes.Buffer
}

// startS3cChild re-executes this test binary as a service process on the given
// store DSN. The old process opens the store through the proxy; the
// replacement process opens it directly, because the proxy stays armed and
// would drop the replacement's own first commit response.
func startS3cChild(t *testing.T, storeDSN, directDSN, schema, deployment string) *s3cChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestS3cHelperProcess$")
	cmd.Env = append(os.Environ(),
		s3cHelperEnv+"=1",
		s3cStoreDSNEnv+"="+storeDSN,
		s3cDirectDSNEnv+"="+directDSN,
		s3cSchemaEnv+"="+schema,
		s3cDeploymentEnv+"="+deployment,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &s3cChild{cmd: cmd, stdin: bufio.NewWriter(stdin), lines: make(chan s3cMessage, 16), stderr: stderr}
	go func() {
		defer close(child.lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64<<10), s3cScanLimit)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "{") {
				continue
			}
			var message s3cMessage
			if err := json.Unmarshal([]byte(line), &message); err != nil {
				child.lines <- s3cMessage{Error: "child stdout is not a report: " + err.Error()}
				return
			}
			child.lines <- message
		}
		if err := scanner.Err(); err != nil {
			child.lines <- s3cMessage{Error: "child stdout scan: " + err.Error()}
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return child
}

// command hands one instruction to the child over the test IPC channel.
func (c *s3cChild) command(t *testing.T, command s3cCommand) {
	t.Helper()
	encoded, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > s3cScanLimit {
		t.Fatalf("command exceeds the IPC bound: %d bytes", len(encoded))
	}
	if _, err := fmt.Fprintln(c.stdin, string(encoded)); err != nil {
		t.Fatalf("child stdin: %v (stderr: %s)", err, c.stderr.String())
	}
	if err := c.stdin.Flush(); err != nil {
		t.Fatalf("child flush: %v", err)
	}
}

func (c *s3cChild) next(t *testing.T) s3cMessage {
	t.Helper()
	select {
	case message, ok := <-c.lines:
		if !ok {
			t.Fatalf("child produced no result (stderr: %s)", c.stderr.String())
		}
		if message.Error != "" {
			t.Fatalf("child error: %s (stderr: %s)", message.Error, c.stderr.String())
		}
		return message
	case <-time.After(60 * time.Second):
		t.Fatalf("child timed out (stderr: %s)", c.stderr.String())
		return s3cMessage{}
	}
}

// exitAndWait instructs the failing process to retire and confirms it really
// exited before the deployment is rebuilt.
func (c *s3cChild) exitAndWait(t *testing.T) {
	t.Helper()
	c.command(t, s3cCommand{Op: "exit"})
	if exited := c.next(t); !exited.Exited {
		t.Fatalf("child exit: %+v", exited)
	}
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("child exit code: %v (stderr: %s)", err, c.stderr.String())
	}
}

func rowCounts3c(t *testing.T, admin *pgx.Conn, room string) [3]int64 {
	t.Helper()
	var events, operations, audits int64
	err := admin.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM room_events WHERE room_id = $1),
		       (SELECT count(*) FROM operations WHERE room_id = $1),
		       (SELECT count(*) FROM audit WHERE room_id = $1)`, room).Scan(&events, &operations, &audits)
	if err != nil {
		t.Fatal(err)
	}
	return [3]int64{events, operations, audits}
}

// eventEncodingsHash hashes the authoritative stored bytes of one Room's whole
// log, in sequence order with length prefixes, so "the history did not change"
// is compared on the facts themselves and not on a row count.
func eventEncodingsHash(t *testing.T, admin *pgx.Conn, room string) string {
	t.Helper()
	rows, err := admin.Query(context.Background(), `SELECT event_encoding FROM room_events WHERE room_id = $1 ORDER BY seq`, room)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	digest := sha256.New()
	var length [8]byte
	for rows.Next() {
		var encoding []byte
		if err := rows.Scan(&encoding); err != nil {
			t.Fatal(err)
		}
		binary.BigEndian.PutUint64(length[:], uint64(len(encoding)))
		digest.Write(length[:])
		digest.Write(encoding)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// storedFactMessage reads the canonical message identity, creation time, full
// text and target of one stored fact straight from its authoritative encoding.
// The creation time is compared as its encoded RFC 3339 text: a round trip
// through `timestamptz` would round to microsecond precision and could differ
// by a nanosecond from the authoritative value.
func storedFactMessage(t *testing.T, admin *pgx.Conn, room string, seq int64) (string, string, string, string) {
	t.Helper()
	var id, createdText, text, to string
	err := admin.QueryRow(context.Background(), `
		SELECT convert_from(event_encoding, 'UTF8')::jsonb -> 'event' -> 'data' ->> 'id',
		       convert_from(event_encoding, 'UTF8')::jsonb -> 'event' -> 'data' ->> 'created_at',
		       convert_from(event_encoding, 'UTF8')::jsonb -> 'event' -> 'data' ->> 'text',
		       convert_from(event_encoding, 'UTF8')::jsonb -> 'event' -> 'data' ->> 'to'
		FROM room_events WHERE room_id = $1 AND seq = $2`, room, seq).Scan(&id, &createdText, &text, &to)
	if err != nil {
		t.Fatal(err)
	}
	return id, createdText, text, to
}

// storedOriginUser reads the immutable principal the storage layer recorded for
// one stored fact.
func storedOriginUser(t *testing.T, admin *pgx.Conn, room string, seq int64) string {
	t.Helper()
	var origin string
	if err := admin.QueryRow(context.Background(), `SELECT origin_user FROM room_events WHERE room_id = $1 AND seq = $2`, room, seq).Scan(&origin); err != nil {
		t.Fatal(err)
	}
	return origin
}

// assertLogContiguous proves the Room log is still one contiguous fact
// sequence: no gap, no rewrite.
func assertLogContiguous(t *testing.T, admin *pgx.Conn, room string) {
	t.Helper()
	var count, maxSeq, minSeq int64
	err := admin.QueryRow(context.Background(), `
		SELECT count(*), COALESCE(max(seq), 0), COALESCE(min(seq), 0) FROM room_events WHERE room_id = $1`, room).
		Scan(&count, &maxSeq, &minSeq)
	if err != nil {
		t.Fatal(err)
	}
	if count != maxSeq || (count > 0 && minSeq != 1) {
		t.Fatalf("room %s log is not contiguous: count=%d min=%d max=%d", room, count, minSeq, maxSeq)
	}
}

// aclSnapshot is the complete authorization state the deployment rebuild must
// not touch: every membership with its role and disabled flag, and every user
// with its disabled flag.
func aclSnapshot(t *testing.T, admin *pgx.Conn) []string {
	t.Helper()
	rows, err := admin.Query(context.Background(), `
		SELECT 'member:' || room_id || '/' || user_id || '/' || role || '/' || disabled::text FROM room_members
		UNION ALL
		SELECT 'user:' || user_id || '/' || disabled::text FROM users
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var snapshot []string
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			t.Fatal(err)
		}
		snapshot = append(snapshot, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func fetchOwnership(t *testing.T, admin *pgx.Conn) (int64, string) {
	t.Helper()
	var epoch int64
	var instance string
	if err := admin.QueryRow(context.Background(), `SELECT epoch, instance_id FROM ownership WHERE id`).Scan(&epoch, &instance); err != nil {
		t.Fatal(err)
	}
	return epoch, instance
}

// deployRebuild performs the in-process object-level rebuild used by the
// simulation-only test: every old Room engine is retired (closed through the
// enterprise shutdown, which settles in-flight commands on the gate) before
// the shared store is closed, and a fresh store — new instance, new epoch — is
// opened through the existing reviewed APIs. The rebuild unit is the whole
// deployment. It never touches relay.Open (which reaps) and never clears an
// old fatal or a sticky lease-lost incarnation. The process-level tests do not
// use it: there a real replacement child performs the restart itself.
func deployRebuild(t *testing.T, dsn, schema string, oldEngines []*relay.Engine, oldStore *sqlstore.Store) *sqlstore.Store {
	t.Helper()
	for _, engine := range oldEngines {
		if err := engine.Close(); err != nil {
			t.Fatalf("retire old engine: %v", err)
		}
	}
	if oldStore != nil {
		if err := oldStore.Close(); err != nil {
			t.Fatalf("retire old store: %v", err)
		}
	}
	freshStore, err := sqlstore.Open(context.Background(), sqlstore.Config{DSN: dsn, Schema: schema, DeploymentID: "dep-test"})
	if err != nil {
		t.Fatalf("deployment rebuild Open: %v", err)
	}
	t.Cleanup(func() { _ = freshStore.Close() })
	return freshStore
}

// The full process-level rebuild, both Unknown shapes through a real proxy. A
// first real child process loses its commit response and reports the typed
// unknown to the caller; it really exits. A SECOND real child process then
// takes the deployment over: it opens the store, verifies the pending outcome
// read-only with the original identity, key and intent, builds the Room engine
// from an authenticated read-only snapshot, and only then executes the
// separately received explicit send with the same key and intent. The parent
// keeps the original request and reads PostgreSQL; it never opens the test's
// service store.
func TestS3cProcessRebuildBothUnknownShapes(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			schema, _ := sqltest.CreateSchema(t, dsn)
			admin := sqltest.SeedEnterpriseRoom(t, dsn, schema, "u-owner2")
			birthCounts := rowCounts3c(t, admin, "room-1")
			proxy := sqltest.StartProxy(t, mode)

			// The caller retains the original typed request — Room, principal,
			// client identity, target and the complete original text — outside
			// every service process, before the first attempt is ever made.
			// The text is larger than one scanner default and carries newlines,
			// quotes, backslashes and non-ASCII characters.
			request := s3cCommand{Op: "send", Deployment: "dep-test", UserID: "u-owner", Room: "room-1", ClientID: "s3c-1", To: "slot2", Text: s3cLargeText("rebuild body")}

			oldChild := startS3cChild(t, proxy.DSN(dsn), dsn, schema, "dep-test")
			oldReady := oldChild.next(t)
			if !oldReady.Ready || oldReady.Epoch < 1 || oldReady.PID == 0 || oldReady.PID == os.Getpid() {
				t.Fatalf("old child ready: %+v", oldReady)
			}

			// Both injected boundaries are a lost commit response: the child
			// reports the typed Unknown and its class, which is what a caller
			// may act on — nothing else.
			proxy.Arm()
			oldChild.command(t, request)
			lost := oldChild.next(t)
			if lost.Outcome != relay.EnterpriseOutcomeUnknown || lost.Class != string(sqlstore.ClassConnectionException) {
				t.Fatalf("old child outcome = %+v, want unknown/connection_exception", lost)
			}
			oldChild.exitAndWait(t)

			before := rowCounts3c(t, admin, "room-1")
			var originalID, originalCreated, originalText string
			switch mode {
			case "drop-response":
				// The server accepted the commit before the response was lost.
				if before != [3]int64{birthCounts[0] + 1, birthCounts[1] + 1, birthCounts[2] + 1} {
					t.Fatalf("accepted-then-lost rows: %v", before)
				}
				originalID, originalCreated, originalText, _ = storedFactMessage(t, admin, "room-1", birthCounts[0]+1)
				if originalText != request.Text {
					t.Fatal("the accepted fact did not store the complete original text")
				}
			case "break-on-commit":
				// The COMMIT never reached the server: the operation is absent.
				if before != birthCounts {
					t.Fatalf("broken-commit rows: %v", before)
				}
			}
			roomOneHash := eventEncodingsHash(t, admin, "room-1")

			// The replacement service process: a distinct child that performs
			// the restart itself. The parent never opens this store.
			replacement := startS3cChild(t, dsn, dsn, schema, "dep-test")
			newReady := replacement.next(t)
			if !newReady.Ready || newReady.PID == 0 || newReady.PID == os.Getpid() || newReady.PID == oldReady.PID {
				t.Fatalf("replacement child is not a new process: %+v (old pid %d, parent pid %d)", newReady, oldReady.PID, os.Getpid())
			}
			if newReady.Instance == oldReady.Instance || newReady.Epoch != oldReady.Epoch+1 {
				t.Fatalf("replacement ownership: instance %q->%q epoch %d->%d", oldReady.Instance, newReady.Instance, oldReady.Epoch, newReady.Epoch)
			}
			if newReady.Commits != 0 {
				t.Fatalf("the replacement committed before any instruction: %d", newReady.Commits)
			}

			// 1) Verify with the original auth, key and intent — read-only, with
			// zero commits issued so far. The commit counter is what makes the
			// no-auto-send claim measurable: the flawed variant sends before
			// this point and is rejected here even when it is an idempotent
			// replay that would not change a single row.
			verifyCommand := request
			verifyCommand.Op = "resolve"
			replacement.command(t, verifyCommand)
			verified := replacement.next(t)
			if verified.Ops != "resolve" || verified.Commits != 0 {
				t.Fatalf("the verification path committed a command before certifying the outcome: %+v", verified)
			}
			switch mode {
			case "drop-response":
				if verified.Outcome != relay.EnterpriseOutcomeReplayed {
					t.Fatalf("verify after accepted loss = %+v, want replayed", verified)
				}
			case "break-on-commit":
				if verified.Outcome != relay.EnterpriseOutcomeNotCommitted {
					t.Fatalf("verify after broken commit = %+v, want not_committed", verified)
				}
			}
			if got := rowCounts3c(t, admin, "room-1"); got != before {
				t.Fatalf("rows changed before verification: %v -> %v", before, got)
			}

			// 2) The authenticated read-only open: a snapshot-derived engine,
			// no reap, no write, and still no command sent.
			replacement.command(t, s3cCommand{Op: "open", Deployment: "dep-test", UserID: "u-owner", Room: "room-1"})
			opened := replacement.next(t)
			if opened.Ops != "open" || opened.Commits != 0 {
				t.Fatalf("the read-only open sent a command: %+v", opened)
			}
			if got := rowCounts3c(t, admin, "room-1"); got != before {
				t.Fatalf("the read-only open wrote state: %v -> %v", before, got)
			}

			// 3) The separate explicit instruction: the same key, the same
			// intent, the complete original text — and now exactly one commit.
			replacement.command(t, request)
			sent := replacement.next(t)
			if sent.Ops != "send" || sent.Outcome != s3cOutcomeAccepted || sent.Commits != 1 || sent.Text != request.Text || sent.To != request.To {
				t.Fatalf("explicit re-send: %+v", sent)
			}
			switch mode {
			case "drop-response":
				// The canonical original result, not a new one: identity,
				// creation time and the complete text are the stored fact's.
				if sent.MessageID != originalID || sent.Text != originalText || sent.Created != originalCreated {
					t.Fatalf("explicit re-send returned %q at %s, want the original %q at %s", sent.MessageID, sent.Created, originalID, originalCreated)
				}
				if got := rowCounts3c(t, admin, "room-1"); got != before {
					t.Fatalf("explicit re-send duplicated the accepted fact: %v -> %v", before, got)
				}
			case "break-on-commit":
				after := rowCounts3c(t, admin, "room-1")
				if after[0] != before[0]+1 || after[1] != before[1]+1 || after[2] != before[2]+1 {
					t.Fatalf("explicit re-send did not add exactly one fact/operation/audit: %v -> %v", before, after)
				}
				storedID, _, storedText, _ := storedFactMessage(t, admin, "room-1", birthCounts[0]+1)
				if storedID != sent.MessageID || storedText != request.Text {
					t.Fatalf("the stored fact is %q/%q, want the explicit request %q", storedID, sent.MessageID, request.Text)
				}
			}
			assertLogContiguous(t, admin, "room-1")

			// 4) Identity and target travel verbatim: a different live member
			// and the other slot, on the same replacement instance. The stored
			// fact must carry that principal and that target.
			other := s3cCommand{Op: "send", Deployment: "dep-test", UserID: "u-owner2", Room: "room-1", ClientID: "s3c-2", To: "slot1", Text: "second user body"}
			replacement.command(t, other)
			accepted := replacement.next(t)
			if accepted.Ops != "send" || accepted.Outcome != s3cOutcomeAccepted || accepted.To != "slot1" || accepted.Commits != 2 {
				t.Fatalf("second-identity send: %+v", accepted)
			}
			storedID, _, _, storedTo := storedFactMessage(t, admin, "room-1", birthCounts[0]+2)
			if storedID != accepted.MessageID || storedTo != "slot1" {
				t.Fatalf("stored fact %q to %q, want %q to slot1", storedID, storedTo, accepted.MessageID)
			}
			if origin := storedOriginUser(t, admin, "room-1", birthCounts[0]+2); origin != "u-owner2" {
				t.Fatalf("stored principal = %q, want u-owner2", origin)
			}
			if storedHash := eventEncodingsHash(t, admin, "room-1"); storedHash == roomOneHash {
				t.Fatal("the second-identity send did not append a fact")
			}
			assertLogContiguous(t, admin, "room-1")

			// 5) The replacement process retires cleanly and really releases the
			// deployment: a fresh store can take it over afterwards.
			replacement.exitAndWait(t)
			released := openSQLStore(t, dsn, schema)
			if _, instance := fetchOwnership(t, admin); instance == newReady.Instance {
				t.Fatalf("the retired replacement still owns the deployment: %q", instance)
			}
			_ = released
		})
	}
}

// The deployment is one unit: while the old instance is still alive, a second
// writer is refused; after it really exited, the replacement process recovers
// through the reviewed verification and an explicit send only.
func TestS3cSecondWriterRefusedWhileOldStoreLives(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedEnterpriseRoom(t, dsn, schema)
	birthCounts := rowCounts3c(t, admin, "room-1")
	proxy := sqltest.StartProxy(t, "break-on-commit")
	request := s3cCommand{Op: "send", Deployment: "dep-test", UserID: "u-owner", Room: "room-1", ClientID: "s3c-1", To: "slot2", Text: "rebuild body"}

	oldChild := startS3cChild(t, proxy.DSN(dsn), dsn, schema, "dep-test")
	if ready := oldChild.next(t); !ready.Ready {
		t.Fatalf("old child ready: %+v", ready)
	}
	proxy.Arm()
	oldChild.command(t, request)
	if lost := oldChild.next(t); lost.Outcome != relay.EnterpriseOutcomeUnknown {
		t.Fatalf("old child outcome: %+v", lost)
	}
	before := rowCounts3c(t, admin, "room-1")
	if _, err := sqlstore.Open(context.Background(), sqlstore.Config{DSN: dsn, Schema: schema, DeploymentID: "dep-test"}); err == nil || !strings.Contains(err.Error(), "live lease holder") {
		t.Fatalf("a second writer was not refused while the old store lived: %v", err)
	}
	if got := rowCounts3c(t, admin, "room-1"); got != before {
		t.Fatalf("the refused second writer changed rows: %v -> %v", before, got)
	}
	oldChild.exitAndWait(t)

	replacement := startS3cChild(t, dsn, dsn, schema, "dep-test")
	newReady := replacement.next(t)
	if !newReady.Ready {
		t.Fatalf("replacement ready: %+v", newReady)
	}
	verifyCommand := request
	verifyCommand.Op = "resolve"
	replacement.command(t, verifyCommand)
	verified := replacement.next(t)
	if verified.Outcome != relay.EnterpriseOutcomeNotCommitted || verified.Commits != 0 {
		t.Fatalf("recovery verification: %+v", verified)
	}
	replacement.command(t, request)
	accepted := replacement.next(t)
	if accepted.Outcome != s3cOutcomeAccepted {
		t.Fatalf("recovery send: %+v", accepted)
	}
	after := rowCounts3c(t, admin, "room-1")
	if after != [3]int64{birthCounts[0] + 1, birthCounts[1] + 1, birthCounts[2] + 1} {
		t.Fatalf("recovery rows: %v", after)
	}
	replacement.exitAndWait(t)
}

// Object-level instance rebuild (in-process): this only simulates an instance
// rebuild — close every engine and the store, then reopen — and is explicitly
// not process or ownership recovery. It is what proves the remaining
// object-level claims: a retired engine refuses with zero further port calls,
// an old fatal is never cleared by the rebuild, and the new instance can
// verify — a re-send is never made here, and the canonical-result claim is the
// process-level test's job.
func TestS3cObjectLevelInstanceRebuildIsSimulationOnly(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedEnterpriseRoom(t, dsn, schema)
	birthCounts := rowCounts3c(t, admin, "room-1")
	proxy := sqltest.StartProxy(t, "drop-response")
	ctx := context.Background()
	auth := executeAuth("u-owner")

	dbStore := openSQLStore(t, proxy.DSN(dsn), schema)
	counting := &countingPort{inner: sqlstore.NewRoomPort(dbStore)}
	oldEngine, err := relay.OpenEnterprise(ctx, relay.Config{RoomID: "room-1"}, counting, auth)
	if err != nil {
		t.Fatal(err)
	}
	proxy.Arm()
	if _, err := oldEngine.ExecuteHumanSend(ctx, auth, relay.SendRequest{ID: "obj-1", Text: "one", To: model.ActorSlot2}); err == nil {
		t.Fatal("the lost outcome reported success")
	}
	fatalBefore := oldEngine.Fatal()
	if fatalBefore == nil {
		t.Fatal("the lost outcome did not fail the old engine closed")
	}

	freshStore := deployRebuild(t, dsn, schema, []*relay.Engine{oldEngine}, dbStore)
	rowsBefore := rowCounts3c(t, admin, "room-1")
	if rowsBefore != [3]int64{birthCounts[0] + 1, birthCounts[1] + 1, birthCounts[2] + 1} {
		t.Fatalf("the rebuild changed the accepted fact rows: %v", rowsBefore)
	}
	commits, resolves := counting.commits.Load(), counting.resolves.Load()
	if _, err := oldEngine.ExecuteHumanSend(ctx, auth, relay.SendRequest{ID: "obj-2", Text: "after", To: model.ActorSlot2}); err == nil {
		t.Fatal("the retired engine accepted a command")
	}
	if counting.commits.Load() != commits || counting.resolves.Load() != resolves {
		t.Fatal("the retired engine still called the port")
	}
	if oldEngine.Fatal() == nil || oldEngine.Fatal().Error() != fatalBefore.Error() {
		t.Fatalf("the rebuild cleared the old engine's fatal: %v -> %v", fatalBefore, oldEngine.Fatal())
	}

	newPort := &countingPort{inner: sqlstore.NewRoomPort(freshStore)}
	newEngine, err := relay.OpenEnterprise(ctx, relay.Config{RoomID: "room-1"}, newPort, auth)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = newEngine.Close() }()
	if got := rowCounts3c(t, admin, "room-1"); got != rowsBefore {
		t.Fatalf("the rebuilt instance's open wrote state: %v -> %v", rowsBefore, got)
	}
	verified, err := newPort.ResolveHumanSend(ctx, auth, "room-1", "obj-1", relay.EnterpriseSendIntent{To: "slot2", Text: "one"})
	if err != nil || verified.Outcome != relay.EnterpriseOutcomeReplayed {
		t.Fatalf("the rebuilt instance could not verify the pending outcome: %+v %v", verified, err)
	}
	if newPort.commits.Load() != 0 {
		t.Fatal("the rebuilt instance verified with a command it should never have sent")
	}
	if got := rowCounts3c(t, admin, "room-1"); got != rowsBefore {
		t.Fatalf("the verification wrote state: %v -> %v", rowsBefore, got)
	}
}

// A verification target that cannot answer is synthetic here: the stub returns
// a typed unknown without touching PostgreSQL, so this test claims nothing
// about a real database failure — it only proves that an unverifiable outcome
// sends nothing and stays unresolved.
func TestS3cSyntheticUnverifiableVerificationSendsNothing(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedEnterpriseRoom(t, dsn, schema)
	before := rowCounts3c(t, admin, "room-1")

	stub := &syntheticUnverifiablePort{}
	resolved, err := stub.ResolveHumanSend(context.Background(), executeAuth("u-owner"), "room-1", "s3c-1", relay.EnterpriseSendIntent{To: "slot2", Text: "rebuild body"})
	var commandErr *relay.EnterpriseCommandError
	if !errors.As(err, &commandErr) || resolved.Outcome != relay.EnterpriseOutcomeUnknown || commandErr.Class != string(sqlstore.ClassConnectionException) {
		t.Fatalf("synthetic unverifiable verification: %+v %v", resolved, err)
	}
	if got := rowCounts3c(t, admin, "room-1"); got != before {
		t.Fatalf("the synthetic unverifiable verification wrote state: %v -> %v", before, got)
	}
}

// syntheticUnverifiablePort is a test-side stub: it answers no verification and
// performs no SQL. It is NOT a real PostgreSQL failure injection.
type syntheticUnverifiablePort struct{}

func (s *syntheticUnverifiablePort) CommitHumanSend(context.Context, relay.EnterpriseSendCommand) (relay.EnterpriseSendResult, error) {
	return relay.EnterpriseSendResult{}, errors.New("the synthetic stub never commits")
}

func (s *syntheticUnverifiablePort) ResolveHumanSend(context.Context, relay.EnterpriseAuth, string, string, relay.EnterpriseSendIntent) (relay.EnterpriseSendResult, error) {
	return relay.EnterpriseSendResult{Outcome: relay.EnterpriseOutcomeUnknown}, &relay.EnterpriseCommandError{Outcome: relay.EnterpriseOutcomeUnknown, Class: string(sqlstore.ClassConnectionException), Err: errors.New("the synthetic stub cannot verify")}
}

func (s *syntheticUnverifiablePort) ReadonlySnapshot(context.Context, relay.EnterpriseAuth, string) (relay.EnterpriseRoomSnapshot, error) {
	return relay.EnterpriseRoomSnapshot{}, errors.New("the synthetic stub has no snapshot")
}

// Refusals after the real rebuild are verified, never sent: changed intent,
// revoked users and a lost lease leave the rows untouched, the pending outcome
// unresolved, and the replacement process issues zero commands.
func TestS3cRebuildVerificationRefusalsNeverSend(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedEnterpriseRoom(t, dsn, schema)
	proxy := sqltest.StartProxy(t, "drop-response")
	request := s3cCommand{Op: "send", Deployment: "dep-test", UserID: "u-owner", Room: "room-1", ClientID: "s3c-1", To: "slot2", Text: "rebuild body"}

	oldChild := startS3cChild(t, proxy.DSN(dsn), dsn, schema, "dep-test")
	if ready := oldChild.next(t); !ready.Ready {
		t.Fatalf("old child ready: %+v", ready)
	}
	proxy.Arm()
	oldChild.command(t, request)
	if lost := oldChild.next(t); lost.Outcome != relay.EnterpriseOutcomeUnknown {
		t.Fatalf("old child outcome: %+v", lost)
	}
	oldChild.exitAndWait(t)

	replacement := startS3cChild(t, dsn, dsn, schema, "dep-test")
	if ready := replacement.next(t); !ready.Ready {
		t.Fatalf("replacement ready: %+v", ready)
	}
	before := rowCounts3c(t, admin, "room-1")

	changed := request
	changed.Op, changed.Text = "resolve", "different intent"
	replacement.command(t, changed)
	refused := replacement.next(t)
	if refused.Outcome != string(sqlstore.OutcomeConflict) || refused.Class != string(sqlstore.ClassPayloadConflict) || refused.Commits != 0 {
		t.Fatalf("changed-intent verification: %+v", refused)
	}

	if _, err := admin.Exec(context.Background(), `UPDATE users SET disabled = true WHERE user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	replacement.command(t, mustResolve(request))
	refused = replacement.next(t)
	if refused.Class != relay.EnterpriseClassAuthorization || refused.Commits != 0 {
		t.Fatalf("revoked verification: %+v", refused)
	}
	if _, err := admin.Exec(context.Background(), `UPDATE users SET disabled = false WHERE user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	if got := rowCounts3c(t, admin, "room-1"); got != before {
		t.Fatalf("refusals wrote or sent: %v -> %v", before, got)
	}

	// A lost lease is sticky: the replacement instance's own lease backend is
	// terminated, and the verification refuses while the pending outcome stays
	// unresolved — it never degrades into an absence or an automatic send.
	var leasePID int32
	if err := admin.QueryRow(context.Background(), `SELECT lease_pid FROM ownership`).Scan(&leasePID); err != nil {
		t.Fatal(err)
	}
	sqltest.KillBackend(t, admin, uint32(leasePID), "replacement lease")
	replacement.command(t, mustResolve(request))
	refused = replacement.next(t)
	if refused.Outcome != string(sqlstore.OutcomeRefused) || refused.Class != string(sqlstore.ClassLeaseLost) || refused.Commits != 0 {
		t.Fatalf("lease-lost verification: %+v", refused)
	}
	if got := rowCounts3c(t, admin, "room-1"); got != before {
		t.Fatalf("the lease-lost refusal wrote or sent: %v -> %v", before, got)
	}
	assertLogContiguous(t, admin, "room-1")
}

// mustResolve turns the original request into its read-only verification
// instruction: same deployment, principal, Room, key, target and text.
func mustResolve(request s3cCommand) s3cCommand {
	request.Op = "resolve"
	return request
}

// Two distinct Rooms share one deployment store. Room-1 loses its commit
// response (real child, real proxy) and the whole deployment — both Rooms — is
// rebuilt by a real replacement process; Room-2 is interrupted too, not
// spared. The new instance serves both: room-1's pending outcome verifies to
// its original canonical fact, and every stored byte and the complete
// authorization state of both Rooms are unchanged.
func TestS3cRebuildInterruptsBothRoomsPreservingHistory(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedEnterpriseRoom(t, dsn, schema)
	birthCounts := rowCounts3c(t, admin, "room-1")
	ctx := context.Background()
	seedStore := openSQLStore(t, dsn, schema)
	sqltest.CreateEnterpriseRoom(t, seedStore, "room-2", "u-owner")
	if err := seedStore.Close(); err != nil {
		t.Fatal(err)
	}
	proxy := sqltest.StartProxy(t, "drop-response")
	request := s3cCommand{Op: "send", Deployment: "dep-test", UserID: "u-owner", Room: "room-1", ClientID: "a-1", To: "slot2", Text: "room one pending"}

	oldChild := startS3cChild(t, proxy.DSN(dsn), dsn, schema, "dep-test")
	oldReady := oldChild.next(t)
	if !oldReady.Ready {
		t.Fatalf("old child ready: %+v", oldReady)
	}

	// Room-2 establishes its history first, through the same shared store.
	oldChild.command(t, s3cCommand{Op: "open", Deployment: "dep-test", UserID: "u-owner", Room: "room-2"})
	if opened := oldChild.next(t); opened.Ops != "open" {
		t.Fatalf("room-2 open: %+v", opened)
	}
	oldChild.command(t, s3cCommand{Op: "send", Deployment: "dep-test", UserID: "u-owner", Room: "room-2", ClientID: "b-1", To: "slot2", Text: "room two history"})
	if accepted := oldChild.next(t); accepted.Outcome != s3cOutcomeAccepted || accepted.Text != "room two history" {
		t.Fatalf("room-2 history: %+v", accepted)
	}
	roomTwoHistory := rowCounts3c(t, admin, "room-2")
	roomTwoHashBefore := eventEncodingsHash(t, admin, "room-2")
	aclBefore := aclSnapshot(t, admin)

	proxy.Arm()
	oldChild.command(t, request)
	lost := oldChild.next(t)
	if lost.Outcome != relay.EnterpriseOutcomeUnknown || lost.Class != string(sqlstore.ClassConnectionException) {
		t.Fatalf("room-1 outcome = %+v, want unknown/connection_exception", lost)
	}
	// Room-1's accepted-then-lost fact is now durable; this is the state the
	// rebuild must leave untouched.
	roomOneBefore := rowCounts3c(t, admin, "room-1")
	if roomOneBefore != [3]int64{birthCounts[0] + 1, birthCounts[1] + 1, birthCounts[2] + 1} {
		t.Fatalf("room-1 accepted-then-lost rows: %v", roomOneBefore)
	}
	roomOneHashBefore := eventEncodingsHash(t, admin, "room-1")
	originalID, _, originalText, _ := storedFactMessage(t, admin, "room-1", birthCounts[0]+1)
	if originalText != request.Text {
		t.Fatal("the accepted fact did not store the complete original text")
	}

	// The deployment is one unit: while the old instance is alive, a second
	// writer is refused for the whole store, not just for room-1.
	if _, err := sqlstore.Open(ctx, sqlstore.Config{DSN: dsn, Schema: schema, DeploymentID: "dep-test"}); err == nil || !strings.Contains(err.Error(), "live lease holder") {
		t.Fatalf("a second writer was not refused while the old store lived: %v", err)
	}
	oldChild.exitAndWait(t)

	// The replacement process takes the whole deployment over.
	replacement := startS3cChild(t, dsn, dsn, schema, "dep-test")
	newReady := replacement.next(t)
	if !newReady.Ready || newReady.PID == oldReady.PID || newReady.Instance == oldReady.Instance || newReady.Epoch != oldReady.Epoch+1 {
		t.Fatalf("replacement ownership: %+v (old %+v)", newReady, oldReady)
	}

	// Room-2 was interrupted by the same rebuild — not spared — and its stored
	// bytes and authorization state are unchanged, as are room-1's.
	if got := rowCounts3c(t, admin, "room-1"); got != roomOneBefore {
		t.Fatalf("room-1 rows changed across the rebuild: %v -> %v", roomOneBefore, got)
	}
	if got := rowCounts3c(t, admin, "room-2"); got != roomTwoHistory {
		t.Fatalf("room-2 history changed across the rebuild: %v -> %v", roomTwoHistory, got)
	}
	if got := eventEncodingsHash(t, admin, "room-1"); got != roomOneHashBefore {
		t.Fatal("room-1 stored bytes changed across the rebuild")
	}
	if got := eventEncodingsHash(t, admin, "room-2"); got != roomTwoHashBefore {
		t.Fatal("room-2 stored bytes changed across the rebuild")
	}
	if got := aclSnapshot(t, admin); !equalStrings(got, aclBefore) {
		t.Fatalf("authorization state changed across the rebuild: %v -> %v", aclBefore, got)
	}
	assertLogContiguous(t, admin, "room-1")
	assertLogContiguous(t, admin, "room-2")

	// Room-2's ACL still enforces on the new instance: a non-member is refused.
	nonMember := s3cCommand{Op: "resolve", Deployment: "dep-test", UserID: "u-viewer", Room: "room-2", ClientID: "b-1", To: "slot2", Text: "room two history"}
	replacement.command(t, nonMember)
	if refused := replacement.next(t); refused.Class != relay.EnterpriseClassAuthorization || refused.Commits != 0 {
		t.Fatalf("non-member verification: %+v", refused)
	}

	// Room-1: verify with the original key and intent, then the separate
	// explicit same-key instruction — which returns the original canonical
	// fact and adds nothing.
	replacement.command(t, mustResolve(request))
	if verified := replacement.next(t); verified.Outcome != relay.EnterpriseOutcomeReplayed || verified.Commits != 0 {
		t.Fatalf("room-1 verification: %+v", verified)
	}
	replacement.command(t, s3cCommand{Op: "open", Deployment: "dep-test", UserID: "u-owner", Room: "room-1"})
	if opened := replacement.next(t); opened.Ops != "open" {
		t.Fatalf("room-1 open: %+v", opened)
	}
	replacement.command(t, request)
	sent := replacement.next(t)
	if sent.Outcome != s3cOutcomeAccepted || sent.MessageID != originalID || sent.Text != originalText {
		t.Fatalf("room-1 canonical result: %+v, want %q", sent, originalID)
	}
	if got := rowCounts3c(t, admin, "room-1"); got != roomOneBefore {
		t.Fatalf("room-1 explicit re-send duplicated the accepted fact: %v -> %v", roomOneBefore, got)
	}

	// Room-2 continues on the new instance.
	replacement.command(t, s3cCommand{Op: "open", Deployment: "dep-test", UserID: "u-owner", Room: "room-2"})
	if opened := replacement.next(t); opened.Ops != "open" {
		t.Fatalf("room-2 reopen: %+v", opened)
	}
	replacement.command(t, s3cCommand{Op: "send", Deployment: "dep-test", UserID: "u-owner", Room: "room-2", ClientID: "b-3", To: "slot2", Text: "continued"})
	if accepted := replacement.next(t); accepted.Outcome != s3cOutcomeAccepted || accepted.Room != "room-2" {
		t.Fatalf("room-2 continuation: %+v", accepted)
	}
	after := rowCounts3c(t, admin, "room-2")
	if after[0] != roomTwoHistory[0]+1 || after[1] != roomTwoHistory[1]+1 || after[2] != roomTwoHistory[2]+1 {
		t.Fatalf("room-2 continuation appended %v", after)
	}
	assertLogContiguous(t, admin, "room-2")
	replacement.exitAndWait(t)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
