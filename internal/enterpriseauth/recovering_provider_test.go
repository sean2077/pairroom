package enterpriseauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type discoveryControl struct {
	available         atomic.Bool
	calls             atomic.Int32
	cancelled         atomic.Int32
	dropTokenResponse atomic.Bool
	codeEffects       atomic.Int32
	mu                sync.Mutex
	started           chan struct{}
	release           chan struct{}
}

func newRecoveringIDP(t *testing.T) (*testIDP, *discoveryControl) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	idp := &testIDP{key: key, otherKey: other, keyID: "test-key", codes: make(map[string]testCode), userInfoSubject: "subject-1"}
	control := &discoveryControl{}
	idp.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/openid-configuration" {
			control.calls.Add(1)
			if !control.available.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			control.mu.Lock()
			started, release := control.started, control.release
			control.mu.Unlock()
			if release != nil {
				select {
				case started <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
					control.cancelled.Add(1)
					return
				}
			}
		}
		if r.URL.Path == "/token" && control.dropTokenResponse.Load() {
			_ = r.ParseForm()
			idp.mu.Lock()
			idp.tokenCalls++
			_, ok := idp.codes[r.Form.Get("code")]
			delete(idp.codes, r.Form.Get("code"))
			idp.mu.Unlock()
			if ok {
				control.codeEffects.Add(1)
			}
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("test transport cannot drop token response")
				w.WriteHeader(500)
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		idp.serve(w, r)
	}))
	t.Cleanup(idp.server.Close)
	return idp, control
}

func recoveryDue(p *RecoveringProvider) {
	// Advance the private schedule deterministically; no wall-clock sleeps or
	// production-only clock hook are needed to reach the retry boundary.
	p.mu.Lock()
	p.nextAttempt = time.Time{}
	p.mu.Unlock()
}

func blockDiscovery(control *discoveryControl) (<-chan struct{}, chan struct{}) {
	control.mu.Lock()
	defer control.mu.Unlock()
	control.started = make(chan struct{}, 1)
	control.release = make(chan struct{})
	return control.started, control.release
}

func awaitDiscovery(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("discovery request did not reach fixture")
	}
}

