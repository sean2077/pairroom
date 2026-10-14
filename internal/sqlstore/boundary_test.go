package sqlstore

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// pgProxy is a test-only TCP proxy in front of PostgreSQL. After Arm it
// injects one of the two real response-loss boundaries the contract
// distinguishes, per connection: a forwarded COMMIT whose response is dropped
// (the server accepted the commit; the client cannot know), or a connection
// break before COMMIT reaches the server (the server rolls back; the client
// still cannot know). It is a protocol-level boundary, not a production hook.
type pgProxy struct {
	t        *testing.T
	listener net.Listener
	upstream string
	mode     string
	armed    atomic.Bool
	mu       sync.Mutex
	conns    []net.Conn
}

func startProxy(t *testing.T, mode string) *pgProxy {
	t.Helper()
	conf, err := pgx.ParseConfig(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &pgProxy{t: t, listener: listener, upstream: net.JoinHostPort(conf.Host, strconv.Itoa(int(conf.Port))), mode: mode}
	go proxy.serve()
	t.Cleanup(func() {
		_ = listener.Close()
		proxy.mu.Lock()
		defer proxy.mu.Unlock()
		for _, conn := range proxy.conns {
			_ = conn.Close()
		}
	})
	return proxy
}

// dsn rewrites a direct DSN through the proxy.
func (p *pgProxy) dsn(base string) string {
	conf, err := pgx.ParseConfig(base)
	if err != nil {
		p.t.Fatal(err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		url.QueryEscape(conf.User), url.QueryEscape(conf.Password), p.listener.Addr().String(), conf.Database)
}

// arm enables injection; before that the proxy is a plain pass-through so the
// Store can open (its lease connection also commits its ownership row).
func (p *pgProxy) arm() { p.armed.Store(true) }

func (p *pgProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.conns = append(p.conns, client)
		p.mu.Unlock()
		go p.handle(client)
	}
}

func (p *pgProxy) handle(client net.Conn) {
	server, err := net.Dial("tcp", p.upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	closeBoth := func() {
		_ = client.Close()
		_ = server.Close()
	}
	// Injection is gated per chunk on Arm(), so connections opened during the
	// fixture setup (before arming) become injectable mid-stream.
	var seenMu sync.Mutex
	commitSeen := false
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 4096)
		for {
			n, err := client.Read(buf)
			if n > 0 {
				armed := p.armed.Load()
				seenMu.Lock()
				// The real COMMIT is a simple-query frame ending "commit\x00";
				// matching bare "commit" would also hit "read committed" in the
				// BEGIN frame.
				marked := armed && !commitSeen && bytes.Contains(bytes.ToLower(buf[:n]), []byte("commit\x00"))
				if marked {
					commitSeen = true
				}
				seenMu.Unlock()
				if marked && p.mode == "break-on-commit" {
					closeBoth()
					return
				}
				if _, werr := server.Write(buf[:n]); werr != nil {
					closeBoth()
					return
				}
			}
			if err != nil {
				closeBoth()
				return
			}
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 4096)
		for {
			n, err := server.Read(buf)
			if n > 0 {
				seenMu.Lock()
				seen := commitSeen
				seenMu.Unlock()
				if p.armed.Load() && p.mode == "drop-response" && seen {
					closeBoth()
					return
				}
				if _, werr := client.Write(buf[:n]); werr != nil {
					closeBoth()
					return
				}
			}
			if err != nil {
				closeBoth()
				return
			}
		}
	}()
	<-done
	closeBoth()
}

// The accepted-but-lost boundary: the server committed, the client cannot
// know, the kernel reports Unknown — and only full verification recovers the
// original accepted result.
func TestCommitAcceptedButResponseLostRecoversAsReplayed(t *testing.T) {
	proxy := startProxy(t, "drop-response")
	f := newFixtureProxy(t, func(dsn string) string { return proxy.dsn(dsn) })
	ctx := context.Background()
	cmd := f.command("proxy-accepted")
	proxy.arm()

	res, err := f.store.Commit(ctx, cmd)
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeUnknown {
		t.Fatalf("lost response must be unknown: %+v", res)
	}
	resolved, err := f.store.Resolve(ctx, cmd.Auth, cmd.Key, cmd.Mutation)
	if err != nil || resolved.Outcome != OutcomeReplayed || resolved.Seq != 1 || resolved.Revision != 1 || resolved.Fact.ID != cmd.Mutation.Fact.ID {
		t.Fatalf("resolve after accepted response loss: %+v %v", resolved, err)
	}
	if events, operations, _, currentSeq, revision := f.roomCounts(t); events != 1 || operations != 1 || currentSeq != 1 || revision != 1 {
		t.Fatalf("exactly one append expected: events=%d operations=%d seq=%d revision=%d", events, operations, currentSeq, revision)
	}
}

