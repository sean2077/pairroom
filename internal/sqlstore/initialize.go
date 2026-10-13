package sqlstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// InitializeDeployment is an explicit operator action, before Open or any
// public listener. It creates a fresh schema and its exact immutable anchors,
// or verifies an already-complete matching deployment. It never creates a
// User, Room, administrator, session or application credential.
func InitializeDeployment(ctx context.Context, cfg Config, orgID string) error {
	if cfg.DSN == "" || !schemaNamePattern.MatchString(cfg.Schema) || !validRoomCreateIdentity(cfg.DeploymentID) || !validRoomCreateIdentity(orgID) {
		return errors.New("deployment initialization requires DSN, schema and exact deployment/organization IDs")
	}
	ctx, cancel := context.WithTimeout(ctx, openBudget)
	defer cancel()
	conn, err := pgx.Connect(ctx, cfg.DSN)
	if err != nil {
		return err
	}
	defer func() { cleanup, stop := cleanupCtx(); defer stop(); _ = conn.Close(cleanup) }()
	return initializeDeploymentConn(ctx, conn, cfg, orgID)
}

// initializeDeploymentConn runs over an owned connection that its caller
// closes afterwards. The production entry supplies the bounded context and
// validated configuration. Keeping this connection seam separate also lets
// tests observe real driver statement boundaries without production hooks.
func initializeDeploymentConn(ctx context.Context, conn *pgx.Conn, cfg Config, orgID string) error {
	// Acquire the exact live-Service session lock before any namespace/DDL
	// effect. Closing this connection releases it on every exit path.
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_catalog.pg_try_advisory_lock($1)`, int64(leaseKey(cfg.DeploymentID))).Scan(&acquired); err != nil {
		return err
	}
	if !acquired {
		return errors.New("deployment already has a live lease holder")
	}
	if err := configureSession(ctx, conn, cfg.Schema); err != nil {
		return err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { cleanup, stop := cleanupCtx(); defer stop(); _ = tx.Rollback(cleanup) }()
	if _, err := tx.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+pgx.Identifier{cfg.Schema}.Sanitize()); err != nil {
		return err
	}
	if err := applySchemaInTransaction(ctx, tx, cfg.Schema); err != nil {
		return err
	}
	// Version 2 is still unreleased. Do not silently retrofit a previous
	// partial implementation merely because its numeric version matches.
	for _, table := range []string{"identity_policy", "identity_admissions", "organization_admins", "browser_sessions", "control_events", "control_operations"} {
		var present *string
		if err := tx.QueryRow(ctx, `SELECT pg_catalog.to_regclass($1)::text`, qualified(cfg.Schema, table)).Scan(&present); err != nil {
			return err
		}
		if present == nil {
			return fmt.Errorf("identity schema is incomplete: explicit initialization of a fresh supported schema is required")
		}
	}
	var orgCount, anchorCount, deploymentCount int
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+qualified(cfg.Schema, "organizations")+`),(SELECT count(*) FROM `+qualified(cfg.Schema, "configured_org")+`),(SELECT count(*) FROM `+qualified(cfg.Schema, "deployment")+`)`).Scan(&orgCount, &anchorCount, &deploymentCount); err != nil {
		return err
	}
	if orgCount == 0 && anchorCount == 0 && deploymentCount == 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO `+qualified(cfg.Schema, "organizations")+`(org_id) VALUES($1)`, orgID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+qualified(cfg.Schema, "configured_org")+`(org_id) VALUES($1)`, orgID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+qualified(cfg.Schema, "deployment")+`(deployment_id,org_id) VALUES($1,$2)`, cfg.DeploymentID, orgID); err != nil {
			return err
		}
	} else {
		if orgCount != 1 || anchorCount != 1 || deploymentCount != 1 {
			return errors.New("partial or ambiguous deployment anchors; initialization refuses to guess")
		}
		var actualDeployment, actualOrg string
		if err := tx.QueryRow(ctx, `SELECT deployment_id,org_id FROM `+qualified(cfg.Schema, "deployment")+` WHERE id FOR UPDATE`).Scan(&actualDeployment, &actualOrg); err != nil {
			return err
		}
		if actualDeployment != cfg.DeploymentID || actualOrg != orgID {
			return errors.New("configured deployment/organization does not match persistent anchors")
		}
	}
	return tx.Commit(ctx)
}

// CheckIdentityConfiguration is a fenced, content-free startup/readiness
// check. Serve calls it after Open and refuses uninitialized or mismatched
// trust; it never auto-initializes or bootstraps an identity.
func (s *Store) CheckIdentityConfiguration(ctx context.Context, orgID, connectionID, issuer string) error {
	if !validRoomCreateIdentity(orgID) {
		return refusedError(ClassInvalidRequest, nil)
	}
	plan := opPlan{control: &controlMutation{kind: controlReadiness, expectedOrgID: orgID, connectionID: connectionID, issuer: issuer}}
	_, err := s.resolveWithFallback(ctx, plan)
	return err
}
