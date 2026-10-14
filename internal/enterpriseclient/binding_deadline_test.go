package enterpriseclient

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatelock"
	"github.com/sean2077/pairroom/internal/relay"
)

type deadlineRoundTrip func(*http.Request) (*http.Response, error)

func (f deadlineRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Hold on a different goroutine: Windows private mutexes are OS-thread owned
// and recursive, so a same-goroutine lock would not exercise contention.
func holdDeadlineIdentity(t *testing.T, e *nativeEndpoint) func() {
	t.Helper()
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "pairroom", "native-identities", relay.Digest(string(e.auth.Runtime)+"\x00"+e.auth.SessionID))
	return holdDeadlineDirectory(t, dir)
}

func holdDeadlineDirectory(t *testing.T, dir string) func() {
	t.Helper()
	ready, release, done := make(chan error, 1), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		unlock, err := privatelock.Lock(t.Context(), dir)
		ready <- err
		if err == nil {
			defer unlock()
			<-release
		}
	}()
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	finish := func() { once.Do(func() { close(release); <-done }) }
	t.Cleanup(finish)
	return finish
}

func TestEnterprisePublicWaitOwnBudgetCoversBothPrivateLocks(t *testing.T) {
	for _, lock := range []string{"record", "identity"} {
		t.Run(lock, func(t *testing.T) {
			f := newNativeWireFixture(t)
			e := f.endpoint(t, "public-"+lock, model.ActorSlot1, model.RuntimeClaude)
			f.approve(t, e)
			if lock == "record" {
				holdDeadlineDirectory(t, e.binding.dir)
			} else {
				holdDeadlineIdentity(t, e)
			}
			var calls atomic.Int32
			e.parent.http.Transport = deadlineRoundTrip(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, errors.New("unexpected HTTP behind private lock")
			})
			// The caller has no deadline. Cancellation is only a test watchdog
			// for the old unbounded public-entry wait, not the wait's budget.
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			watchdog := time.AfterFunc(3*time.Second, cancel)
			defer watchdog.Stop()
			var result struct {
				Claim *relay.Claim `json:"claim"`
			}
			if err := e.binding.Relay(parent, e.auth, "wait", map[string]any{"timeout_seconds": 1}, &result); err != nil || result.Claim != nil {
				t.Fatalf("public wait did not honor its own poll budget: %v", err)
			}
			if calls.Load() != 0 {
				t.Fatal("public wait behind a private lock reached HTTP")
			}
			deadlineNoJournal(t, e)
		})
	}
}

func TestEnterprisePublicWaitKeepsParentErrorsAndIdentityMismatch(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "public-errors", model.ActorSlot1, model.RuntimeClaude)
	f.approve(t, e)
	unlock := holdDeadlineIdentity(t, e)
	var calls atomic.Int32
	e.parent.http.Transport = deadlineRoundTrip(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected HTTP before identity admission")
	})
	parent, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	var result map[string]any
	err := e.binding.Relay(parent, e.auth, "wait", map[string]any{"timeout_seconds": 1}, &result)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || result != nil {
		t.Fatalf("public wait hid parent deadline: %v", err)
	}
	parent, cancel = context.WithCancel(t.Context())
	cancel()
	if err := e.binding.Relay(parent, e.auth, "wait", map[string]any{"timeout_seconds": 1}, &result); !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("public wait hid parent cancellation: %v", err)
	}
	unlock()
	claim := nativeidentity.Claim{Runtime: e.auth.Runtime, SessionID: e.auth.SessionID, Association: nativeidentity.Enterprise(f.server.URL, "dep-test", "room-native"), BindID: e.auth.BindID, Generation: e.auth.Generation}
	if err := nativeidentity.Release(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	if err := e.binding.Relay(t.Context(), e.auth, "wait", map[string]any{"timeout_seconds": 1}, &result); !errors.Is(err, relay.ErrAuth) || result != nil {
		t.Fatalf("public wait weakened identity rejection: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("public parent/auth failure reached HTTP")
	}
	deadlineNoJournal(t, e)
}

func TestEnterprisePublicWaitValidatesClosedInputBeforePrivateLocks(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "public-input", model.ActorSlot1, model.RuntimeClaude)
	f.approve(t, e)
	holdDeadlineDirectory(t, e.binding.dir)
	var calls atomic.Int32
	e.parent.http.Transport = deadlineRoundTrip(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("invalid wait reached HTTP")
	})
	for _, payload := range []map[string]any{
		{"timeout_seconds": 1, "attachment_ids": []string{"att-0123456789abcdef01234567"}},
		{"timeout_seconds": 1, "quote_id": "message-1"},
		{"timeout_seconds": 1, "review": map[string]any{"schema": 1}},
		{"timeout_seconds": 1, "unknown_field": true},
		{"timeout_seconds": 0}, {"timeout_seconds": 31}, {"timeout_seconds": "not-an-integer"},
	} {
		parent, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		var result map[string]any
		err := e.binding.Relay(parent, e.auth, "wait", payload, &result)
		cancel()
		if !errors.Is(err, ErrConfiguration) || result != nil {
			t.Fatalf("invalid wait was not rejected before private lock: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid public wait performed HTTP")
	}
	deadlineNoJournal(t, e)
}

