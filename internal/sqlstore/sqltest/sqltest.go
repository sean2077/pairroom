// Package sqltest provides test-only helpers for exercising the SQL kernel
// against an isolated local PostgreSQL. It is imported exclusively from
// *_test.go files of other packages; it must never appear in the dependency
// graph of ./cmd/pairroom (verified with go list -deps as evidence).
package sqltest

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sean2077/pairroom/internal/sqlstore"
)

// RequireDSN returns the isolated PostgreSQL DSN or skips the test; a skip is
// not a pass.
func RequireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("PAIRROOM_SQLSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PAIRROOM_SQLSTORE_TEST_DSN is not set: these integration tests need an isolated PostgreSQL DSN; a skip here is not a pass")
	}
	return dsn
}

// CreateSchema creates a uniquely named schema and tears it down afterwards.
func CreateSchema(t *testing.T, dsn string) (string, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("sqltest_%d", time.Now().UnixNano())
	bootstrap, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bootstrap.Close(context.Background()) })
	if _, err := bootstrap.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := bootstrap.Exec(dropCtx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})
	return schema, bootstrap
}

// Connect opens a helper connection pinned to the schema for fixtures and
// assertions.
func Connect(t *testing.T, dsn, schema string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, `SELECT pg_catalog.set_config('search_path', $1, false)`, schema+", pg_catalog"); err != nil {
		t.Fatal(err)
	}
	return conn
}

// SeedRoom applies the kernel schema and seeds one deployment, the configured
// organization anchor, an owner, a viewer, an optional second writer and one
// native Room with its durable owner record and both slot heads at generation
// one. The statement order is the schema's insertion order: the organization
// anchor, then the resources it is referenced by.
func SeedRoom(t *testing.T, dsn, schema string, extraUsers ...string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn := Connect(t, dsn, schema)
	if err := sqlstore.ApplySchema(ctx, conn, schema); err != nil {
		t.Fatal(err)
	}
	seeds := []string{
		`INSERT INTO organizations (org_id) VALUES ('org-test')`,
		`INSERT INTO configured_org (org_id) VALUES ('org-test')`,
		`INSERT INTO deployment (deployment_id, org_id) VALUES ('dep-test', 'org-test')`,
		`INSERT INTO users (user_id, org_id) VALUES ('u-owner', 'org-test'), ('u-viewer', 'org-test')`,
		`INSERT INTO rooms (room_id, org_id, host_mode, owner_user_id) VALUES ('room-1', 'org-test', 'native', 'u-owner')`,
		`INSERT INTO room_members (room_id, org_id, user_id, role) VALUES ('room-1', 'org-test', 'u-owner', 'owner'), ('room-1', 'org-test', 'u-viewer', 'viewer')`,
		`INSERT INTO slot_generations (org_id, room_id, slot, generation) VALUES ('org-test', 'room-1', 'slot1', 1), ('org-test', 'room-1', 'slot2', 1)`,
		`INSERT INTO room_slots (room_id, slot, current_generation) VALUES ('room-1', 'slot1', 1), ('room-1', 'slot2', 1)`,
	}
	for _, user := range extraUsers {
		seeds = append(seeds,
			`INSERT INTO users (user_id, org_id) VALUES ('`+user+`', 'org-test')`,
			`INSERT INTO room_members (room_id, org_id, user_id, role) VALUES ('room-1', 'org-test', '`+user+`', 'member')`)
	}
	for _, seed := range seeds {
		if _, err := conn.Exec(ctx, seed); err != nil {
			t.Fatal(err)
		}
	}
	return conn
}

// WaitForLockWait polls pg_stat_activity until the given backend is waiting on
// a lock — a real database barrier, bounded by ten seconds.
func WaitForLockWait(t *testing.T, admin *pgx.Conn, pid uint32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waitType *string
		if err := admin.QueryRow(context.Background(), `SELECT wait_event_type FROM pg_catalog.pg_stat_activity WHERE pid = $1`, int32(pid)).Scan(&waitType); err != nil {
			t.Fatal(err)
		}
		if waitType != nil && *waitType == "Lock" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("backend never reached a lock wait")
}

// KillBackend terminates a backend with a confirmable wait.
func KillBackend(t *testing.T, admin *pgx.Conn, pid uint32, what string) {
	t.Helper()
	var ok bool
	if err := admin.QueryRow(context.Background(), `SELECT pg_catalog.pg_terminate_backend($1, 5000)`, int32(pid)).Scan(&ok); err != nil {
		t.Fatalf("terminate %s backend: %v", what, err)
	}
	if !ok {
		t.Fatalf("terminate %s backend: not confirmed", what)
	}
}

// Proxy is a test-only TCP proxy in front of PostgreSQL that injects the two
// response-loss boundaries: a forwarded COMMIT whose response is dropped (the
// server accepted the commit; the client cannot know) and a break before
// COMMIT reaches the server. Injection is armed per chunk.
type Proxy struct {
	t        *testing.T
	listener net.Listener
	upstream string
	mode     string
	armed    atomic.Bool
	mu       sync.Mutex
	conns    []net.Conn
}

// StartProxy listens on an ephemeral loopback port in front of the DSN's
// PostgreSQL.
func StartProxy(t *testing.T, mode string) *Proxy {
	t.Helper()
	conf, err := pgx.ParseConfig(RequireDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &Proxy{t: t, listener: listener, upstream: net.JoinHostPort(conf.Host, strconv.Itoa(int(conf.Port))), mode: mode}
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

// DSN rewrites a direct DSN through the proxy.
func (p *Proxy) DSN(base string) string {
	conf, err := pgx.ParseConfig(base)
	if err != nil {
		p.t.Fatal(err)
	}
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		url.QueryEscape(conf.User), url.QueryEscape(conf.Password), p.listener.Addr().String(), conf.Database)
}

// Arm enables injection; before that the proxy is a pass-through so the Store
// can open.
func (p *Proxy) Arm() { p.armed.Store(true) }

func (p *Proxy) serve() {
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

func (p *Proxy) handle(client net.Conn) {
	server, err := net.Dial("tcp", p.upstream)
	if err != nil {
		_ = client.Close()
		return
	}
	closeBoth := func() {
		_ = client.Close()
		_ = server.Close()
	}
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
