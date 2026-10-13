package enterpriseauth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureTransaction(t *testing.T, now time.Time) LoginTransaction {
	t.Helper()
	state, _ := randomSecret()
	browser, _ := randomSecret()
	nonce, _ := randomSecret()
	verifier, _ := randomSecret()
	return LoginTransaction{StateHash: secretHash(state), BrowserHash: secretHash(browser), ProviderFingerprint: secretHash("provider-config"), ConnectionID: "connection", Nonce: nonce, PKCEVerifier: verifier, Intent: LoginIntent{Purpose: PurposeLogin, ReturnPath: "/"}, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
}

func TestLoginTransactionsAreBrowserBoundOneUseAndExpiring(t *testing.T) {
	store := testMemoryStore(t, 2)
	now := time.Now().UTC()
	tx := fixtureTransaction(t, now)
	if err := store.Put(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Consume(context.Background(), tx.StateHash, secretHash("wrong-browser"), now); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatalf("wrong browser: %v", err)
	}
	if got, err := store.Consume(context.Background(), tx.StateHash, tx.BrowserHash, now); err != nil || got != tx {
		t.Fatalf("consume: got=%+v err=%v", got, err)
	}
	if _, err := store.Consume(context.Background(), tx.StateHash, tx.BrowserHash, now); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatalf("replay: %v", err)
	}
	expired := fixtureTransaction(t, now)
	if err := store.Put(context.Background(), expired); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Consume(context.Background(), expired.StateHash, expired.BrowserHash, expired.ExpiresAt); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatalf("expiry: %v", err)
	}
	if _, err := store.Consume(context.Background(), expired.StateHash, expired.BrowserHash, now); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatal("expired transaction resurrected")
	}
}

func TestLoginTransactionCapacityPrunesOnlyExpiredEntries(t *testing.T) {
	store := testMemoryStore(t, 1)
	now := time.Now().UTC()
	first := fixtureTransaction(t, now)
	if err := store.Put(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), first); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatalf("duplicate overwrite: %v", err)
	}
	second := fixtureTransaction(t, now)
	if err := store.Put(context.Background(), second); !errors.Is(err, ErrTransactionCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	if got, err := store.Consume(context.Background(), first.StateHash, first.BrowserHash, now); err != nil || got != first {
		t.Fatal("capacity evicted live login")
	}
	if err := store.Put(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	later := fixtureTransaction(t, first.ExpiresAt)
	if err := store.Put(context.Background(), later); err != nil {
		t.Fatalf("expired entry not pruned: %v", err)
	}
	if _, err := store.Consume(context.Background(), first.StateHash, first.BrowserHash, now); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatal("pruned entry resurrected")
	}
}

func TestLoginConcurrentCallbacksHaveExactlyOneConsumer(t *testing.T) {
	store := testMemoryStore(t, 1)
	now := time.Now().UTC()
	tx := fixtureTransaction(t, now)
	if err := store.Put(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 32 {
		wg.Go(func() {
			<-start
			if _, err := store.Consume(context.Background(), tx.StateHash, tx.BrowserHash, now); err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrTransactionInvalid) {
				t.Errorf("unexpected consume error: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("successful consumers: %d", winners.Load())
	}
}

func TestLoginTransactionsRejectInvalidStoredIntent(t *testing.T) {
	for _, change := range []func(*LoginTransaction){
		func(tx *LoginTransaction) { tx.Intent.Purpose = "unknown" },
		func(tx *LoginTransaction) { tx.Intent.ReturnPath = "//other.test" },
		func(tx *LoginTransaction) { tx.ExpiresAt = tx.CreatedAt.Add(11 * time.Minute) },
		func(tx *LoginTransaction) { tx.PKCEVerifier = "short" },
		func(tx *LoginTransaction) { tx.StateHash = [32]byte{} },
		func(tx *LoginTransaction) { tx.BrowserHash = [32]byte{} },
	} {
		store := testMemoryStore(t, 1)
		tx := fixtureTransaction(t, time.Now().UTC())
		change(&tx)
		if err := store.Put(context.Background(), tx); !errors.Is(err, ErrTransactionInvalid) {
			t.Fatalf("invalid transaction accepted: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := testMemoryStore(t, 1)
	tx := fixtureTransaction(t, time.Now().UTC())
	if err := store.Put(ctx, tx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled put: %v", err)
	}
	if err := store.Put(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Consume(ctx, tx.StateHash, tx.BrowserHash, time.Now()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled consume: %v", err)
	}
	if _, err := store.Consume(context.Background(), tx.StateHash, tx.BrowserHash, time.Now()); err != nil {
		t.Fatalf("cancelled call consumed state: %v", err)
	}
}
