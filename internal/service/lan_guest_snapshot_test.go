package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatelock"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func TestLANManagementSnapshotCancelsCatalogLockWithoutLosingHostedProjects(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	server, project := lanGuestTestService(t)
	store, client, _ := lanAcceptedClient(t, project.Root, model.RuntimeGrok, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	metadata, err := client.Metadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := privatelock.Lock(context.Background(), filepath.Join(store.Root(), metadata.ID))
	if err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(unlock)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/service", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.readService(response, request)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		release()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("snapshot did not finish after releasing its client lock")
		}
		t.Fatal("catalog diagnostic ignored the cancelled request while waiting for a private client lock")
	}
	var snapshot ServiceSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || !snapshot.Healthy || len(snapshot.Projects) != 1 || snapshot.Projects[0].ID != project.ID {
		t.Fatalf("joined catalog lock hid healthy hosted state: %+v", snapshot)
	}
	if len(snapshot.JoinedRooms) != 0 || !strings.Contains(snapshot.Diagnostic, context.Canceled.Error()) {
		t.Fatalf("cancelled catalog read lost its bounded diagnostic: %+v", snapshot)
	}
	release()
	response = httptest.NewRecorder()
	server.readService(response, httptest.NewRequest(http.MethodGet, "/api/v1/service", nil))
	snapshot = ServiceSnapshot{}
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.JoinedRooms) != 1 || snapshot.JoinedRooms[0].ID != metadata.ID || snapshot.Diagnostic != "" || !snapshot.Healthy {
		t.Fatalf("released catalog did not recover on the next snapshot: %+v", snapshot)
	}
}
