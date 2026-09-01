package host

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/service"
)

func TestEmbeddedHostOwnsOneDataRootAndShutsDown(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	first, err := Start(ctx, Options{
		DataRoot:                 root,
		Mock:                     true,
		DisableExternalDiscovery: true,
		RuntimeLimit:             1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Mode() != ModeEmbedded || !strings.Contains(first.URL(), "?desktop=1#token=") {
		t.Fatalf("unexpected embedded host: mode=%q url=%q", first.Mode(), first.URL())
	}

	if _, err := Start(ctx, Options{
		DataRoot:                 root,
		Mock:                     true,
		DisableExternalDiscovery: true,
	}); !errors.Is(err, service.ErrServiceAlreadyRunning) {
		t.Fatalf("second host error = %v, want ErrServiceAlreadyRunning", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := first.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	if err := first.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("idempotent shutdown: %v", err)
	}
}

func TestEnvironmentPathsAreAccepted(t *testing.T) {
	root := t.TempDir()
	t.Setenv(dataRootVariable, root)
	t.Setenv(configPathVariable, "")
	t.Setenv("PAIRROOM_DESKTOP_URL", "")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	value, err := Start(ctx, Options{
		Mock:                     true,
		DisableExternalDiscovery: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer value.Shutdown(context.Background())
	if value.Mode() != ModeEmbedded {
		t.Fatalf("mode = %q", value.Mode())
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("desktop data root was not used: %v", err)
	}
}
