package sqlstore

import (
	"errors"
	"github.com/sean2077/pairroom/internal/enterprisebackup"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupUnknownExportBothCommitOutcomesRetainGateAndNoManifest(t *testing.T) {
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			b, objects := backupPGSource(t)
			if e := b.f.store.Close(); e != nil {
				t.Fatal(e)
			}
			proxy := startProxy(t, mode)
			cfg := b.f.cfg
			cfg.DSN = proxy.dsn(cfg.DSN)
			s, e := Open(t.Context(), cfg)
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { _ = s.Close() })
			if _, e = s.BeginAttachmentBackup(t.Context(), objects, "unknown-export"); e != nil {
				t.Fatal(e)
			}
			// The same-ID begin recovers its accepted receipt without COMMIT; the next
			// COMMIT is the data export, so this exercises the coordinator itself.
			proxy.arm()
			path := filepath.Join(t.TempDir(), "bundle")
			manifest, e := s.BackupOffline(t.Context(), objects, path, "unknown-export")
			var k *KernelError
			if !errors.As(e, &k) || k.Outcome != OutcomeUnknown || manifest.Header.BackupID != "" {
				t.Fatalf("unconfirmed export reported success: %+v %v", manifest, e)
			}
			if _, e = os.Stat(filepath.Join(path, "manifest.json")); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("unknown export published complete manifest")
			}
			if _, e = enterprisebackup.Open(t.Context(), path, "dep-test", "org-test"); e == nil {
				t.Fatal("unknown export became usable bundle")
			}
			var active bool
			if e = b.f.admin.QueryRow(t.Context(), `SELECT ended_at IS NULL FROM attachment_backup_barriers WHERE barrier_id='unknown-export'`).Scan(&active); e != nil || !active {
				t.Fatal("unknown export released retention", e)
			}
		})
	}
}
