package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/store"
)

func TestNativeHealthReflectsFatalStateWithoutLeakingDetails(t *testing.T) {
	for _, failure := range []string{"healthy", "writer", "listener"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			log, err := store.Open(root)
			if err != nil {
				t.Fatal(err)
			}
			e, err := relay.Open(relay.Config{RoomID: "room", Store: log})
			if err != nil {
				_ = log.Close()
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Close() })
			n := &nativeHostRuntime{engine: e}
			switch failure {
			case "writer":
				if err := log.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := e.SendUser(relay.SendRequest{ID: "fail", To: model.ActorSlot1, Text: "private-test-detail"}); err == nil {
					t.Fatal("closed writer accepted publication")
				}
			case "listener":
				err := errors.New("private-test-detail: " + root)
				n.serveFatal.Store(&err)
			}
			sequence := e.Sequence()
			w := httptest.NewRecorder()
			n.serve(w, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
			var health struct {
				OK       bool           `json:"ok"`
				HostMode model.HostMode `json:"host_mode"`
				Code     string         `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil {
				t.Fatal(err)
			}
			if health.HostMode != model.HostNative || e.Sequence() != sequence {
				t.Fatal("health lost its host boundary or mutated relay state")
			}
			if failure == "healthy" {
				if w.Code != http.StatusOK || !health.OK || health.Code != "" {
					t.Fatalf("healthy relay: %d %s", w.Code, w.Body.String())
				}
			} else if w.Code != http.StatusServiceUnavailable || health.OK || health.Code != "runtime_not_ready" {
				t.Fatalf("fatal relay appeared healthy: %d %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "private-test-detail") || strings.Contains(w.Body.String(), root) {
				t.Fatal("health leaked a raw runtime error")
			}
		})
	}
}

func TestNativeHealthStillRequiresRoomAuthentication(t *testing.T) {
	f := nativeHTTP(t)
	w := httptest.NewRecorder()
	f.native.boundary(http.HandlerFunc(f.native.serve)).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated health: %d", w.Code)
	}
}