func deadlineNoJournal(t *testing.T, e *nativeEndpoint) {
	t.Helper()
	j, err := e.binding.readDelivery()
	if err != nil || j.Version != 0 {
		t.Fatalf("read-only deadline created a delivery journal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.binding.dir, "delivery.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only deadline wrote a delivery file")
	}
}

func TestEnterpriseIdentityPollDeadlineIsEmptyBeforeHTTPOrClaim(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "identity-deadline", model.ActorSlot1, model.RuntimeClaude)
	f.approve(t, e)
	holdDeadlineIdentity(t, e)
	var calls atomic.Int32
	e.parent.http.Transport = deadlineRoundTrip(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected HTTP before identity check")
	})
	var result struct {
		Claim *relay.Claim `json:"claim"`
	}
	if err := e.binding.collect(t.Context(), e.auth, nativeRelayRequest{TimeoutSeconds: 1}, &result); err != nil || result.Claim != nil {
		t.Fatalf("own poll deadline before any effect must be empty: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("blocked identity check reached HTTP")
	}
	deadlineNoJournal(t, e)
}

func TestEnterpriseIdentityParentDeadlineRemainsDeadline(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "identity-parent-deadline", model.ActorSlot1, model.RuntimeClaude)
	f.approve(t, e)
	holdDeadlineIdentity(t, e)
	var calls atomic.Int32
	e.parent.http.Transport = deadlineRoundTrip(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected HTTP before identity check")
	})
	parent, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	var result map[string]any
	err := e.binding.collect(parent, e.auth, nativeRelayRequest{TimeoutSeconds: 1}, &result)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, relay.ErrAuth) || result != nil {
		t.Fatalf("parent deadline was changed into auth or idle: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("parent deadline reached HTTP")
	}
	deadlineNoJournal(t, e)
}

type identityCancelProbe struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (p *identityCancelProbe) Err() error {
	p.once.Do(func() { close(p.entered) })
	return p.Context.Err()
}

func TestEnterpriseAuthenticatedIdentityCancellationPreservesCause(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "identity-cancel", model.ActorSlot1, model.RuntimeClaude)
	f.approve(t, e)
	holdDeadlineIdentity(t, e)
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	probe := &identityCancelProbe{Context: parent, entered: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := e.binding.authenticated(probe, e.auth); done <- err }()
	// authenticated has no earlier Context use: this observes the actual
	// private-lock wait's cancellation check, rather than a scheduler delay.
	<-probe.entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || errors.Is(err, relay.ErrAuth) {
		t.Fatalf("identity lock cancellation lost its cause: %v", err)
	}
	var result map[string]any
	if err := e.binding.collect(parent, e.auth, nativeRelayRequest{TimeoutSeconds: 1}, &result); !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("parent cancellation became idle: %v", err)
	}
	deadlineNoJournal(t, e)
}

