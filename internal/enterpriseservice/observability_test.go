package enterpriseservice

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

type blockingResourceProvider struct {
	*fakeProvider
	started chan struct{}
	release chan struct{}
}

func (p *blockingResourceProvider) Begin(ctx context.Context, store enterpriseauth.TransactionStore, browser string, intent enterpriseauth.LoginIntent) (enterpriseauth.Authorization, error) {
	close(p.started)
	select {
	case <-p.release:
		return p.fakeProvider.Begin(ctx, store, browser, intent)
	case <-ctx.Done():
		return enterpriseauth.Authorization{}, ctx.Err()
	}
}

func TestHTTPResourceAdmissionAndDrainDoNotReachBusinessBackend(t *testing.T) {
	h, p, _ := newHandlerFixture(t)
	provider := &blockingResourceProvider{fakeProvider: p, started: make(chan struct{}), release: make(chan struct{})}
	h.provider = provider
	h.resources = newRequestResources(ResourceLimits{ConcurrentRequests: 1})
	done := make(chan int, 1)
	go func() { done <- request(t, h, http.MethodGet, "/enterprise/auth/login", "", nil, "").Code }()
	<-provider.started
	w := request(t, h, http.MethodGet, "/enterprise/auth/login", "", nil, "")
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" || h.resources.active.Load() != 1 {
		t.Fatal("saturated HTTP reached business provider")
	}
	h.BeginDrain()
	if w := request(t, h, http.MethodGet, "/enterprise/health/live", "", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"draining":true`) {
		t.Fatal("drain killed metadata liveness")
	}
	if w := request(t, h, http.MethodGet, "/enterprise/health/ready", "", nil, ""); w.Code != 503 {
		t.Fatal("draining instance advertised readiness")
	}
	close(provider.release)
	if <-done != 303 {
		t.Fatal("drain discarded accepted operation")
	}
	if h.resources.active.Load() != 0 {
		t.Fatal("request retained capacity after completion")
	}
}

type observableBackend struct {
	*fakeAdministrationBackend
	readiness      sqlstore.Readiness
	err            error
	readinessCalls int
}

func (b *observableBackend) Readiness(context.Context) (sqlstore.Readiness, error) {
	b.readinessCalls++
	return b.readiness, b.err
}

func TestHealthUsesActualSQLMetadataAndRedactsFailures(t *testing.T) {
	h, base, _, _ := newAdministrationHTTPFixture(t)
	b := &observableBackend{fakeAdministrationBackend: base, readiness: sqlstore.Readiness{SchemaCompatible: true, OwnershipCurrent: true, RecoveryComplete: true, RestoreClear: true}}
	h.backend = b
	if w := request(t, h, "GET", "/enterprise/health/live", "", nil, ""); w.Code != 200 || b.readinessCalls != 0 {
		t.Fatal("liveness contacted SQL or IdP")
	}
	if w := request(t, h, "GET", "/enterprise/health/ready", "", nil, ""); w.Code != 200 || b.readinessCalls != 1 {
		t.Fatal("readiness skipped fenced SQL state")
	}
	b.readiness.RestoreClear = false
	if w := request(t, h, "GET", "/enterprise/health/ready", "", nil, ""); w.Code != 503 {
		t.Fatal("pending restore advertised readiness")
	}
	b.err = errors.New("secret-token private-user C:\\private\\session task-text")
	w := request(t, h, "GET", "/enterprise/health/ready", "", nil, "")
	if w.Code != 503 || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "private") {
		t.Fatal("health leaked raw backend error")
	}
	before := b.readinessCalls
	for _, path := range []string{"/enterprise/health/ready?room=private", "/enterprise/health/ready?"} {
		if w := request(t, h, "GET", path, "", nil, ""); w.Code != 400 {
			t.Fatal("health accepted query authority")
		}
	}
	if b.readinessCalls != before {
		t.Fatal("invalid health request reached SQL")
	}
}

func TestDiagnosticsAndMetricsUseOnlyAllowlistAndCurrentAdministrator(t *testing.T) {
	h, b, cookie, _ := newAdministrationHTTPFixture(t)
	for _, surface := range []string{"diagnostics", "metrics"} {
		before := b.reads
		w := request(t, h, "GET", administrationPrefix+surface, "", cookie, "")
		if w.Code != 200 || b.reads != before+1 || b.query.Resource != "users" || b.query.Limit != 1 {
			t.Fatal("observability skipped current fenced administrator check")
		}
		for _, secret := range []string{"user-1", "deployment-1", "session_id", "room_id", "token", "C:\\", "task-text", "credential"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("observability leaked %s", secret)
			}
		}
	}
	for hash, s := range b.sessions {
		s.Administrator = false
		b.sessions[hash] = s
	}
	before := b.reads
	for _, surface := range []string{"diagnostics", "metrics"} {
		if w := request(t, h, "GET", administrationPrefix+surface, "", cookie, ""); w.Code != 403 {
			t.Fatal("nonadmin received observability")
		}
	}
	if b.reads != before {
		t.Fatal("nonadmin reached diagnostics authority lookup")
	}
}
