package lanclient

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogPreflightIsReadOnlyAndUsesItsCallerContext(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if err := store.PreflightCatalog(ctx); err != nil {
		t.Fatalf("absent catalog: %v", err)
	}
	if _, err := os.Stat(store.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight created an absent catalog: %v", err)
	}
	remote := newRemote(t)
	client, _, _ := remote.join(t, store)
	metadata, err := client.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Root(), metadata.ID, "client.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contacts := len(remote.actions())
	if err := store.PreflightCatalog(ctx); err != nil {
		t.Fatalf("valid existing catalog: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.PreflightCatalog(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("catalog preflight ignored cancellation: %v", err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("preflight rewrote a client identity: %v", err)
	}
	if len(remote.actions()) != contacts {
		t.Fatal("local format preflight contacted the remote host")
	}
}
