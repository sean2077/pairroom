package relay_test

import (
	"bufio"
	"bytes"
	"context"
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
// The whole deployment is then rebuilt as a new instance and epoch through the
// existing reviewed APIs (sqlstore.Open, sqlstore.RoomPort, relay.OpenEnterprise
// — never relay.Open, which reaps), the pending outcome is verified read-only
// before anything is sent again, and a re-send happens only as a separate
// explicit instruction carrying the same key and the same intent.
//
// Nothing here claims server-side persistence of a pending outcome, recovery
// from an arbitrary crash, a durable caller outbox, production service
// orchestration, restored logins, Bindings, collectors or HA. Production
// behavior is untouched; there is no new production API, control or hook.

// s3cAutoSendDemonstration is the test-side flaw injection for the
// no-auto-send sensitivity experiment: when true, the caller sends the
// original request once BEFORE any verification, which the row-count
// assertions must reject with red wherever a duplicate effect is possible. It
// is never enabled in normal runs; the experiment is run by flipping it, and
// the red and green runs are recorded as evidence.
const s3cAutoSendDemonstration = false

const (
	s3cHelperEnv    = "PAIRROOM_S3C_HELPER"
	s3cProxyDSNEnv  = "PAIRROOM_S3C_PROXY_DSN"
	s3cDirectDSNEnv = "PAIRROOM_S3C_DIRECT_DSN"
	s3cSchemaEnv    = "PAIRROOM_S3C_SCHEMA"
	s3cRoomsEnv     = "PAIRROOM_S3C_ROOMS"
)

// s3cOutcomeAccepted is the child's report for a human send its engine
// accepted; the committed/replayed distinction is asserted parent-side through
// the read-only verification, not guessed in the child.
const s3cOutcomeAccepted = "accepted"

// s3cCommand is one instruction from the parent; the full original request
// travels this channel, never a credential.
type s3cCommand struct {
	Op   string `json:"op"`
	Room string `json:"room,omitempty"`
	ID   string `json:"id,omitempty"`
	Text string `json:"text,omitempty"`
}

// s3cMessage is one report from the child over stdout.
type s3cMessage struct {
	Ready     bool     `json:"ready,omitempty"`
	Rooms     []string `json:"rooms,omitempty"`
	PID       int      `json:"pid,omitempty"`
	Epoch     int64    `json:"epoch,omitempty"`
	Instance  string   `json:"instance,omitempty"`
	Outcome   string   `json:"outcome,omitempty"`
	Class     string   `json:"class,omitempty"`
	MessageID string   `json:"message_id,omitempty"`
	Text      string   `json:"text,omitempty"`
	Error     string   `json:"error,omitempty"`
	Exited    bool     `json:"exited,omitempty"`
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

// runS3cHelperProcess is one service process holding one Store and one
// enterprise engine per Room. The DSNs reach it only through private
// environment variables; every report carries identities and outcomes, never
// credentials.
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
	dbStore, err := sqlstore.Open(ctx, sqlstore.Config{DSN: os.Getenv(s3cProxyDSNEnv), Schema: schema, DeploymentID: "dep-test"})
	if err != nil {
		fail(err)
	}
	port := sqlstore.NewRoomPort(dbStore)
	engines := map[string]*relay.Engine{}
	rooms := []string{}
	for _, room := range strings.Split(os.Getenv(s3cRoomsEnv), ",") {
		room = strings.TrimSpace(room)
		if room == "" {
			continue
		}
		engine, err := relay.OpenEnterprise(ctx, relay.Config{RoomID: room, Runtimes: roomRuntimes()}, port, executeAuth("u-owner"))
		if err != nil {
			fail(err)
		}
		engines[room] = engine
		rooms = append(rooms, room)
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
	report(s3cMessage{Ready: true, Rooms: rooms, PID: os.Getpid(), Epoch: epoch, Instance: instance})

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var command s3cCommand
		if err := json.Unmarshal([]byte(line), &command); err != nil {
			fail(err)
		}
		switch command.Op {
		case "send":
			engine, ok := engines[command.Room]
			if !ok {
				fail(fmt.Errorf("unknown room %q", command.Room))
			}
			message, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: command.ID, Text: command.Text, To: model.ActorSlot2})
			if err == nil {
				report(s3cMessage{Outcome: s3cOutcomeAccepted, MessageID: message.ID, Text: message.Text})
				continue
			}
			var commandErr *relay.EnterpriseCommandError
			if errors.As(err, &commandErr) {
				report(s3cMessage{Outcome: commandErr.Outcome, Class: commandErr.Class})
				continue
			}
			report(s3cMessage{Outcome: "error", Error: err.Error()})
		case "exit":
			for _, engine := range engines {
				_ = engine.Close()
			}
			if err := dbStore.Close(); err != nil {
				fail(err)
			}
			report(s3cMessage{Exited: true})
			return
		default:
			fail(fmt.Errorf("unknown command %q", command.Op))
		}
	}
}