// The broken-before-server boundary: the server never saw COMMIT, the client
// still cannot know, and verification proves the absence.
func TestCommitBrokenBeforeServerIsUnknownThenAbsent(t *testing.T) {
	proxy := startProxy(t, "break-on-commit")
	f := newFixtureProxy(t, func(dsn string) string { return proxy.dsn(dsn) })
	ctx := context.Background()
	cmd := f.command("proxy-broken")
	proxy.arm()

	res, err := f.store.Commit(ctx, cmd)
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeUnknown {
		t.Fatalf("broken commit must be unknown: %+v", res)
	}
	resolved, err := f.store.Resolve(ctx, cmd.Auth, cmd.Key, cmd.Mutation)
	if err != nil || resolved.Outcome != OutcomeNotCommitted {
		t.Fatalf("resolve must prove the absence: %+v %v", resolved, err)
	}
	if events, _, _, _, _ := f.roomCounts(t); events != 0 {
		t.Fatalf("broken commit wrote %d events", events)
	}
}

// killOnQueryTracer kills the traced backend the first time an armed query
// matches — a driver-level boundary, no production seam involved. The gate
// keeps setup operations unaffected.
type killOnQueryTracer struct {
	t      *testing.T
	admin  *pgx.Conn
	target atomic.Uint32
	gate   atomic.Bool
	killed atomic.Bool
	match  func(sql string) bool
}

func (k *killOnQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if k.gate.Load() && !k.killed.Load() && k.match(data.SQL) {
		k.killed.Store(true)
		var ok bool
		if err := k.admin.QueryRow(context.Background(), `SELECT pg_catalog.pg_terminate_backend($1, 5000)`, int32(k.target.Load())).Scan(&ok); err != nil || !ok {
			k.t.Errorf("tracer kill did not confirm: ok=%v err=%v", ok, err)
		}
	}
	return ctx
}

func (k *killOnQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// flipMembershipTracer toggles the owner membership before every dependency
// read, so the discovery reads and the locked reads always disagree and the
// bounded restart limit is reached deterministically.
type flipMembershipTracer struct {
	t     *testing.T
	admin *pgx.Conn
	table string
	flip  atomic.Bool
}

func (f *flipMembershipTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "SELECT disabled FROM") {
		next := !f.flip.Load()
		f.flip.Store(next)
		if _, err := f.admin.Exec(context.Background(), `UPDATE `+f.table+` SET disabled = $1 WHERE room_id = 'room-1' AND user_id = 'u-owner'`, next); err != nil {
			f.t.Errorf("flip membership: %v", err)
		}
	}
	return ctx
}

