package sqlstore

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func initializeFixture(t *testing.T, dsn string) (Config, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	cfg := Config{DSN: dsn, Schema: fmt.Sprintf("initialize_%d", time.Now().UnixNano()), DeploymentID: "dep-test", RequireSessionAuth: true}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, `DROP SCHEMA IF EXISTS `+pgx.Identifier{cfg.Schema}.Sanitize()+` CASCADE`); err != nil {
			t.Logf("test namespace cleanup: %v", err)
		}
	})
	return cfg, admin
}

func requireInitializeNamespaceAbsent(t *testing.T, admin *pgx.Conn, schema string) {
	t.Helper()
	var namespace *string
	if err := admin.QueryRow(context.Background(), `SELECT pg_catalog.to_regnamespace($1)::text`, schema).Scan(&namespace); err != nil {
		t.Fatal(err)
	}
	if namespace != nil {
		t.Fatal("failed initialization left a persistent namespace")
	}
}

type initializeQueryMarker struct{}
type initializeTrace struct {
	schema        string
	cancel        context.CancelFunc
	wrote         atomic.Bool
	ddlComplete   atomic.Bool
	anchorReached atomic.Bool
}

func (t *initializeTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	upper := strings.ToUpper(query.SQL)
	if strings.Contains(upper, "CREATE SCHEMA") || strings.Contains(upper, "CREATE TABLE") || strings.HasPrefix(upper, "INSERT ") || strings.HasPrefix(upper, "UPDATE ") || strings.HasPrefix(upper, "DELETE ") {
		t.wrote.Store(true)
	}
	if strings.HasPrefix(query.SQL, "INSERT INTO "+qualified(t.schema, "schema_meta")) {
		return context.WithValue(ctx, initializeQueryMarker{}, true)
	}
	if t.cancel != nil && strings.HasPrefix(query.SQL, "INSERT INTO "+qualified(t.schema, "organizations")) {
		t.anchorReached.Store(true)
		t.cancel()
	}
	return ctx
}
func (t *initializeTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, end pgx.TraceQueryEndData) {
	if ctx.Value(initializeQueryMarker{}) == true && end.Err == nil {
		t.ddlComplete.Store(true)
	}
}

func initializeTracedConn(t *testing.T, cfg Config, trace *initializeTrace) *pgx.Conn {
	t.Helper()
	connection, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		t.Fatal(err)
	}
	connection.Tracer = trace
	conn, err := pgx.ConnectConfig(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func TestInitializeDeploymentLiveHolderRejectsBeforeAnyDDL(t *testing.T) {
	f := newFixture(t)
	cfg, admin := initializeFixture(t, f.dsn)
	trace := &initializeTrace{schema: cfg.Schema}
	conn := initializeTracedConn(t, cfg, trace)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := initializeDeploymentConn(ctx, conn, cfg, "org-test"); err == nil || !strings.Contains(err.Error(), "live lease holder") {
		t.Fatalf("live-holder refusal: %v", err)
	}
	if trace.wrote.Load() {
		t.Fatal("initialization issued DDL or anchor writes before refusing the live holder")
	}
	requireInitializeNamespaceAbsent(t, admin, cfg.Schema)
	// Cover the public connection-owning entry too, including any work it
	// might accidentally perform before entering the shared implementation.
	if err := InitializeDeployment(ctx, cfg, "org-test"); err == nil || !strings.Contains(err.Error(), "live lease holder") {
		t.Fatalf("public initializer did not refuse: %v", err)
	}
	requireInitializeNamespaceAbsent(t, admin, cfg.Schema)
	if result, err := f.store.Commit(ctx, f.command("after-init-refusal")); err != nil || result.Outcome != OutcomeCommitted {
		t.Fatalf("initialization damaged the existing lease: %+v %v", result, err)
	}
}

func TestInitializeDeploymentFailureAfterDDLRollsBackNamespace(t *testing.T) {
	cfg, admin := initializeFixture(t, testDSN(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	trace := &initializeTrace{schema: cfg.Schema, cancel: cancel}
	conn := initializeTracedConn(t, cfg, trace)
	if err := initializeDeploymentConn(ctx, conn, cfg, "org-test"); err == nil {
		t.Fatal("canceled initialization reported success")
	}
	if !trace.ddlComplete.Load() || !trace.anchorReached.Load() {
		t.Fatal("fault did not follow completed schema DDL at the anchor insertion boundary")
	}
	requireInitializeNamespaceAbsent(t, admin, cfg.Schema)
	if err := conn.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The failed transaction and its session lease leave no obstacle to a
	// new explicit initialization, which can subsequently be opened normally.
	if err := InitializeDeployment(context.Background(), cfg, "org-test"); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInitializeDeploymentAnchorsAreExactAndIdempotent(t *testing.T) {
	cfg, admin := initializeFixture(t, testDSN(t))
	ctx := context.Background()
	if err := InitializeDeployment(ctx, cfg, "org-test"); err != nil {
		t.Fatal(err)
	}
	if err := InitializeDeployment(ctx, cfg, "org-test"); err != nil {
		t.Fatal(err)
	}
	if err := InitializeDeployment(ctx, cfg, "org-other"); err == nil {
		t.Fatal("initialization relabeled an existing organization")
	}
	other := cfg
	other.DeploymentID = "dep-other"
	if err := InitializeDeployment(ctx, other, "org-test"); err == nil {
		t.Fatal("initialization relabeled an existing deployment")
	}
	var deployment, org string
	var users, rooms int
	if err := admin.QueryRow(ctx, `SELECT deployment_id,org_id,(SELECT count(*) FROM `+qualified(cfg.Schema, "users")+`),(SELECT count(*) FROM `+qualified(cfg.Schema, "rooms")+`) FROM `+qualified(cfg.Schema, "deployment")+` WHERE id`).Scan(&deployment, &org, &users, &rooms); err != nil {
		t.Fatal(err)
	}
	if deployment != "dep-test" || org != "org-test" || users != 0 || rooms != 0 {
		t.Fatalf("anchor replay changed identity or invented actors: %s %s users=%d rooms=%d", deployment, org, users, rooms)
	}
}

func TestIdentityReadinessPinsOrganizationAndPreservesLease(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	identity := identityPGConfig().Bootstrap
	for _, request := range []struct{ org, connection, issuer string }{
		{"org-other", identity.ConnectionID, identity.Issuer},
		{"org-test", "other-unconfigured", identity.Issuer},
		{"org-test", identity.ConnectionID, "https://different.example.test"},
	} {
		if err := f.store.CheckIdentityConfiguration(ctx, request.org, request.connection, request.issuer); err == nil {
			t.Fatalf("readiness accepted mismatched %v", request)
		}
	}
	if err := f.store.CheckIdentityConfiguration(ctx, "org-test", identity.ConnectionID, identity.Issuer); err != nil {
		t.Fatal(err)
	}
	cmd, session := identityPGLogin(t, f, "operator-selected", "readiness-after-mismatch")
	if !session.Administrator {
		t.Fatal("mismatch poisoned normal bootstrap or authenticated read")
	}
	if _, err := f.store.AuthenticateSession(ctx, cmd.Session.TokenHash); err != nil {
		t.Fatal(err)
	}
}