type s3cChild struct {
	cmd    *exec.Cmd
	stdin  *bufio.Writer
	lines  chan s3cMessage
	stderr *bytes.Buffer
}

// startS3cChild re-executes this test binary as the failing service process
// holding the given Rooms.
func startS3cChild(t *testing.T, proxyDSN, directDSN, schema, rooms string) *s3cChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestS3cHelperProcess$")
	cmd.Env = append(os.Environ(),
		s3cHelperEnv+"=1",
		s3cProxyDSNEnv+"="+proxyDSN,
		s3cDirectDSNEnv+"="+directDSN,
		s3cSchemaEnv+"="+schema,
		s3cRoomsEnv+"="+rooms,
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
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "{") {
				continue
			}
			var message s3cMessage
			if err := json.Unmarshal([]byte(line), &message); err != nil {
				continue
			}
			child.lines <- message
		}
		close(child.lines)
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
	case <-time.After(30 * time.Second):
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

// storedFactMessage reads the canonical message identity, creation time and
// full text of one stored fact straight from its authoritative encoding. The
// creation time is compared as its encoded RFC 3339 text: a round trip through
// `timestamptz` would round to microsecond precision and could differ by a
// nanosecond from the authoritative value.
func storedFactMessage(t *testing.T, admin *pgx.Conn, room string, seq int64) (string, string, string) {
	t.Helper()
	var id, createdText, text string
	err := admin.QueryRow(context.Background(), `
		SELECT convert_from(event_encoding, 'UTF8')::jsonb -> 'data' ->> 'id',
		       convert_from(event_encoding, 'UTF8')::jsonb -> 'data' ->> 'created_at',
		       convert_from(event_encoding, 'UTF8')::jsonb -> 'data' ->> 'text'
		FROM room_events WHERE room_id = $1 AND seq = $2`, room, seq).Scan(&id, &createdText, &text)
	if err != nil {
		t.Fatal(err)
	}
	return id, createdText, text
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

// membershipRows snapshots the authorization rows the rebuild must not touch.
func membershipRows(t *testing.T, admin *pgx.Conn) []string {
	t.Helper()
	rows, err := admin.Query(context.Background(), `SELECT room_id, user_id, role FROM room_members ORDER BY room_id, user_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var snapshot []string
	for rows.Next() {
		var room, user, role string
		if err := rows.Scan(&room, &user, &role); err != nil {
			t.Fatal(err)
		}
		snapshot = append(snapshot, room+"/"+user+"/"+role)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// deployRebuild performs the controlled deployment rebuild: every old Room
// engine is retired (closed through the enterprise shutdown, which settles
// in-flight commands on the gate) before the shared store is closed, and a
// fresh store — new instance, new epoch — is opened through the existing
// reviewed APIs. The rebuild unit is the whole deployment. It never touches
// relay.Open (which reaps) and never clears an old fatal or a sticky
// lease-lost incarnation.
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

func fetchOwnership(t *testing.T, admin *pgx.Conn) (int64, string) {
	t.Helper()
	var epoch int64
	var instance string
	if err := admin.QueryRow(context.Background(), `SELECT epoch, instance_id FROM ownership WHERE id`).Scan(&epoch, &instance); err != nil {
		t.Fatal(err)
	}
	return epoch, instance
}

// countingPort wraps a real port and counts the calls each retried or refused
// command path makes, so "no port call" claims are measured, not asserted.
type countingPort struct {
	inner     relay.EnterprisePort
	commits   atomic.Int64
	resolves  atomic.Int64
	snapshots atomic.Int64
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
	c.snapshots.Add(1)
	return c.inner.ReadonlySnapshot(ctx, auth, roomID)
}

// The full process-level rebuild, both Unknown shapes through a real proxy: a
// real child process loses its commit response, reports the Unknown to the
// caller, and exits; the caller keeps the original typed request, rebuilds the
// whole deployment (new instance, new epoch, new PID), verifies the pending
// outcome read-only without sending anything, and re-sends only under a
// separate explicit instruction with the same key and intent.
func TestS3cProcessRebuildBothUnknownShapes(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			schema, _ := sqltest.CreateSchema(t, dsn)
			admin := sqltest.SeedRoom(t, dsn, schema)
			proxy := sqltest.StartProxy(t, mode)
			ctx := context.Background()

			// The caller retains the original typed request — Room, principal,
			// client identity, target and the complete original text — outside
			// the service process, before the first attempt is ever made. The
			// child never stores it; it only receives the instruction it is
			// about to attempt.
			request := relay.SendRequest{ID: "s3c-1", Text: "rebuild body with spaces\nand a second line", To: model.ActorSlot2}
			auth := executeAuth("u-owner")

			child := startS3cChild(t, proxy.DSN(dsn), dsn, schema, "room-1")
			ready := child.next(t)
			if !ready.Ready || ready.Epoch < 1 {
				t.Fatalf("child ready: %+v", ready)
			}
			if ready.PID == 0 || ready.PID == os.Getpid() {
				t.Fatalf("child is not a distinct process: pid=%d", ready.PID)
			}
			if len(ready.Rooms) != 1 || ready.Rooms[0] != "room-1" {
				t.Fatalf("child rooms: %+v", ready.Rooms)
			}

			// Both injected boundaries are a lost commit response: the child
			// reports the typed Unknown and its class, which is what a caller
			// may act on — nothing else.
			proxy.Arm()
			child.command(t, s3cCommand{Op: "send", Room: "room-1", ID: request.ID, Text: request.Text})
			lost := child.next(t)
			if lost.Outcome != relay.EnterpriseOutcomeUnknown || lost.Class != string(sqlstore.ClassConnectionException) {
				t.Fatalf("child outcome = %+v, want unknown/connection_exception", lost)
			}
			child.exitAndWait(t)

			before := rowCounts3c(t, admin, "room-1")
			switch mode {
			case "drop-response":
				// The server accepted the commit before the response was lost.
				if before != [3]int64{1, 1, 1} {
					t.Fatalf("accepted-then-lost rows: %v", before)
				}
			case "break-on-commit":
				// The COMMIT never reached the server: the operation is absent.
				if before != [3]int64{0, 0, 0} {
					t.Fatalf("broken-commit rows: %v", before)
				}
			}
			var originalID, originalCreated string
			if mode == "drop-response" {
				originalID, originalCreated, _ = storedFactMessage(t, admin, "room-1", 1)
			}

			if s3cAutoSendDemonstration {
				// Test-side flaw injection: send before any verification.
				flawed, err := sqlstore.Open(ctx, sqlstore.Config{DSN: dsn, Schema: schema, DeploymentID: "dep-test"})
				if err != nil {
					t.Fatal(err)
				}
				flawedCommand := sqlstore.HumanSendCommand{
					Auth:     sqlstore.AuthContext{Deployment: "dep-test", UserID: "u-owner"},
					RoomID:   "room-1",
					ClientID: request.ID,
					Intent:   sqlstore.HumanSendIntent{To: "slot2", Text: request.Text},
					Fact:     craftHumanSendCandidate("u-owner", request.ID, "slot2", request.Text),
				}
				_, _ = flawed.CommitHumanSend(ctx, flawedCommand)
				_ = flawed.Close()
			}

			freshStore := deployRebuild(t, dsn, schema, nil, nil)
			// The rows assertion comes first so the sensitivity experiment
			// fails on the effect it is about: a send before verification.
			if got := rowCounts3c(t, admin, "room-1"); got != before {
				t.Fatalf("rows changed before verification (rebuild or flaw injection): %v -> %v", before, got)
			}
			newEpoch, newInstance := fetchOwnership(t, admin)
			if newEpoch != ready.Epoch+1 {
				t.Fatalf("rebuild epoch = %d, want %d", newEpoch, ready.Epoch+1)
			}
			if newInstance == ready.Instance {
				t.Fatal("rebuild kept the old instance identity")
			}
			assertLogContiguous(t, admin, "room-1")

			port := sqlstore.NewRoomPort(freshStore)
			resolved, err := port.ResolveHumanSend(ctx, auth, "room-1", request.ID, relay.EnterpriseSendIntent{To: "slot2", Text: request.Text})
			if err != nil {
				t.Fatalf("verification: %v", err)
			}
			if got := rowCounts3c(t, admin, "room-1"); got != before {
				t.Fatalf("verification wrote state: %v -> %v", before, got)
			}

			// The new instance is constructed through the read-only enterprise
			// open: an authenticated snapshot, no reap, no write.
			freshEngine, err := relay.OpenEnterprise(ctx, relay.Config{RoomID: "room-1", Runtimes: roomRuntimes()}, port, auth)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = freshEngine.Close() }()
			if got := rowCounts3c(t, admin, "room-1"); got != before {
				t.Fatalf("engine construction wrote state: %v -> %v", before, got)
			}

			switch mode {
			case "drop-response":
				if resolved.Outcome != relay.EnterpriseOutcomeReplayed {
					t.Fatalf("verify after accepted loss = %q, want replayed", resolved.Outcome)
				}
				// A separate explicit instruction with the same key and intent
				// returns the original canonical result and adds nothing.
				message, err := freshEngine.ExecuteHumanSend(ctx, auth, request)
				if err != nil {
					t.Fatal(err)
				}
				if message.ID != originalID || message.CreatedAt.UTC().Format(time.RFC3339Nano) != originalCreated {
					t.Fatalf("explicit re-send returned %q at %s, want the original %q at %s", message.ID, message.CreatedAt.UTC().Format(time.RFC3339Nano), originalID, originalCreated)
				}
				if message.Text != request.Text {
					t.Fatalf("canonical text = %q, want the full original %q", message.Text, request.Text)
				}
				if got := rowCounts3c(t, admin, "room-1"); got != before {
					t.Fatalf("explicit re-send duplicated the accepted fact: %v -> %v", before, got)
				}
			case "break-on-commit":
				if resolved.Outcome != relay.EnterpriseOutcomeNotCommitted {
					t.Fatalf("verify after broken commit = %q, want not_committed", resolved.Outcome)
				}
				// Only the caller's explicit same-key request may send, and it
				// adds exactly one fact.
				message, err := freshEngine.ExecuteHumanSend(ctx, auth, request)
				if err != nil {
					t.Fatalf("explicit same-key re-send after absence: %v", err)
				}
				after := rowCounts3c(t, admin, "room-1")
				if after[0] != before[0]+1 || after[1] != before[1]+1 || after[2] != before[2]+1 {
					t.Fatalf("explicit re-send did not add exactly one fact/operation/audit: %v -> %v", before, after)
				}
				storedID, _, storedText := storedFactMessage(t, admin, "room-1", 1)
				if message.ID != storedID || storedText != request.Text {
					t.Fatalf("re-sent fact %q/%q, want the explicit request %q/%q", storedID, storedText, message.ID, request.Text)
				}
			}
			assertLogContiguous(t, admin, "room-1")
		})
	}
}

// The deployment is one unit: while the old instance is still alive, a second
// writer is refused; after it really exited, a new writer recovers through the
// reviewed verification and an explicit send only.
func TestS3cSecondWriterRefusedWhileOldStoreLives(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedRoom(t, dsn, schema)
	proxy := sqltest.StartProxy(t, "break-on-commit")
	ctx := context.Background()
	request := relay.SendRequest{ID: "s3c-1", Text: "rebuild body", To: model.ActorSlot2}

	child := startS3cChild(t, proxy.DSN(dsn), dsn, schema, "room-1")
	if ready := child.next(t); !ready.Ready {
		t.Fatalf("child ready: %+v", ready)
	}
	proxy.Arm()
	child.command(t, s3cCommand{Op: "send", Room: "room-1", ID: request.ID, Text: request.Text})
	if lost := child.next(t); lost.Outcome != relay.EnterpriseOutcomeUnknown {
		t.Fatalf("child outcome: %+v", lost)
	}
	before := rowCounts3c(t, admin, "room-1")
	if _, err := sqlstore.Open(ctx, sqlstore.Config{DSN: dsn, Schema: schema, DeploymentID: "dep-test"}); err == nil || !strings.Contains(err.Error(), "live lease holder") {
		t.Fatalf("a second writer was not refused while the old store lived: %v", err)
	}
	if got := rowCounts3c(t, admin, "room-1"); got != before {
		t.Fatalf("the refused second writer changed rows: %v -> %v", before, got)
	}
	child.exitAndWait(t)

	freshStore := deployRebuild(t, dsn, schema, nil, nil)
	port := sqlstore.NewRoomPort(freshStore)
	resolved, err := port.ResolveHumanSend(ctx, executeAuth("u-owner"), "room-1", request.ID, relay.EnterpriseSendIntent{To: "slot2", Text: request.Text})
	if err != nil || resolved.Outcome != relay.EnterpriseOutcomeNotCommitted {
		t.Fatalf("recovery verification: %+v %v", resolved, err)
	}
	engine, err := relay.OpenEnterprise(ctx, relay.Config{RoomID: "room-1", Runtimes: roomRuntimes()}, port, executeAuth("u-owner"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close() }()
	if _, err := engine.ExecuteHumanSend(ctx, executeAuth("u-owner"), request); err != nil {
		t.Fatalf("the new writer could not recover after the old instance exited: %v", err)
	}
	after := rowCounts3c(t, admin, "room-1")
	if after != [3]int64{1, 1, 1} {
		t.Fatalf("recovery rows: %v", after)
	}
}

// Object-level instance rebuild (in-process): this only simulates an instance
// rebuild — close every engine and the store, then reopen — and is explicitly
// not process or ownership recovery. It is what proves the remaining
// object-level claims: a retired engine refuses with zero further port calls,
// an old fatal is never cleared by the rebuild, and the new instance continues.
func TestS3cObjectLevelInstanceRebuildIsSimulationOnly(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedRoom(t, dsn, schema)
	proxy := sqltest.StartProxy(t, "drop-response")
	ctx := context.Background()
	auth := executeAuth("u-owner")

	dbStore := openSQLStore(t, proxy.DSN(dsn), schema)
	counting := &countingPort{inner: sqlstore.NewRoomPort(dbStore)}
	oldEngine, err := relay.OpenEnterprise(ctx, relay.Config{RoomID: "room-1", Runtimes: roomRuntimes()}, counting, auth)
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
	if got := rowCounts3c(t, admin, "room-1"); got != [3]int64{1, 1, 1} {
		t.Fatalf("the rebuild changed the accepted fact rows: %v", got)
	}
	commits, resolves, snapshots := counting.commits.Load(), counting.resolves.Load(), counting.snapshots.Load()
	if _, err := oldEngine.ExecuteHumanSend(ctx, auth, relay.SendRequest{ID: "obj-2", Text: "after", To: model.ActorSlot2}); err == nil {
		t.Fatal("the retired engine accepted a command")
	}
	if counting.commits.Load() != commits || counting.resolves.Load() != resolves || counting.snapshots.Load() != snapshots {
		t.Fatal("the retired engine still called the port")
	}
	if oldEngine.Fatal() == nil || oldEngine.Fatal().Error() != fatalBefore.Error() {
		t.Fatalf("the rebuild cleared the old engine's fatal: %v -> %v", fatalBefore, oldEngine.Fatal())
	}
	newEngine, err := relay.OpenEnterprise(ctx, relay.Config{RoomID: "room-1", Runtimes: roomRuntimes()}, sqlstore.NewRoomPort(freshStore), auth)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = newEngine.Close() }()
	message, err := newEngine.ExecuteHumanSend(ctx, auth, relay.SendRequest{ID: "obj-1", Text: "one", To: model.ActorSlot2})
	if err != nil {
		t.Fatalf("the rebuilt instance could not continue: %v", err)
	}
	storedID, _, _ := storedFactMessage(t, admin, "room-1", 1)
	if message.ID != storedID {
		t.Fatalf("the rebuilt instance returned %q, want the canonical %q", message.ID, storedID)
	}
}

// deadTargetRoomPort forces the verification to be incomplete, standing in
// for a rebuild whose verification target is unreachable.
type deadTargetRoomPort struct {
	inner relay.EnterprisePort
}

func (d *deadTargetRoomPort) CommitHumanSend(ctx context.Context, cmd relay.EnterpriseSendCommand) (relay.EnterpriseSendResult, error) {
	return d.inner.CommitHumanSend(ctx, cmd)
}

func (d *deadTargetRoomPort) ResolveHumanSend(ctx context.Context, auth relay.EnterpriseAuth, roomID, clientID string, intent relay.EnterpriseSendIntent) (relay.EnterpriseSendResult, error) {
	return relay.EnterpriseSendResult{Outcome: relay.EnterpriseOutcomeUnknown}, &relay.EnterpriseCommandError{Outcome: relay.EnterpriseOutcomeUnknown, Class: string(sqlstore.ClassConnectionException), Err: errors.New("injected unverifiable state")}
}

func (d *deadTargetRoomPort) ReadonlySnapshot(ctx context.Context, auth relay.EnterpriseAuth, roomID string) (relay.EnterpriseRoomSnapshot, error) {
	return d.inner.ReadonlySnapshot(ctx, auth, roomID)
}

// Refusals after a rebuild are verified, never sent: changed intent, revoked
// users, unverifiable states and a lost lease leave the rows untouched and the
// pending outcome unresolved.
func TestS3cRebuildVerificationRefusalsNeverSend(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedRoom(t, dsn, schema)
	proxy := sqltest.StartProxy(t, "drop-response")
	ctx := context.Background()

	child := startS3cChild(t, proxy.DSN(dsn), dsn, schema, "room-1")
	if ready := child.next(t); !ready.Ready {
		t.Fatalf("child ready: %+v", ready)
	}
	proxy.Arm()
	child.command(t, s3cCommand{Op: "send", Room: "room-1", ID: "s3c-1", Text: "rebuild body"})
	if lost := child.next(t); lost.Outcome != relay.EnterpriseOutcomeUnknown {
		t.Fatalf("child outcome: %+v", lost)
	}
	child.exitAndWait(t)

	freshStore := deployRebuild(t, dsn, schema, nil, nil)
	port := sqlstore.NewRoomPort(freshStore)
	before := rowCounts3c(t, admin, "room-1")

	resolved, err := port.ResolveHumanSend(ctx, executeAuth("u-owner"), "room-1", "s3c-1", relay.EnterpriseSendIntent{To: "slot2", Text: "different intent"})
	var commandErr *relay.EnterpriseCommandError
	if !errors.As(err, &commandErr) || commandErr.Outcome != string(sqlstore.OutcomeConflict) || commandErr.Class != string(sqlstore.ClassPayloadConflict) {
		t.Fatalf("changed-intent verification: %+v %v", resolved, err)
	}

	if _, err := admin.Exec(ctx, `UPDATE users SET disabled = true WHERE user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	resolved, err = port.ResolveHumanSend(ctx, executeAuth("u-owner"), "room-1", "s3c-1", relay.EnterpriseSendIntent{To: "slot2", Text: "rebuild body"})
	if !errors.As(err, &commandErr) || commandErr.Class != relay.EnterpriseClassAuthorization {
		t.Fatalf("revoked verification: %+v %v", resolved, err)
	}
	if _, err := admin.Exec(ctx, `UPDATE users SET disabled = false WHERE user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}

	dead := &deadTargetRoomPort{inner: port}
	resolved, err = dead.ResolveHumanSend(ctx, executeAuth("u-owner"), "room-1", "s3c-1", relay.EnterpriseSendIntent{To: "slot2", Text: "rebuild body"})
	if err == nil || resolved.Outcome != relay.EnterpriseOutcomeUnknown {
		t.Fatalf("unverifiable verification: %+v %v", resolved, err)
	}
	if got := rowCounts3c(t, admin, "room-1"); got != before {
		t.Fatalf("refusals wrote or sent: %v -> %v", before, got)
	}

	// A lost lease is sticky: the rebuilt instance's own lease backend is
	// terminated, and the verification refuses while the pending outcome stays
	// unresolved — it never degrades into an absence or an automatic send.
	var leasePID int32
	if err := admin.QueryRow(ctx, `SELECT lease_pid FROM ownership`).Scan(&leasePID); err != nil {
		t.Fatal(err)
	}
	sqltest.KillBackend(t, admin, uint32(leasePID), "fresh store lease")
	resolved, err = port.ResolveHumanSend(ctx, executeAuth("u-owner"), "room-1", "s3c-1", relay.EnterpriseSendIntent{To: "slot2", Text: "rebuild body"})
	if !errors.As(err, &commandErr) || commandErr.Outcome != string(sqlstore.OutcomeRefused) || commandErr.Class != string(sqlstore.ClassLeaseLost) {
		t.Fatalf("lease-lost verification: %+v %v", resolved, err)
	}
	if got := rowCounts3c(t, admin, "room-1"); got != before {
		t.Fatalf("the lease-lost refusal wrote or sent: %v -> %v", before, got)
	}
	assertLogContiguous(t, admin, "room-1")
}

// Two distinct Rooms share one deployment store. A loses its commit response
// (real child, real proxy) and the whole deployment — both Rooms — is rebuilt;
// B is interrupted too, not spared, and the new instance serves both: A's
// pending outcome verifies to its original canonical fact, B's history and
// ACLs are intact, and B continues on the new instance.
func TestS3cRebuildInterruptsBothRoomsPreservingHistory(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedRoom(t, dsn, schema)
	ctx := context.Background()
	if _, err := admin.Exec(ctx, `INSERT INTO rooms (room_id, host_mode) VALUES ('room-2', 'native')`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO room_members (room_id, user_id, role) VALUES ('room-2', 'u-owner', 'owner')`); err != nil {
		t.Fatal(err)
	}
	proxy := sqltest.StartProxy(t, "drop-response")
	request := relay.SendRequest{ID: "a-1", Text: "room one pending", To: model.ActorSlot2}

	child := startS3cChild(t, proxy.DSN(dsn), dsn, schema, "room-1,room-2")
	ready := child.next(t)
	if !ready.Ready || len(ready.Rooms) != 2 {
		t.Fatalf("child ready: %+v", ready)
	}

	// B establishes its history first, through the same shared store.
	child.command(t, s3cCommand{Op: "send", Room: "room-2", ID: "b-1", Text: "room two history"})
	if accepted := child.next(t); accepted.Outcome != s3cOutcomeAccepted || accepted.Text != "room two history" {
		t.Fatalf("room-2 history: %+v", accepted)
	}
	roomTwoHistory := rowCounts3c(t, admin, "room-2")
	membersBefore := membershipRows(t, admin)

	proxy.Arm()
	child.command(t, s3cCommand{Op: "send", Room: "room-1", ID: request.ID, Text: request.Text})
	lost := child.next(t)
	if lost.Outcome != relay.EnterpriseOutcomeUnknown || lost.Class != string(sqlstore.ClassConnectionException) {
		t.Fatalf("room-1 outcome = %+v, want unknown/connection_exception", lost)
	}
	roomOneBefore := rowCounts3c(t, admin, "room-1")
	if roomOneBefore != [3]int64{1, 1, 1} {
		t.Fatalf("room-1 accepted-then-lost rows: %v", roomOneBefore)
	}
	originalID, _, originalText := storedFactMessage(t, admin, "room-1", 1)
	if originalText != request.Text {
		t.Fatalf("accepted fact text = %q, want the full original %q", originalText, request.Text)
	}

	// The deployment is one unit: while the old instance is alive, a second
	// writer is refused for the whole store, not just for room-1.
	if _, err := sqlstore.Open(ctx, sqlstore.Config{DSN: dsn, Schema: schema, DeploymentID: "dep-test"}); err == nil || !strings.Contains(err.Error(), "live lease holder") {
		t.Fatalf("a second writer was not refused while the old store lived: %v", err)
	}
	child.exitAndWait(t)

	freshStore := deployRebuild(t, dsn, schema, nil, nil)
	newEpoch, newInstance := fetchOwnership(t, admin)
	if newEpoch != ready.Epoch+1 || newInstance == ready.Instance {
		t.Fatalf("rebuild ownership: epoch %d->%d instance %q->%q", ready.Epoch, newEpoch, ready.Instance, newInstance)
	}
	// B was interrupted by the same rebuild: nothing of its history, ACLs or
	// the pending room-1 fact changed, and nothing was re-sent.
	if got := rowCounts3c(t, admin, "room-1"); got != roomOneBefore {
		t.Fatalf("room-1 rows changed across the rebuild: %v -> %v", roomOneBefore, got)
	}
	if got := rowCounts3c(t, admin, "room-2"); got != roomTwoHistory {
		t.Fatalf("room-2 history changed across the rebuild: %v -> %v", roomTwoHistory, got)
	}
	if got := membershipRows(t, admin); !equalStrings(got, membersBefore) {
		t.Fatalf("membership changed across the rebuild: %v -> %v", membersBefore, got)
	}
	assertLogContiguous(t, admin, "room-1")
	assertLogContiguous(t, admin, "room-2")

	port := sqlstore.NewRoomPort(freshStore)
	// Room-2's ACL still enforces on the new instance: a non-member is refused.
	resolved, err := port.ResolveHumanSend(ctx, executeAuth("u-viewer"), "room-2", "b-1", relay.EnterpriseSendIntent{To: "slot2", Text: "room two history"})
	var commandErr *relay.EnterpriseCommandError
	if !errors.As(err, &commandErr) || commandErr.Class != relay.EnterpriseClassAuthorization {
		t.Fatalf("non-member verification: %+v %v", resolved, err)
	}

	// Room-1: the pending outcome verifies to its original canonical fact, and
	// the explicit same-key instruction adds nothing.
	verified, err := port.ResolveHumanSend(ctx, executeAuth("u-owner"), "room-1", request.ID, relay.EnterpriseSendIntent{To: "slot2", Text: request.Text})
	if err != nil || verified.Outcome != relay.EnterpriseOutcomeReplayed {
		t.Fatalf("room-1 verification: %+v %v", verified, err)
	}
	newA, err := relay.OpenEnterprise(ctx, relay.Config{RoomID: "room-1", Runtimes: roomRuntimes()}, port, executeAuth("u-owner"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = newA.Close() }()
	message, err := newA.ExecuteHumanSend(ctx, executeAuth("u-owner"), request)
	if err != nil {
		t.Fatal(err)
	}
	if message.ID != originalID || message.Text != request.Text {
		t.Fatalf("room-1 canonical result: %+v, want %q", message, originalID)
	}
	if got := rowCounts3c(t, admin, "room-1"); got != roomOneBefore {
		t.Fatalf("room-1 explicit re-send duplicated the accepted fact: %v -> %v", roomOneBefore, got)
	}

	// Room-2 continues on the new instance.
	newB, err := relay.OpenEnterprise(ctx, relay.Config{RoomID: "room-2", Runtimes: roomRuntimes()}, port, executeAuth("u-owner"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = newB.Close() }()
	if _, err := newB.ExecuteHumanSend(ctx, executeAuth("u-owner"), relay.SendRequest{ID: "b-3", Text: "continued", To: model.ActorSlot2}); err != nil {
		t.Fatalf("the rebuilt instance could not continue room-2: %v", err)
	}
	after := rowCounts3c(t, admin, "room-2")
	if after[0] != roomTwoHistory[0]+1 || after[1] != roomTwoHistory[1]+1 || after[2] != roomTwoHistory[2]+1 {
		t.Fatalf("room-2 continuation appended %v", after)
	}
	assertLogContiguous(t, admin, "room-2")
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
