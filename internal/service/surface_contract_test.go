package service

import (
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Exercise the maintained allowlist against the actual Room entry point rather
// than a second hard-coded asset list. Adding a script must not work standalone
// but silently fail through Management's authenticated iframe gateway.
func TestSurfaceAllowsEveryRoomEntryPointAsset(t *testing.T) {
	page, err := os.ReadFile("../server/assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	assets := regexp.MustCompile(`<(?:script|link)\b[^>]*\b(?:src|href)="([^"]+)"`).FindAllSubmatch(page, -1)
	if len(assets) == 0 {
		t.Fatal("Room entry point has no external assets; contract probe found nothing")
	}
	for _, match := range assets {
		asset, err := url.Parse(string(match[1]))
		if err != nil || asset.IsAbs() || asset.Host != "" {
			t.Fatalf("unexpected external Room asset: %q", match[1])
		}
		path := "/" + strings.TrimPrefix(asset.Path, "/")
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			if !allowedSurfaceRequest(method, path) {
				t.Errorf("gateway blocks Room asset: %s %s", method, path)
			}
		}
		if allowedSurfaceRequest(http.MethodPost, path) {
			t.Errorf("gateway allows a write to static asset %s", path)
		}
	}
}

func TestSurfaceParticipantControlsMatchRoomAPI(t *testing.T) {
	for _, test := range []struct {
		method, path string
		allowed      bool
	}{
		{http.MethodPut, "/api/v1/participants/claude/permissions", true},
		{http.MethodPut, "/api/v1/participants/codex/permissions", true},
		{http.MethodPost, "/api/v1/participants/claude/permissions", false},
		{http.MethodGet, "/api/v1/participants/claude/permissions", false},
		{http.MethodPut, "/api/v1/participants/codex/role", false},
		{http.MethodPost, "/api/v1/participants/codex/role", false},
		{http.MethodPost, "/api/v1/participants/codex/start", true},
		{http.MethodPost, "/api/v1/participants/codex/stop", true},
		{http.MethodPost, "/api/v1/participants/codex/restart", true},
		{http.MethodPost, "/api/v1/participants/codex/interrupt", true},
		{http.MethodPost, "/api/v1/participants/codex/START", true},
		{http.MethodPut, "/api/v1/participants/codex/start", false},
		{http.MethodPost, "/api/v1/participants/codex/unknown", false},
		{http.MethodPost, "/api/v1/participants/codex/nested/start", false},
		{http.MethodPut, "/api/v1/participants/codex/nested/permissions", false},
		{http.MethodPost, "/api/v1/participants//start", false},
		{http.MethodPost, "/api/v1/participants/codex", false},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			if got := allowedSurfaceRequest(test.method, test.path); got != test.allowed {
				t.Fatalf("allowed=%v, want %v", got, test.allowed)
			}
		})
	}
}
