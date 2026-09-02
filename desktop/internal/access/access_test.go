package access

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/daemon"
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
		"http://127.0.0.1:7332/?#token=secret",
		"http://127.0.0.1:7332/?token=secret",
		"http://127.0.0.1:7332/#token=one&token=two",
		"http://user:pass@127.0.0.1:7332/#token=secret",
	} {
		if _, err := Parse(value); err == nil {
			t.Fatalf("unsafe Management URL was accepted: %q", value)
		}
	}
}

func TestProbeDataRootRequiresTheExpectedOwner(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pairroom")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer root-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data_root":%q}`, root)
	}))
	defer server.Close()

	value, err := Parse(server.URL + "/#token=root-secret")
	if err != nil {
		t.Fatal(err)
	}
	if !ProbeDataRoot(context.Background(), value, filepath.Join(root, ".")) {
		t.Fatal("matching data root was rejected")
	}
	if ProbeDataRoot(context.Background(), value, filepath.Join(t.TempDir(), "other")) {
		t.Fatal("different data root was accepted")
	}
}

func TestProbeDataRootStreamsLargeServiceSnapshots(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pairroom")
	largeProjects := strings.Repeat(`{"id":"project","name":"padding"},`, 4000)
	body := `{"projects":[` + strings.TrimSuffix(largeProjects, ",") + `],"data_root":` + fmt.Sprintf("%q", root) + `}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	value, err := Parse(server.URL + "/#token=unused")
	if err != nil {
		t.Fatal(err)
	}
	if !ProbeDataRoot(context.Background(), value, root) {
		t.Fatal("data root was not found in a large service snapshot")
	}
}

func TestResolveDaemonLogFileUsesPersistedWorkDirForLegacyRelativeMetadata(t *testing.T) {
	workDir := t.TempDir()
	meta := &daemon.Meta{WorkDir: workDir, LogFile: filepath.Join("logs", "service.log")}
	path, err := resolveDaemonLogFile(meta)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(workDir, "logs", "service.log")
	if path != want {
		t.Fatalf("resolved log path = %q, want %q", path, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("test log unexpectedly exists: %v", err)
	}
}
