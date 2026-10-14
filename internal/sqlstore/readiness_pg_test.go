package sqlstore

import (
	"context"
	"crypto/sha256"
	"github.com/sean2077/pairroom/internal/model"
	"testing"
)

func TestReadinessIsFencedMetadataAndCurrentRecoveryOnly(t *testing.T) {
	f := backupPGPristine(t)
	before, e := f.store.Readiness(t.Context())
	if e != nil || before != (Readiness{SchemaCompatible: true, OwnershipCurrent: true, RestoreClear: true}) {
		t.Fatal(before, e)
	}
	if _, e = f.store.RecoverDeliveries(t.Context()); e != nil {
		t.Fatal(e)
	}
	after, e := f.store.Readiness(t.Context())
	if e != nil || after != (Readiness{SchemaCompatible: true, OwnershipCurrent: true, RecoveryComplete: true, RestoreClear: true}) {
		t.Fatal(after, e)
	}
	if e = f.store.Close(); e != nil {
		t.Fatal(e)
	}
	f.store, e = Open(context.Background(), f.cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	reopened, e := f.store.Readiness(t.Context())
	if e != nil || reopened.RecoveryComplete || !reopened.OwnershipCurrent {
		t.Fatal("old epoch certified current recovery", reopened, e)
	}
	if e = f.store.Close(); e != nil {
		t.Fatal(e)
	}
	// Recovery itself leaves a durable audit marker; restore admission requires
	// a separate pristine schema rather than pretending that database is new.
	f = backupPGPristine(t)
	digest := sha256.Sum256([]byte("readiness-only-proof"))
	p := f.store.restorePlan(&restoreMutation{kind: "begin", id: "readiness-barrier", digest: digest})
	if _, e = f.store.commitWithRestarts(t.Context(), p, model.Event{}); e != nil {
		t.Fatal(e)
	}
	pending, e := f.store.Readiness(t.Context())
	if e != nil || pending.RestoreClear || !pending.SchemaCompatible || !pending.OwnershipCurrent {
		t.Fatal("metadata hid pending restore", pending, e)
	}
	if e = f.store.Close(); e != nil {
		t.Fatal(e)
	}
	closed, e := f.store.Readiness(t.Context())
	if e == nil || closed != (Readiness{}) {
		t.Fatal("closed lease disclosed readiness", closed, e)
	}
}
