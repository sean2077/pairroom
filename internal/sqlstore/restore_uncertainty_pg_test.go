package sqlstore

import (
	"context"
	"encoding/json"
	"github.com/sean2077/pairroom/internal/enterprisebackup"
	"github.com/sean2077/pairroom/internal/enterpriseobject"
	"github.com/sean2077/pairroom/internal/model"
	"path/filepath"
	"testing"
)

func TestRestoreImportUnknownBothActualCommitOutcomesOnlyResolve(t *testing.T) {
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			b, objects := backupPGSource(t)
			path := filepath.Join(t.TempDir(), "bundle")
			if _, e := b.f.store.BackupOffline(t.Context(), objects, path, "unknown-backup"); e != nil {
				t.Fatal(e)
			}
			bundle, e := enterprisebackup.Open(t.Context(), path, "dep-test", "org-test")
			if e != nil {
				t.Fatal(e)
			}
			if e = b.f.store.Close(); e != nil {
				t.Fatal(e)
			}
			target := backupPGPristine(t)
			if e = target.store.Close(); e != nil {
				t.Fatal(e)
			}
			proxy := startProxy(t, mode)
			cfg := target.cfg
			cfg.DSN = proxy.dsn(cfg.DSN)
			target.store, e = Open(t.Context(), cfg)
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { _ = target.store.Close() })
			begin := target.store.restorePlan(&restoreMutation{kind: "begin", id: "uncertain-import", digest: bundle.Digest()})
			if _, e = target.store.commitWithRestarts(t.Context(), begin, model.Event{}); e != nil {
				t.Fatal(e)
			}
			proxy.arm()
			p := target.store.restorePlan(&restoreMutation{kind: "import", id: "uncertain-import", digest: bundle.Digest(), bundle: bundle})
			r, e := target.store.commitWithRestarts(t.Context(), p, model.Event{})
			if k := kernelErr(t, r, e); k.Outcome != OutcomeUnknown || r.Restore != nil {
				t.Fatal("unknown disclosed candidate restore receipt")
			}
			first, e := target.store.ResolveRestore(t.Context(), "uncertain-import", bundle.Digest())
			if e != nil || first.Outcome != OutcomeReplayed {
				t.Fatal(first, e)
			}
			imported := mode == "drop-response"
			if first.Restore.Imported != imported || first.Restore.Complete {
				t.Fatal("Resolve invented import or completion", first)
			}
			var usersBefore int
			if e = target.admin.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&usersBefore); e != nil {
				t.Fatal(e)
			}
			for range 2 {
				again, e := target.store.ResolveRestore(t.Context(), "uncertain-import", bundle.Digest())
				if e != nil || *again.Restore != *first.Restore {
					t.Fatal("Resolve changed original accepted identity", e)
				}
			}
			var usersAfter int
			if e = target.admin.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&usersAfter); e != nil || usersAfter != usersBefore || imported != (usersAfter > 0) {
				t.Fatal("Resolve retried uncertain import", e)
			}
			if e = target.store.EnsureRestoreReady(t.Context()); e == nil {
				t.Fatal("unknown restore opened listener")
			}
		})
	}
}

func TestRestoreRejectsChecksummedAnchorMismatchBeforeReservation(t *testing.T) {
	b, objects := backupPGSource(t)
	sourcePath := filepath.Join(t.TempDir(), "source-bundle")
	if _, e := b.f.store.BackupOffline(t.Context(), objects, sourcePath, "anchor-backup"); e != nil {
		t.Fatal(e)
	}
	source, e := enterprisebackup.Open(t.Context(), sourcePath, "dep-test", "org-test")
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "mismatched-bundle")
	w, e := enterprisebackup.NewWriter(path, source.Manifest().Header)
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range enterprisebackup.Tables() {
		if e = w.WriteTable(t.Context(), name, func(emit func(json.RawMessage) error) error {
			return source.TableRows(t.Context(), name, func(raw json.RawMessage) error {
				if name == "deployment" {
					var row map[string]json.RawMessage
					if e := json.Unmarshal(raw, &row); e != nil {
						return e
					}
					row["deployment_id"] = json.RawMessage(`"forged-deployment"`)
					raw, _ = json.Marshal(row)
				}
				return emit(raw)
			})
		}); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = w.Seal(t.Context()); e != nil {
		t.Fatal(e)
	}
	bundle, e := enterprisebackup.Open(t.Context(), path, "dep-test", "org-test")
	if e != nil {
		t.Fatal("bundle should have valid checksums before semantic scope check", e)
	}
	_ = b.f.store.Close()
	target := backupPGPristine(t)
	targetObjects, e := enterpriseobject.Open(filepath.Join(t.TempDir(), "target-objects"), "dep-test")
	if e != nil {
		t.Fatal(e)
	}
	r, e := target.store.RestoreOffline(t.Context(), targetObjects, bundle, "mismatched-anchor")
	if e == nil || r.Outcome != OutcomeRefused {
		t.Fatal("accepted checksummed but unrelated deployment body", r, e)
	}
	var markers int
	if e = target.admin.QueryRow(t.Context(), `SELECT count(*) FROM restore_runs`).Scan(&markers); e != nil || markers != 0 {
		t.Fatal("invalid bundle mutated reservation", e)
	}
	if e = target.store.EnsureRestoreReady(t.Context()); e != nil {
		t.Fatal("invalid preflight barred pristine target", e)
	}
}
func TestRestoreAuditFailureRollsBackAuthorityImportRetainsBarrier(t *testing.T) {
	b, objects := backupPGSource(t)
	path := filepath.Join(t.TempDir(), "bundle")
	if _, e := b.f.store.BackupOffline(t.Context(), objects, path, "rollback-backup"); e != nil {
		t.Fatal(e)
	}
	bundle, e := enterprisebackup.Open(t.Context(), path, "dep-test", "org-test")
	if e != nil {
		t.Fatal(e)
	}
	_ = b.f.store.Close()
	target := backupPGPristine(t)
	targetObjects, e := enterpriseobject.Open(filepath.Join(t.TempDir(), "objects"), "dep-test")
	if e != nil {
		t.Fatal(e)
	}
	mustExec(t, target.admin, `CREATE FUNCTION reject_restore_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='deployment.restore' THEN RAISE EXCEPTION 'injected restore audit failure'; END IF; RETURN NEW; END $$`)
	mustExec(t, target.admin, `CREATE TRIGGER reject_restore_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION reject_restore_audit()`)
	r, e := target.store.RestoreOffline(context.Background(), targetObjects, bundle, "rollback-import")
	if e == nil || r.Outcome != OutcomeNotCommitted {
		t.Fatal(r, e)
	}
	resolved, e := target.store.ResolveRestore(t.Context(), "rollback-import", bundle.Digest())
	if e != nil || resolved.Restore.Imported {
		t.Fatal("partial restore accepted", e)
	}
	var users, policies, sessions int
	if e = target.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM users),(SELECT count(*) FROM identity_policy),(SELECT count(*) FROM browser_sessions)`).Scan(&users, &policies, &sessions); e != nil || users != 0 || policies != 0 || sessions != 0 {
		t.Fatal("partial authority escaped rollback", e)
	}
	if e = target.store.EnsureRestoreReady(t.Context()); e == nil {
		t.Fatal("failed import lost durable barrier")
	}
	if _, e = target.store.RestoreOffline(t.Context(), targetObjects, bundle, "rollback-import"); e == nil {
		t.Fatal("automatically repeated failed import")
	}
}
