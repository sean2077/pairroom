package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/enterprisebackup"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/sqlstore"
	"github.com/sean2077/pairroom/internal/sqlstore/sqltest"
)

func TestEnterpriseMaintenanceProcessHelper(t *testing.T) {
	path := os.Getenv("PAIRROOM_MAINTENANCE_TEST_ARGUMENTS")
	if path == "" {
		return
	}
	var args []string
	if err := privatefile.ReadJSON(path, 64<<10, &args); err != nil {
		t.Fatal("private maintenance test arguments unavailable")
	}
	if err := runEnterprise(args); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	os.Exit(0)
}

func maintenanceProcess(t *testing.T, args []string, success bool) []byte {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := privatefile.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "arguments.json")
	if err := privatefile.WriteJSON(path, args); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestEnterpriseMaintenanceProcessHelper$")
	command.Env = append(os.Environ(), "PAIRROOM_MAINTENANCE_TEST_ARGUMENTS="+path)
	output, err := command.CombinedOutput()
	if (err == nil) != success {
		t.Fatalf("maintenance process success=%t error=%v output=%s", success, err, output)
	}
	return output
}

func maintenanceConfigFile(t *testing.T, cfg enterpriseFileConfig) string {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "enterprise.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnterpriseMaintenanceCLIProcessesBackupRestoreAndPreserveSource(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	t.Setenv("PAIRROOM_TEST_DSN", dsn)
	sourceSchema, admin := sqltest.CreateSchema(t, dsn)
	targetSchema, _ := sqltest.CreateSchema(t, dsn)
	cfg := testEnterpriseConfig()
	cfg.DatabaseSchema = sourceSchema
	cfg.DeploymentID = "cli-maintenance-test"
	cfg.Attachments = &enterpriseAttachmentConfig{Directory: filepath.Join(t.TempDir(), "source-objects"), DeploymentBytes: 32 << 20, RoomBytes: 16 << 20, DeploymentPending: 32, RoomPending: 16}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	sourceConfig := sqlstore.Config{DSN: dsn, Schema: sourceSchema, DeploymentID: cfg.DeploymentID, RequireSessionAuth: true, AttachmentLimits: cfg.attachmentLimits()}
	if err := sqlstore.InitializeDeployment(t.Context(), sourceConfig, cfg.OrganizationID); err != nil {
		t.Fatal(err)
	}
	store, err := sqlstore.Open(t.Context(), sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity := enterpriseauth.VerifiedIdentity{ConnectionID: cfg.OIDC.ConnectionID, Issuer: cfg.OIDC.Issuer, Subject: "synthetic-cli-admin"}
	_, err = store.InitializeIdentity(t.Context(), sqlstore.IdentityConfig{Connections: []sqlstore.TrustConnectionConfig{{ConnectionID: identity.ConnectionID, Issuer: identity.Issuer}}, Bootstrap: identity})
	if err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte("synthetic-cli-browser-token"))
	login, err := store.AdmitIdentity(t.Context(), sqlstore.IdentityLoginCommand{ClientID: "login-before-backup", Completion: enterpriseauth.Completion{Identity: identity, Purpose: enterpriseauth.PurposeLogin}, Session: sqlstore.BrowserSessionCandidate{SessionID: "cli-session", TokenHash: tokenHash, CSRFHash: sha256.Sum256([]byte("synthetic-cli-csrf")), ExpiresAt: time.Now().UTC().Add(time.Hour)}})
	if err != nil || login.Control == nil {
		t.Fatal("source session unavailable")
	}
	auth, err := store.AuthenticateSession(t.Context(), tokenHash)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := model.PrepareEnterpriseRoomIntent("CLI backup Room", model.Collaboration{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitRoomCreate(t.Context(), sqlstore.RoomCreateCommand{Auth: auth.Auth, RoomID: "cli-room", ClientID: "cli-room-create", Intent: intent}); err != nil {
		t.Fatal(err)
	}
	sourceFile := maintenanceConfigFile(t, cfg)
	bundlePath := filepath.Join(t.TempDir(), "bundle")
	// A live original owner prevents the operator subprocess from exporting.
	output := maintenanceProcess(t, []string{"backup", "--config", sourceFile, "--output", bundlePath, "--id", "backup-one"}, false)
	if !strings.Contains(string(output), "exclusive database ownership") {
		t.Fatal("backup did not refuse the live service owner")
	}
	if _, err := os.Stat(bundlePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused competing backup created a bundle")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	output = maintenanceProcess(t, []string{"backup", "--config", sourceFile, "--output", bundlePath, "--id", "backup-one"}, true)
	var report struct {
		BackupID string `json:"backup_id"`
		Rows     uint64 `json:"rows"`
	}
	if json.Unmarshal(output, &report) != nil || report.BackupID != "backup-one" || report.Rows == 0 {
		t.Fatalf("CLI backup result not canonical: %s", output)
	}
	bundle, err := enterprisebackup.Open(t.Context(), bundlePath, cfg.DeploymentID, cfg.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	var beforeSource string
	if err := admin.QueryRow(t.Context(), `SELECT row_to_json(t)::text FROM `+pgx.Identifier{sourceSchema, "identity_policy"}.Sanitize()+` t WHERE id`).Scan(&beforeSource); err != nil {
		t.Fatal(err)
	}
	target := cfg
	target.DatabaseSchema = targetSchema
	copyObjects := *cfg.Attachments
	copyObjects.Directory = filepath.Join(t.TempDir(), "target-objects")
	target.Attachments = &copyObjects
	targetFile := maintenanceConfigFile(t, target)
	output = maintenanceProcess(t, []string{"restore", "--config", targetFile, "--source", bundlePath, "--id", "restore-one"}, true)
	var restored struct {
		Outcome      sqlstore.Outcome        `json:"outcome"`
		ManifestHash string                  `json:"manifest_hash"`
		Receipt      sqlstore.RestoreReceipt `json:"receipt"`
	}
	if json.Unmarshal(output, &restored) != nil || !restored.Receipt.Complete || restored.Receipt.RestoreEpoch == bundle.Manifest().Header.RestoreEpoch {
		t.Fatalf("restore CLI did not rotate epoch and complete recovery: %s", output)
	}
	var afterSource string
	if err := admin.QueryRow(t.Context(), `SELECT row_to_json(t)::text FROM `+pgx.Identifier{sourceSchema, "identity_policy"}.Sanitize()+` t WHERE id`).Scan(&afterSource); err != nil || afterSource != beforeSource {
		t.Fatal("CLI restore modified original source authority")
	}
	targetConfig := sourceConfig
	targetConfig.Schema = targetSchema
	restoredStore, err := sqlstore.Open(t.Context(), targetConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredStore.Close()
	if err := restoredStore.EnsureRestoreReady(t.Context()); err != nil {
		t.Fatal("completed CLI restore retained serving barrier")
	}
	if _, err := restoredStore.AuthenticateSession(t.Context(), tokenHash); err == nil {
		t.Fatal("CLI restore revived backed-up browser token")
	}
	if err := restoredStore.CheckIdentityConfiguration(t.Context(), cfg.OrganizationID, cfg.OIDC.ConnectionID, cfg.OIDC.Issuer); err != nil {
		t.Fatal("restored identity policy changed")
	}
	if err := restoredStore.Close(); err != nil {
		t.Fatal(err)
	}
	output = maintenanceProcess(t, []string{"resolve-restore", "--config", targetFile, "--id", "restore-one", "--manifest-hash", restored.ManifestHash}, true)
	if !strings.Contains(string(output), `"complete":true`) {
		t.Fatal("pure original restore resolution lost completed receipt")
	}
	maintenanceProcess(t, []string{"restore", "--config", targetFile, "--source", bundlePath, "--id", "different-restore"}, false)
}
