package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sean2077/pairroom/internal/updatecheck"
)

func TestUpdateEndpointAndEmbeddedAsset(t *testing.T) {
	// An unsupported build verifies real mux wiring without external traffic.
	previous := releaseChecker
	releaseChecker = updatecheck.New("dev")
	t.Cleanup(func() { releaseChecker = previous })
	mux := http.NewServeMux()
	Mount(mux)
	// The Management host also has a catch-all asset handler. Unsupported
	// methods must still reach the exact metadata boundary, not that fallback.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, updatecheck.Endpoint, nil))
	var result updatecheck.Result
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || result.Status != "unsupported" || result.CurrentVersion != "dev" {
		t.Fatalf("unexpected update response: %d %+v", w.Code, result)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("metadata escaped the API cache boundary")
	}
	for _, method := range []string{http.MethodPost, http.MethodHead, http.MethodDelete} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, updatecheck.Endpoint, nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s fell through the metadata boundary: %d", method, w.Code)
		}
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, Prefix+"updates.js", nil))
	if w.Code != http.StatusOK || w.Body.Len() == 0 {
		t.Fatalf("update UI was not embedded: %d", w.Code)
	}
}
