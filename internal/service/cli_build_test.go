package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/relay"
)

func TestCLIBuildBannerRequiresStableAuthenticatedObservations(t *testing.T) {
	const running = "v5.7.1+2.abcdef0"
	o := newCLIBuildObserver(running)
	now := time.Now()
	other := "v5.8.0+1234567"
	o.observe(other, now)
	if o.snapshot() != nil {
		t.Fatal("single report flashed a banner")
	}
	o.observe(other, now.Add(time.Second))
	if o.snapshot() != nil {
		t.Fatal("one CLI invocation's request burst counted as stability")
	}
	o.observe(other, now.Add(cliBuildStabilityWindow))
	if got := o.snapshot(); got == nil || got.CLI != other || got.Service != running {
		t.Fatalf("missing stable mismatch: %+v", got)
	}
	o.observe("v5.7.1+3.1234567", now.Add(time.Minute))
	if o.snapshot() != nil {
		t.Fatal("alternating builds retained another CLI's banner")
	}
	o.observe("v5.7.1+3.1234567", now.Add(2*time.Minute))
	if o.snapshot() == nil {
		t.Fatal("same-release build difference hidden")
	}
	o.observe(running, now.Add(3*time.Minute))
	if o.snapshot() != nil {
		t.Fatal("matching CLI did not clear banner")
	}
	o.observe("secret\ninvalid", now.Add(4*time.Minute))
	if o.latest != "" || o.snapshot() != nil {
		t.Fatal("invalid text retained")
	}
	if newCLIBuildObserver("v5.7.1") != nil {
		t.Fatal("unstamped Service compared builds")
	}
}

func TestCLIBuildHeaderCannotPoisonStateBeforeAuthentication(t *testing.T) {
	f := nativeHTTP(t)
	// The test server's actual auth middleware, with a deterministic stamped baseline.
	// Obtain a separate Management handler using the fixture's real Registry/runtime.
	s, err := NewManagementServer(ManagementServerConfig{Registry: f.registry, Runtimes: f.manager, Provisioner: SyntheticProvisioner{}, Token: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	s.cliBuild = newCLIBuildObserver("v5.7.1+abcdef0")
	request := func(path, auth, build string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		req.Header.Set(relay.CLIBuildHeader, build)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w.Code
	}
	for _, auth := range []string{"", "Bearer wrong"} {
		if status := request("/api/v1/service", auth, "v5.8.0+1234567"); status != http.StatusUnauthorized {
			t.Fatal(status)
		}
		if s.cliBuild.latest != "" {
			t.Fatal("unauthenticated report retained")
		}
	}
	if status := request("/api/v1/service", "Bearer owner", "v5.8.0+1234567"); status != http.StatusOK {
		t.Fatal(status)
	}
	if s.cliBuild.latest != "v5.8.0+1234567" {
		t.Fatal("authenticated report missing")
	}
	before := s.cliBuild.latest
	request("/api/v1/service", "Bearer owner", strings.Repeat("a", 200))
	if s.cliBuild.latest != before {
		t.Fatal("invalid report replaced valid observation")
	}
	s.cliBuild.observe(before, time.Now().Add(time.Minute))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/service", nil)
	req.Header.Set("Authorization", "Bearer owner")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var snap ServiceSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil || snap.CLIBuildMismatch == nil {
		t.Fatalf("snapshot missing observation: %v", err)
	}
	if strings.Contains(w.Body.String(), strings.Repeat("a", 200)) {
		t.Fatal("invalid metadata reflected")
	}
}

func TestRelayBuildObservationRequiresCurrentBinding(t *testing.T) {
	f := nativeHTTP(t)
	a := f.bind(t, "slot1")
	s, err := NewManagementServer(ManagementServerConfig{Registry: f.registry, Runtimes: f.manager, Provisioner: SyntheticProvisioner{}, Token: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	s.cliBuild = newCLIBuildObserver("v5.7.1+abcdef0")
	post := func(secret string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/relay/"+f.room.ID+"/slot1/summary", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Relay "+secret)
		req.Header.Set("X-PairRoom-Bind", a.BindID)
		req.Header.Set("X-PairRoom-Generation", "1")
		req.Header.Set("X-PairRoom-Session", a.SessionID)
		req.Header.Set(relay.CLIBuildHeader, "v5.8.0+1234567")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w.Code
	}
	if post("wrong") != http.StatusUnauthorized || s.cliBuild.latest != "" {
		t.Fatal("rejected relay altered build observation")
	}
	if post(a.Secret) != http.StatusOK || s.cliBuild.latest == "" {
		t.Fatal("authenticated relay did not observe build")
	}
}
