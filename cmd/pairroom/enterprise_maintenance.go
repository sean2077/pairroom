package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sean2077/pairroom/internal/enterprisebackup"
	"github.com/sean2077/pairroom/internal/enterpriseobject"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

func maintenanceID(value string) bool { return enterpriseIDPattern.MatchString(value) }
func maintenanceDigest(value string) ([32]byte, error) {
	var digest [32]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(digest) || hex.EncodeToString(decoded) != value {
		return digest, errors.New("enterprise manifest hash must be canonical SHA256")
	}
	copy(digest[:], decoded)
	return digest, nil
}
func maintenanceOutsideObjects(path, objects string) bool {
	file, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	root, err := filepath.Abs(objects)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, file)
	return err == nil && relative != "." && (relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

// Maintenance is an explicit offline operator command. It cannot share the
// deployment lease with a serving instance and exposes no browser endpoint.
func runEnterpriseMaintenance(args []string) error {
	action := args[0]
	flags := flag.NewFlagSet("pairroom enterprise "+action, flag.ContinueOnError)
	configuration := flags.String("config", "", "operator JSON configuration")
	id := flags.String("id", "", "stable operator operation ID; retain it on uncertainty")
	var output, source, manifestHash *string
	switch action {
	case "backup":
		output = flags.String("output", "", "new owner-private bundle directory outside the object root")
	case "restore":
		source = flags.String("source", "", "verified owner-private bundle; target configuration selects a pristine schema and new object root")
	case "resolve-restore", "complete-restore":
		manifestHash = flags.String("manifest-hash", "", "original bundle manifest SHA256")
	case "backup-end":
	default:
		return errors.New("unsupported enterprise maintenance command")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || !maintenanceID(*id) {
		return errors.New("enterprise maintenance requires --config and a stable valid --id")
	}
	cfg, err := loadEnterpriseConfig(*configuration)
	if err != nil {
		return err
	}
	if cfg.Attachments == nil {
		return errors.New("enterprise maintenance requires the explicitly configured private attachment directory")
	}
	if output != nil {
		if *output == "" || !maintenanceOutsideObjects(*output, cfg.Attachments.Directory) {
			return errors.New("enterprise backup output must be outside the managed object root")
		}
		if _, err := os.Lstat(*output); !errors.Is(err, os.ErrNotExist) {
			return errors.New("enterprise backup requires a new output directory; inspect any prior incomplete bundle and its retention barrier")
		}
		parent := filepath.Dir(*output)
		info, err := os.Lstat(parent)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("enterprise backup output parent must be an existing direct directory")
		}
	}
	parent, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(parent, 20*time.Minute)
	defer cancel()
	var bundle *enterprisebackup.Bundle
	var digest [32]byte
	if source != nil {
		if *source == "" || !maintenanceOutsideObjects(*source, cfg.Attachments.Directory) {
			return errors.New("enterprise restore source must be outside the new managed object root")
		}
		bundle, err = enterprisebackup.Open(ctx, *source, cfg.DeploymentID, cfg.OrganizationID)
		if err != nil {
			return errors.New("enterprise restore bundle is incomplete, corrupt or incompatible; target was not opened")
		}
		digest = bundle.Digest()
	} else if manifestHash != nil {
		digest, err = maintenanceDigest(*manifestHash)
		if err != nil {
			return err
		}
	}
	dsn, err := enterpriseSecretFromEnv(cfg.DatabaseDSNEnv, true)
	if err != nil {
		return err
	}
	storeConfig := sqlstore.Config{DSN: dsn, Schema: cfg.DatabaseSchema, DeploymentID: cfg.DeploymentID, RequireSessionAuth: true, AttachmentLimits: cfg.attachmentLimits(), ResourceLimits: cfg.sqlResourceLimits()}
	if action == "restore" {
		// Existing application data is never erased by initialization. The
		// restore transaction independently requires pristine anchor-only state.
		if err := sqlstore.InitializeDeployment(ctx, storeConfig, cfg.OrganizationID); err != nil {
			return errors.New("enterprise restore target is unavailable or incompatible; no import was attempted")
		}
	}
	store, err := sqlstore.Open(ctx, storeConfig)
	if err != nil {
		return errors.New("enterprise maintenance requires exclusive database ownership; stop the serving instance first")
	}
	defer func() { _ = store.Close() }()
	objects, err := enterpriseobject.Open(cfg.Attachments.Directory, cfg.DeploymentID)
	if err != nil {
		return errors.New("enterprise object root is unavailable or belongs to another deployment")
	}
	if action == "backup" || action == "backup-end" {
		if err := store.EnsureRestoreReady(ctx); err != nil {
			return errors.New("enterprise incomplete restore must be resolved before backup")
		}
		if err := store.CheckIdentityConfiguration(ctx, cfg.OrganizationID, cfg.OIDC.ConnectionID, cfg.OIDC.Issuer); err != nil {
			return errors.New("enterprise backup identity configuration does not match the deployment")
		}
	}
	switch action {
	case "backup":
		manifest, err := store.BackupOffline(ctx, objects, *output, *id)
		if err != nil || manifest.Header.BackupID != *id || manifest.Header.DeploymentID != cfg.DeploymentID {
			return errors.New("enterprise backup did not complete; retain the original ID and inspect its retention barrier before explicit backup-end")
		}
		return json.NewEncoder(os.Stdout).Encode(struct {
			DeploymentID string `json:"deployment_id"`
			BackupID     string `json:"backup_id"`
			Rows         uint64 `json:"rows"`
			Bytes        int64  `json:"bytes"`
			Objects      int    `json:"objects"`
		}{cfg.DeploymentID, manifest.Header.BackupID, manifest.Rows, manifest.Bytes, len(manifest.Objects)})
	case "backup-end":
		result, err := store.EndAttachmentBackup(ctx, objects, *id)
		return maintenanceResult(cfg.DeploymentID, *id, digest, result, err, true)
	case "resolve-restore":
		result, err := store.ResolveRestore(ctx, *id, digest)
		return maintenanceResult(cfg.DeploymentID, *id, digest, result, err, false)
	case "restore":
		result, err := store.RestoreOffline(ctx, objects, bundle, *id)
		if err != nil || result.Restore == nil || !result.Restore.Imported {
			return maintenanceResult(cfg.DeploymentID, *id, digest, result, err, true)
		}
		if err := store.Close(); err != nil {
			return errors.New("enterprise restore import is recorded but ownership release is uncertain; resolve the original ID before completion")
		}
		store, err = sqlstore.Open(ctx, storeConfig)
		if err != nil {
			return errors.New("enterprise restore import is recorded; reacquiring ownership for completion failed")
		}
		fallthrough
	case "complete-restore":
		result, err := store.ResolveRestore(ctx, *id, digest)
		if err != nil || result.Restore == nil || !result.Restore.Imported {
			return maintenanceResult(cfg.DeploymentID, *id, digest, result, err, true)
		}
		if !result.Restore.Complete {
			if _, err := store.RecoverDeliveries(ctx); err != nil {
				return errors.New("enterprise restore recovery did not complete; public serving remains barred")
			}
			result, err = store.CompleteRestore(ctx, objects, *id, digest)
		}
		return maintenanceResult(cfg.DeploymentID, *id, digest, result, err, true)
	}
	return errors.New("unsupported enterprise maintenance command")
}

func maintenanceResult(deployment, id string, digest [32]byte, result sqlstore.Result, operationErr error, requireComplete bool) error {
	response := struct {
		DeploymentID string                   `json:"deployment_id"`
		OperationID  string                   `json:"operation_id"`
		ManifestHash string                   `json:"manifest_hash,omitempty"`
		Outcome      sqlstore.Outcome         `json:"outcome"`
		Class        string                   `json:"class,omitempty"`
		Receipt      *sqlstore.RestoreReceipt `json:"receipt,omitempty"`
	}{DeploymentID: deployment, OperationID: id, Outcome: result.Outcome, Class: result.Class, Receipt: result.Restore}
	if digest != ([32]byte{}) {
		response.ManifestHash = hex.EncodeToString(digest[:])
	}
	if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
		return fmt.Errorf("write enterprise maintenance result: %w", err)
	}
	if operationErr != nil {
		return errors.New("enterprise maintenance was not confirmed; use the original ID and manifest hash to resolve without repeating import")
	}
	if result.Outcome != sqlstore.OutcomeCommitted && result.Outcome != sqlstore.OutcomeReplayed && result.Outcome != sqlstore.OutcomeNotCommitted {
		return errors.New("enterprise maintenance did not return a confirmed result")
	}
	if requireComplete && result.Outcome == sqlstore.OutcomeNotCommitted {
		return errors.New("enterprise maintenance effect was not committed")
	}
	if requireComplete && result.Restore != nil && !result.Restore.Complete {
		return errors.New("enterprise restore remains incomplete; inspection or explicit completion is required before serving")
	}
	return nil
}