func TestEnterpriseIdentityMismatchStillRejectsAuthBeforeHTTP(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "identity-mismatch", model.ActorSlot1, model.RuntimeClaude)
	f.approve(t, e)
	var calls atomic.Int32
	e.parent.http.Transport = deadlineRoundTrip(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected HTTP after auth failure")
	})
	claim := nativeidentity.Claim{Runtime: e.auth.Runtime, SessionID: e.auth.SessionID, Association: nativeidentity.Enterprise(f.server.URL, "dep-test", "room-native"), BindID: e.auth.BindID, Generation: e.auth.Generation}
	if err := nativeidentity.Release(t.Context(), claim); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := e.binding.collect(t.Context(), e.auth, nativeRelayRequest{TimeoutSeconds: 1}, &result); !errors.Is(err, relay.ErrAuth) || result != nil {
		t.Fatalf("real ownership mismatch did not remain auth rejection: %v", err)
	}
	// A canceled context is not a reason to replace a genuine local credential
	// mismatch with a context error before its identity check has even begun.
	parent, cancel := context.WithCancel(t.Context())
	cancel()
	wrong := e.auth
	wrong.Secret = "wrong-local-credential"
	if _, err := e.binding.authenticated(parent, wrong); !errors.Is(err, relay.ErrAuth) {
		t.Fatalf("canceled context masked credential mismatch: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("auth mismatch reached HTTP")
	}
	deadlineNoJournal(t, e)
}

func TestEnterpriseIdentityDeadlineAfterPrefetchIsEmptyBeforeClaim(t *testing.T) {
	f, _, e, meta := attachmentEndpoints(t)
	base := e.parent.http.Transport
	var writes atomic.Int32
	e.parent.http.Transport = deadlineRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writes.Add(1)
		}
		return base.RoundTrip(r)
	})
	locked := make(chan struct{})
	f.mu.Lock()
	before := f.posts
	f.downloadAction = func(*nativeWireFixture) { holdDeadlineIdentity(t, e); close(locked) }
	f.mu.Unlock()
	var result struct {
		Claim *relay.Claim `json:"claim"`
	}
	if err := e.binding.collect(t.Context(), e.auth, nativeRelayRequest{TimeoutSeconds: 1}, &result); err != nil || result.Claim != nil {
		t.Fatalf("identity deadline after verified read escaped as auth/claim: %v", err)
	}
	select {
	case <-locked:
	default:
		t.Fatal("fixture did not reach actual attachment prefetch")
	}
	deadlineNoJournal(t, e)
	r, err := e.binding.read()
	if err != nil {
		t.Fatal(err)
	}
	if path, err := e.binding.cachedAttachment(t.Context(), r, meta); err != nil || path == "" {
		t.Fatal("successful verification read was not retained")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != before || writes.Load() != 0 || f.objectCalls["download"] != 1 {
		t.Fatal("post-prefetch identity deadline wrote or repeated an effect")
	}
}

func TestEnterpriseDeadlineAfterClaimJournalStaysUnknown(t *testing.T) {
	f := newNativeWireFixture(t)
	a := f.endpoint(t, "deadline-sender", model.ActorSlot1, model.RuntimeClaude)
	e := f.endpoint(t, "deadline-reader", model.ActorSlot2, model.RuntimeClaude)
	f.approve(t, a)
	f.approve(t, e)
	var sent relay.Message
	if err := a.binding.Relay(t.Context(), a.auth, "send", map[string]any{"id": "deadline-queued", "text": "pending claim must retain its original uncertainty", "expected_peer_generation": uint64(1)}, &sent); err != nil {
		t.Fatal(err)
	}
	base := e.parent.http.Transport
	var attempts atomic.Int32
	var original atomic.Value
	e.parent.http.Transport = deadlineRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/relay") {
			attempts.Add(1)
			j, err := e.binding.readDelivery()
			if err != nil || j.Phase != "claim_unknown" || j.OperationID == "" || j.BindingID != e.auth.BindID || j.Generation != e.auth.Generation {
				t.Error("claim request began without its original durable journal")
			}
			original.Store(j.OperationID)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return base.RoundTrip(r)
	})
	var result map[string]any
	err := e.binding.collect(t.Context(), e.auth, nativeRelayRequest{TimeoutSeconds: 1}, &result)
	var failure *Error
	if !errors.As(err, &failure) || failure.Outcome != "unknown" || result != nil {
		t.Fatalf("journaled claim deadline became idle or definite auth failure: %v", err)
	}
	j, journalErr := e.binding.readDelivery()
	if journalErr != nil || j.Phase != "claim_unknown" || j.OperationID != original.Load() || j.BindingID != e.auth.BindID || attempts.Load() != 1 {
		t.Fatal("claim uncertainty or original identity was lost/retried")
	}
}
