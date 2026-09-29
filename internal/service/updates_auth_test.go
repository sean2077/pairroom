package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUpdateChecksUseManagementAuthentication(t *testing.T) {
	registry, _ := testRegistry(t, testGitRepo(t))
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})
	for _, tc := range []struct {
		name, method, token string
		want                int
	}{
		{"signed out", http.MethodGet, "", http.StatusUnauthorized},
		{"scoped relay token", http.MethodGet, server.cliToken, http.StatusForbidden},
		{"wrong token", http.MethodGet, "not-the-token", http.StatusUnauthorized},
		{"not a mutation API", http.MethodPost, server.Token(), http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := managementRequest(tc.method, "/api/v1/updates?refresh=1", "", false)
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("update API response may be cached by the browser")
			}
		})
	}
}