func recoveringLogin(t *testing.T, p *RecoveringProvider, idp *testIDP, store TransactionStore) (browser, state, code string) {
	t.Helper()
	browser, err := NewBrowserSecret()
	if err != nil {
		t.Fatal(err)
	}
	auth, err := p.Begin(context.Background(), store, browser, LoginIntent{Purpose: PurposeLogin, ReturnPath: "/rooms"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(auth.URL)
	if err != nil {
		t.Fatal(err)
	}
	state = u.Query().Get("state")
	code = "code-" + state
	idp.mu.Lock()
	idp.codes[code] = testCode{nonce: u.Query().Get("nonce"), challenge: u.Query().Get("code_challenge")}
	idp.mu.Unlock()
	return browser, state, code
}

func TestRecoveringProviderStartsDegradedAndLaterFreshLoginSucceeds(t *testing.T) {
	idp, control := newRecoveringIDP(t)
	p, err := NewRecoveringProvider(context.Background(), idp.config())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.Ready() || control.calls.Load() != 1 {
		t.Fatal("initial outage did not yield one degraded discovery")
	}
	store := testMemoryStore(t, 2)
	browser, _ := NewBrowserSecret()
	// Pin the blocked side of the retry window; a request during it must make
	// no provider call and must not allocate a pending browser transaction.
	p.mu.Lock()
	p.nextAttempt = time.Now().Add(time.Hour)
	p.mu.Unlock()
	if _, err := p.Begin(context.Background(), store, browser, LoginIntent{Purpose: PurposeLogin, ReturnPath: "/rooms"}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("degraded begin: %v", err)
	}
	if control.calls.Load() != 1 || len(store.transactions) != 0 {
		t.Fatal("degraded begin retried early or allocated state")
	}
	control.available.Store(true)
	recoveryDue(p)
	browser, state, code := recoveringLogin(t, p, idp, store)
	result, err := p.Complete(context.Background(), store, browser, state, code)
	if err != nil || result.Identity.Subject != "subject-1" || !p.Ready() || control.calls.Load() != 2 {
		t.Fatalf("fresh login after discovery recovery: %+v %v", result, err)
	}
}

func TestRecoveringProviderConfigurationFailuresRemainFatal(t *testing.T) {
	idp, control := newRecoveringIDP(t)
	cfg := idp.config()
	cfg.ClientID = ""
	if p, err := NewRecoveringProvider(context.Background(), cfg); !errors.Is(err, ErrConfiguration) || p != nil {
		t.Fatalf("configuration error softened: %v", err)
	}
	if control.calls.Load() != 0 {
		t.Fatal("invalid operator configuration contacted IdP")
	}
	control.available.Store(true)
	idp.discoveryModify = func(metadata map[string]any) { metadata["token_endpoint"] = "https://unapproved.example.test/token" }
	if p, err := NewRecoveringProvider(context.Background(), idp.config()); !errors.Is(err, ErrConfiguration) || p != nil {
		t.Fatalf("unsafe discovery softened: %v", err)
	}
}

func TestRecoveringProviderSnapshotsAllMutableConfiguration(t *testing.T) {
	idp, control := newRecoveringIDP(t)
	cfg := idp.config()
	p, err := NewRecoveringProvider(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	cfg.SigningAlgorithms[0] = "none"
	cfg.AllowedReturnPaths[0] = "/retarget"
	cfg.AllowedReturnPaths[1] = "/retarget-2"
	cfg.Outbound.AllowedOrigins[0] = "https://unapproved.example.test"
	template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "Unrelated test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &idp.otherKey.PublicKey, idp.otherKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Outbound.RootCAs.AddCert(ca)
	if cfg.Outbound.RootCAs.Equal(p.cfg.Outbound.RootCAs) {
		t.Fatal("caller mutation changed frozen CA trust")
	}
	control.available.Store(true)
	recoveryDue(p)
	store := testMemoryStore(t, 1)
	browser, state, code := recoveringLogin(t, p, idp, store)
	if _, err := p.Complete(context.Background(), store, browser, state, code); err != nil {
		t.Fatalf("caller retargeted recovery configuration: %v", err)
	}
}

func TestRecoveringProviderConcurrentLoginAttemptsShareDiscoveryAndCancelIndependently(t *testing.T) {
	idp, control := newRecoveringIDP(t)
	p, err := NewRecoveringProvider(context.Background(), idp.config())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	control.available.Store(true)
	recoveryDue(p)
	started, release := blockDiscovery(control)
	store := testMemoryStore(t, 64)
	browser, _ := NewBrowserSecret()
	intent := LoginIntent{Purpose: PurposeLogin, ReturnPath: "/rooms"}
	waiter, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := make(chan error, 1)
	go func() { _, err := p.Begin(waiter, store, browser, intent); cancelled <- err }()
	awaitDiscovery(t, started)
	const callers = 24
	results := make(chan error, callers)
	for range callers {
		go func() { _, err := p.Begin(context.Background(), store, browser, intent); results <- err }()
	}
	cancel()
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}
	if control.calls.Load() != 2 || control.cancelled.Load() != 0 {
		t.Fatal("caller cancellation cancelled/repeated shared discovery")
	}
	close(release)
	for range callers {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("shared discovery caller: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("shared discovery caller did not finish")
		}
	}
	if control.calls.Load() != 2 || !p.Ready() {
		t.Fatal("concurrent callers created extra provider requests")
	}
}

func TestRecoveringProviderCloseCancelsDiscoveryWithoutLatePublication(t *testing.T) {
	idp, control := newRecoveringIDP(t)
	cfg := idp.config()
	// A long request budget ensures the five-second assertion below proves
	// active Close cancellation, rather than accidentally waiting for timeout.
	cfg.Outbound.Timeout = time.Minute
	p, err := NewRecoveringProvider(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	control.available.Store(true)
	recoveryDue(p)
	started, release := blockDiscovery(control)
	t.Cleanup(func() { close(release) })
	browser, _ := NewBrowserSecret()
	store := testMemoryStore(t, 1)
	result := make(chan error, 1)
	go func() {
		_, err := p.Begin(context.Background(), store, browser, LoginIntent{Purpose: PurposeLogin, ReturnPath: "/rooms"})
		result <- err
	}()
	awaitDiscovery(t, started)
	p.Close()
	p.Close()
	select {
	case err := <-result:
		if !errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("closed discovery: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel discovery")
	}
	if p.Ready() {
		t.Fatal("provider was published after Close")
	}
	if _, err := p.Begin(context.Background(), store, browser, LoginIntent{Purpose: PurposeLogin, ReturnPath: "/rooms"}); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("closed provider restarted: %v", err)
	}
	if control.calls.Load() != 2 {
		t.Fatal("closed wrapper issued another discovery")
	}
}

func TestRecoveringProviderConstructorCancellationDoesNotStartDegradedService(t *testing.T) {
	idp, control := newRecoveringIDP(t)
	control.available.Store(true)
	started, release := blockDiscovery(control)
	t.Cleanup(func() { close(release) })
	cfg := idp.config()
	cfg.Outbound.Timeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		p, err := NewRecoveringProvider(ctx, cfg)
		if p != nil {
			p.Close()
			result <- errors.New("cancelled constructor returned a provider")
			return
		}
		result <- err
	}()
	awaitDiscovery(t, started)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("constructor cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("constructor cancellation did not return")
	}
}

func TestRecoveringProviderRetainsDiscoveryAndNeverRetriesUnknownCodeExchange(t *testing.T) {
	idp, control := newRecoveringIDP(t)
	control.available.Store(true)
	p, err := NewRecoveringProvider(context.Background(), idp.config())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	store := testMemoryStore(t, 2)
	browser, state, code := recoveringLogin(t, p, idp, store)
	control.available.Store(false)
	control.dropTokenResponse.Store(true)
	if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("unknown exchange: %v", err)
	}
	if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatalf("unknown exchange state was reusable: %v", err)
	}
	if !p.Ready() || control.calls.Load() != 1 || control.codeEffects.Load() != 1 || idp.tokenCalls != 1 {
		t.Fatal("code exchange retried or successful discovery discarded")
	}
	if _, err := p.Begin(context.Background(), store, browser, LoginIntent{Purpose: PurposeLogin, ReturnPath: "/rooms"}); err != nil {
		t.Fatalf("successful discovery lost during token outage: %v", err)
	}
	if control.calls.Load() != 1 {
		t.Fatal("token failure caused automatic metadata refresh")
	}
}

func TestRecoveringProviderInvalidInputCannotTriggerDiscovery(t *testing.T) {
	idp, control := newRecoveringIDP(t)
	p, err := NewRecoveringProvider(context.Background(), idp.config())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	control.available.Store(true)
	recoveryDue(p)
	store := testMemoryStore(t, 1)
	browser, _ := NewBrowserSecret()
	if _, err := p.Begin(context.Background(), store, browser, LoginIntent{Purpose: PurposeLogin, ReturnPath: "/unlisted"}); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatalf("unlisted return: %v", err)
	}
	if _, err := p.Begin(context.Background(), store, "not-a-browser", LoginIntent{Purpose: PurposeLogin, ReturnPath: "/rooms"}); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatalf("invalid browser: %v", err)
	}
	if _, err := p.Complete(context.Background(), store, browser, "bad-state", "code"); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatalf("invalid state: %v", err)
	}
	if control.calls.Load() != 1 {
		t.Fatal("invalid callback caused discovery")
	}
}