func (f *flipMembershipTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// noopTracer keeps openStoreWithTracer usable without a tracer (pgx accepts a
// tracer only as an interface value).
type noopTracer struct{}

func (noopTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (noopTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// openStoreWithTracer composes a Store over a raw fixture with a driver
// tracer on the work connection; it mirrors Open without any production seam.
func openStoreWithTracer(t *testing.T, f *fixture, tracer pgx.QueryTracer) *Store {
	t.Helper()
	if tracer == nil {
		tracer = noopTracer{}
	}
	ctx := context.Background()
	conf, err := pgx.ParseConfig(f.cfg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	conf.Tracer = tracer
	work, err := pgx.ConnectConfig(ctx, conf)
	if err != nil {
		t.Fatal(err)
	}
	if err := configureSession(ctx, work, f.schema); err != nil {
		t.Fatal(err)
	}
	lease, err := openLease(ctx, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{cfg: f.cfg, lease: lease, work: work, sem: make(chan struct{}, 1)}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// The server reaps a stalled kernel transaction without any cooperation from
// the wedged process: after the lease backend is killed and nobody rolls back
// or closes the stalled work connection, the successor waits only until the
// server's idle-in-transaction timeout terminates the old work backend and
// releases its locks, then takes over with a new epoch. Nothing in this test
// rolls the stalled transaction back or closes its connection.
//
// The production idle timeout is 15s (asserted by
// TestSessionBudgetsAreConfigured); this work connection shrinks its own
// budget to 1s as an explicit fixture override so the regression stays fast.
func TestStalledTransactionTakeoverWithoutRollback(t *testing.T) {
	f := newRawFixture(t)
	ctx := context.Background()
	store := openStoreWithTracer(t, f, nil)
	if _, err := store.work.Exec(ctx, `SELECT pg_catalog.set_config('idle_in_transaction_session_timeout', '1s', false)`); err != nil {
		t.Fatal(err)
	}
	tx, err := store.work.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT epoch, lease_pid, lease_backend_start FROM `+store.table("ownership")+` WHERE id FOR SHARE`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT current_seq, revision FROM `+store.table("rooms")+` WHERE room_id = 'room-1' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	f.terminate(t, store.lease.pid, "lease")

	start := time.Now()
	successor, err := Open(ctx, f.cfg)
	if err != nil {
		t.Fatalf("successor did not take over after the server reaped the stalled transaction: %v", err)
	}
	defer func() { _ = successor.Close() }()
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Fatalf("takeover was not server-bounded: %v", elapsed)
	}
	var epoch int64
	if err := successor.work.QueryRow(ctx, `SELECT epoch FROM `+successor.table("ownership")).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if epoch != 2 {
		t.Fatalf("takeover epoch = %d, want 2", epoch)
	}
	// The old work connection was reaped by the server, not by this test.
	var one int
	if err := store.work.QueryRow(ctx, `SELECT 1`).Scan(&one); err == nil {
		t.Fatal("the stalled work connection still answered after the idle timeout")
	}
}

// A dependency change arriving while the kernel waits on the lock: the
// discovery reads are stale by the time the locks are held, the pure
// discovery phase restarts, and the fresh state refuses the plan.
func TestDiscoveryRestartOnConcurrentDependencyChange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	conn2 := schemaConn(t, f.dsn, f.schema)
	tx2, err := conn2.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Exec(ctx, `SELECT role FROM `+f.store.table("room_members")+` WHERE room_id = 'room-1' AND user_id = 'u-owner' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan Result, 1)
	var rerr error
	cmd := f.command("restart-live")
	go func() {
		res, err := f.store.Commit(ctx, cmd)
		rerr = err
		done <- res
	}()
	waitForLockWait(t, f.admin, f.store.work.PgConn().PID())
	if _, err := tx2.Exec(ctx, `UPDATE `+f.store.table("room_members")+` SET disabled = true WHERE room_id = 'room-1' AND user_id = 'u-owner'`); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if kerr := kernelErr(t, res, rerr); kerr.Outcome != OutcomeRefused || kerr.Class != ClassAuthorization {
		t.Fatalf("restart then refusal: %+v", res)
	}
	if events, _, _, _, _ := f.roomCounts(t); events != 0 {
		t.Fatalf("restart produced %d events", events)
	}
}

// The restart limit is reached when every discovery read and locked read
// disagree; the driver tracer supplies those real query boundaries.
func TestDiscoveryRestartLimitWithDriverTracer(t *testing.T) {
	f := newRawFixture(t)
	ctx := context.Background()
	tracer := &flipMembershipTracer{t: t, admin: f.admin, table: qualified(f.schema, "room_members")}
	store := openStoreWithTracer(t, f, tracer)
	res, err := store.Commit(ctx, f.command("restart-limit"))
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeNotCommitted || kerr.Class != ClassDiscoveryRestart {
		t.Fatalf("restart limit: %+v", res)
	}
}

// Resolve must wait on the same guard while an in-flight transaction holds
// it, and must not answer Absent before that transaction is resolved.
func TestResolveWaitsForInFlightGuard(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	conn2 := schemaConn(t, f.dsn, f.schema)
	tx2, err := conn2.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Exec(ctx, `SELECT current_seq, revision FROM `+f.store.table("rooms")+` WHERE room_id = 'room-1' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	cmd := f.command("inflight-1")
	done := make(chan Result, 1)
	var rerr error
	go func() {
		res, err := f.store.Resolve(ctx, cmd.Auth, cmd.Key, cmd.Mutation)
		rerr = err
		done <- res
	}()
	// The barrier: the resolver is observably waiting on the lock, so it has
	// not answered while the transaction is unresolved.
	waitForLockWait(t, f.admin, f.store.work.PgConn().PID())
	if err := tx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if rerr != nil || res.Outcome != OutcomeNotCommitted {
		t.Fatalf("resolve after the in-flight guard settled: %+v %v", res, rerr)
	}
}

// A cancellation while the resolver waits on a held guard is an unknown
// outcome — the check never completed, so no absence may be claimed.
func TestResolveCancellationWhileWaitingIsUnknown(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	conn2 := schemaConn(t, f.dsn, f.schema)
	tx2, err := conn2.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Exec(ctx, `SELECT current_seq, revision FROM `+f.store.table("rooms")+` WHERE room_id = 'room-1' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	cmd := f.command("cancel-wait")
	rctx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()
	res, err := f.store.Resolve(rctx, cmd.Auth, cmd.Key, cmd.Mutation)
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeUnknown || kerr.Class != ClassCanceled {
		t.Fatalf("cancelled resolve: %+v", res)
	}
	if err := tx2.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

// The bounded replacement connection recovers the stored outcome when the
// work connection dies at the lookup.
func TestResolveReplacementConnRecoversAfterLookupFailure(t *testing.T) {
	f := newRawFixture(t)
	ctx := context.Background()
	tracer := &killOnQueryTracer{t: t, admin: f.admin, match: func(sql string) bool { return strings.Contains(sql, "intent_hash") }}
	store := openStoreWithTracer(t, f, tracer)
	tracer.target.Store(store.work.PgConn().PID())
	cmd := f.command("replacement-1")
	if res, err := store.Commit(ctx, cmd); err != nil || res.Outcome != OutcomeCommitted {
		t.Fatalf("setup commit: %+v %v", res, err)
	}
	tracer.gate.Store(true)
	res, err := store.Resolve(ctx, cmd.Auth, cmd.Key, cmd.Mutation)
	if err != nil || res.Outcome != OutcomeReplayed || res.Fact.ID != cmd.Mutation.Fact.ID {
		t.Fatalf("replacement verification: %+v %v", res, err)
	}
}

// When the replacement connection cannot verify either, the outcome stays
// unknown — never a guessed absence.
func TestResolveUnknownWhenReplacementCannotVerify(t *testing.T) {
	f := newRawFixture(t)
	ctx := context.Background()
	tracer := &killOnQueryTracer{t: t, admin: f.admin, match: func(sql string) bool { return strings.Contains(sql, "intent_hash") }}
	store := openStoreWithTracer(t, f, tracer)
	tracer.target.Store(store.work.PgConn().PID())
	cmd := f.command("replacement-2")
	if res, err := store.Commit(ctx, cmd); err != nil || res.Outcome != OutcomeCommitted {
		t.Fatalf("setup commit: %+v %v", res, err)
	}
	// The replacement target is unreachable; only configuration data changes.
	store.cfg.DSN = "postgres://postgres@127.0.0.1:1/pairroom_test?sslmode=disable&connect_timeout=1"
	tracer.gate.Store(true)
	res, err := store.Resolve(ctx, cmd.Auth, cmd.Key, cmd.Mutation)
	if kerr := kernelErr(t, res, err); kerr.Outcome != OutcomeUnknown {
		t.Fatalf("unverifiable resolve: %+v", res)
	}
}

// Every work and lease connection carries the bounded server-side session
// budgets, so a process wedged between statements cannot pin locks forever.
func TestSessionBudgetsAreConfigured(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	settings := map[string]string{
		"idle_in_transaction_session_timeout": sessionIdleInTransactionTimeout,
		"transaction_timeout":                 sessionTransactionTimeout,
		"lock_timeout":                        sessionLockTimeout,
		"statement_timeout":                   sessionStatementTimeout,
	}
	for name, conn := range map[string]*pgx.Conn{"work": f.store.work, "lease": f.store.lease.conn} {
		for setting, want := range settings {
			var got string
			if err := conn.QueryRow(ctx, `SELECT pg_catalog.current_setting($1)`, setting).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("%s %s = %q, want %q", name, setting, got, want)
			}
		}
	}
}

// A stalled in-flight transaction holds the ownership and Room locks; a
// successor Open fails bounded instead of waiting forever, and takes over
// cleanly once the stall is resolved.
func TestStalledTransactionTakeoverIsBounded(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	tx, err := f.store.work.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT epoch, lease_pid, lease_backend_start FROM `+f.store.table("ownership")+` WHERE id FOR SHARE`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT current_seq, revision FROM `+f.store.table("rooms")+` WHERE room_id = 'room-1' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	f.terminate(t, f.store.lease.pid, "lease")

	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	start := time.Now()
	if _, err := Open(sctx, f.cfg); err == nil {
		t.Fatal("successor took over while the stalled transaction still held the locks")
	}
	cancel()
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Fatalf("successor Open was not bounded: %v", elapsed)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	successor, err := Open(ctx, f.cfg)
	if err != nil {
		t.Fatalf("takeover after the stall released: %v", err)
	}
	defer func() { _ = successor.Close() }()
	var epoch int64
	if err := successor.work.QueryRow(ctx, `SELECT epoch FROM `+successor.table("ownership")).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if epoch != 2 {
		t.Fatalf("takeover epoch = %d, want 2", epoch)
	}
}
