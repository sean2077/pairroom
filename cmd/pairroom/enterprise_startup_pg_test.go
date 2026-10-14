package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

func TestEnterpriseStartupRecoveryPrecedesOpeningListener(t *testing.T) {
	dsn := os.Getenv("PAIRROOM_SQLSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PAIRROOM_SQLSTORE_TEST_DSN is required for real PostgreSQL startup ordering")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Discovery is a real bounded TLS request; no identity token or provider
	// account is needed to verify the application's startup ordering.
	var issuer string
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/jwks", "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"}})
	}))
	defer idp.Close()
	issuer = idp.URL
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	_, port, err := net.SplitHostPort(occupied.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cfg := testEnterpriseConfig()
	cfg.Listen = occupied.Addr().String()
	cfg.PublicOrigin = "https://pairroom.example.test:" + port
	cfg.OIDC.Issuer, cfg.OIDC.AllowedOrigins, cfg.OIDC.ClientSecretEnv = issuer, []string{issuer}, ""
	now := time.Now()
	cfg.TLSCertificateFile, cfg.TLSKeyFile = enterpriseTestCertificate(t, "pairroom.example.test", now.Add(-time.Hour), now.Add(time.Hour))
	cfg.OIDC.CACertificateFile = filepath.Join(t.TempDir(), "idp-ca.pem")
	if err := os.WriteFile(cfg.OIDC.CACertificateFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: idp.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("cli_startup_%d", time.Now().UnixNano())
	storeConfig := sqlstore.Config{DSN: dsn, Schema: schema, DeploymentID: "cli-startup-test", RequireSessionAuth: true}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("test PostgreSQL unavailable")
	}
	defer admin.Close(context.Background())
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, _ = admin.Exec(cleanup, `DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
	}()
	if err := sqlstore.InitializeDeployment(ctx, storeConfig, cfg.OrganizationID); err != nil {
		t.Fatal(err)
	}
	store, err := sqlstore.Open(ctx, storeConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.InitializeIdentity(ctx, sqlstore.IdentityConfig{Connections: []sqlstore.TrustConnectionConfig{{ConnectionID: cfg.OIDC.ConnectionID, Issuer: issuer}}, Bootstrap: enterpriseauth.VerifiedIdentity{ConnectionID: cfg.OIDC.ConnectionID, Issuer: issuer, Subject: "synthetic-operator"}})
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// An otherwise healthy deployment reaches the deliberately occupied
	// listener. Thus an earlier refusal below cannot be a hidden TLS/IdP error.
	err = serveEnterprise(ctx, cfg, storeConfig)
	if err == nil || err.Error() != "enterprise listen address is unavailable" {
		t.Fatalf("healthy startup did not reach listener: %v", err)
	}
	userTable := pgx.Identifier{schema, "users"}.Sanitize()
	roomTable := pgx.Identifier{schema, "rooms"}.Sanitize()
	if _, err := admin.Exec(ctx, `INSERT INTO `+userTable+`(user_id,org_id) VALUES('corrupt-owner',$1)`, cfg.OrganizationID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO `+roomTable+`(room_id,org_id,owner_user_id) VALUES('corrupt-empty-room',$1,'corrupt-owner')`, cfg.OrganizationID); err != nil {
		t.Fatal(err)
	}
	err = serveEnterprise(ctx, cfg, storeConfig)
	if err == nil || err.Error() != "enterprise startup recovery is not complete; no public listener was opened" {
		t.Fatalf("listener opening preceded failed recovery: %v", err)
	}
}
