package access

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseProbeAndDesktopURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/service" || r.Header.Get("Authorization") != "Bearer local-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	value := server.URL + "/#token=local-secret"
	access, err := Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	if !Probe(context.Background(), access) {
		t.Fatal("authenticated loopback endpoint did not probe successfully")
	}
	if got := access.DesktopURL(); !strings.Contains(got, "?desktop=1#token=local-secret") {
		t.Fatalf("desktop URL = %q", got)
	}
	if !EqualToken(access.Token, "local-secret") || EqualToken(access.Token, "other") {
		t.Fatal("constant-time token equality returned an unexpected result")
	}
}

func TestParseRejectsUnsafeManagementURLs(t *testing.T) {
	for _, value := range []string{
		"https://127.0.0.1:7332/#token=secret",
		"http://example.com:7332/#token=secret",
		"http://localhost:7332/#token=secret",
		"http://127.0.0.1:7332/",
		"http://127.0.0.1:7332/?token=secret",
		"http://127.0.0.1:7332/#token=one&token=two",
		"http://user:pass@127.0.0.1:7332/#token=secret",
	} {
		if _, err := Parse(value); err == nil {
			t.Fatalf("unsafe Management URL was accepted: %q", value)
		}
	}
}
