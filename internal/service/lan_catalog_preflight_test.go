package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func lanPreflightCatalogRoot(t *testing.T) string {
	t.Helper()
	root, err := lanclient.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Mkdir(root); err != nil {
		t.Fatal(err)
	}
	return root
}

func lanPreflightDamagedDirectory(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "lan_"+strings.Repeat("a", 32))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := privatefile.CheckDirectory(dir); !errors.Is(err, privatefile.ErrPrivate) {
		t.Fatalf("fixture did not create an owner-boundary failure: %v", err)
	}
	return dir
}

func TestFutureLANClientCatalogErrorsPrecedeRegistryCleanup(t *testing.T) {
	for _, scenario := range []string{"oversized_private_record", "permission_error_before_future", "non_regular_record", "redirected_record"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "redirected_record" && runtime.GOOS == "windows" {
				t.Skip("Windows symlink creation requires an independent OS privilege")
			}
			relayclient.IsolateNativeCaller(t)
			catalog := lanPreflightCatalogRoot(t)
			if scenario == "permission_error_before_future" {
				lanPreflightDamagedDirectory(t, catalog)
			}
			dir := filepath.Join(catalog, "lan_"+strings.Repeat("b", 32))
			if err := privatefile.Mkdir(dir); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "client.json")
			switch scenario {
			case "non_regular_record":
				if err := privatefile.Mkdir(path); err != nil {
					t.Fatal(err)
				}
			case "redirected_record":
				target := filepath.Join(dir, "future.json")
				if err := privatefile.WriteJSON(target, map[string]int{"schema": 2}); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			default:
				if err := privatefile.WriteJSON(path, map[string]int{"schema": 2}); err != nil {
					t.Fatal(err)
				}
				if scenario == "oversized_private_record" {
					file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
					if err != nil {
						t.Fatal(err)
					}
					_, writeErr := file.WriteString(strings.Repeat(" ", 2<<20))
					closeErr := file.Close()
					if writeErr != nil || closeErr != nil {
						t.Fatalf("grow future identity without changing its private ACL: %v %v", writeErr, closeErr)
					}
				}
			}
			root := t.TempDir()
			marker := filepath.Join(root, ".service-registry-catalog.tmp")
			if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: root}); err == nil || !strings.Contains(err.Error(), "data was not modified") {
				t.Fatalf("uninterpretable catalog did not stop Registry recovery: %v", err)
			}
			if got, err := os.ReadFile(marker); err != nil || !bytes.Equal(got, []byte("preserve")) {
				t.Fatalf("catalog failure allowed Registry cleanup: %q %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(root, "service-registry.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("catalog failure allowed checkpoint creation: %v", err)
			}
		})
	}
}

func TestLANClientCatalogOwnerBoundaryAloneDoesNotBlockHostedRooms(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	catalog := lanPreflightCatalogRoot(t)
	damaged := lanPreflightDamagedDirectory(t, catalog)
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("optional catalog permissions blocked the hosting Service: %v", err)
	}
	if err := registry.Healthy(); err != nil {
		t.Fatalf("optional catalog permissions poisoned the local Registry: %v", err)
	}
	if err := privatefile.CheckDirectory(damaged); !errors.Is(err, privatefile.ErrPrivate) {
		t.Fatalf("startup repaired or removed the damaged owner boundary: %v", err)
	}
	entries, err := os.ReadDir(damaged)
	if err != nil || len(entries) != 0 {
		t.Fatalf("startup generated a client identity: %v %v", entries, err)
	}
}
