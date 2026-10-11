package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// Paths are quoted envelope fields on every platform, including Windows.
func lanEnvelopeHasLocalPath(envelope, path string) bool {
	return strings.Contains(envelope, "; path: "+strconv.Quote(path)+"\n")
}
func lanEnvelopeHasPathPrefix(envelope, prefix string) bool {
	quoted := strconv.Quote(prefix)
	return strings.Contains(envelope, prefix) || strings.Contains(envelope, quoted[1:len(quoted)-1])
}

func lanGuestTestService(t *testing.T) (*ManagementServer, Project) {
	t.Helper()
	registry, project := testRegistry(t, testGitRepo(t))
	provisioner := ProvisionerFunc(func(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error) {
		return Binding{}, nil, errors.New("fixture never starts native work")
	})
	manager, err := NewRuntimeManager(registry, EmbeddedRuntimeFactory(registry, EmbeddedRuntimeConfig{Mock: true}), RuntimeManagerConfig{Limit: 5, IdleTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewManagementServer(ManagementServerConfig{Registry: registry, Runtimes: manager, Provisioner: provisioner, Token: "local-management-secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("stop optional Service: %v", err)
		}
		cancel()
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			t.Errorf("stop optional runtime manager: %v", err)
		}
	})
	return server, project
}

func lanGuestTestRemote(t *testing.T, handler http.Handler) (lanshare.Invite, *httptest.Server) {
	t.Helper()
	identity, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	config, err := lanshare.ServerTLS(identity)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = config
	server.StartTLS()
	t.Cleanup(server.Close)
	pin, err := identity.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return lanshare.Invite{Version: 1, Endpoint: server.URL, HostPin: pin, RoomID: "shared-room", InviteID: "invite-one", ExpiresAt: time.Now().Add(time.Minute)}, server
}

// This scripted pinned host admits through the public protocol. The optional
// observer never fabricates or mutates a private client record in its fixture.
func lanAcceptedClient(t *testing.T, workspace string, kind model.RuntimeKind, handler http.Handler) (*lanclient.Store, *lanclient.Client, relay.Auth) {
	t.Helper()
	var mu sync.Mutex
	var admission lanshare.JoinResponse
	invite, _ := lanGuestTestRemote(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "join", "join-status":
			var req struct {
				InviteID  string            `json:"invite_id"`
				RequestID string            `json:"request_id"`
				Runtime   model.RuntimeKind `json:"runtime"`
				Label     string            `json:"label"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
				t.Error("invalid TLS join proof")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if admission.Room == nil {
				collaboration, err := (model.Collaboration{}).ForCreation()
				if err != nil {
					t.Error(err)
					return
				}
				admission = lanshare.JoinResponse{Status: "accepted", Receipt: lanshare.EncodeReceipt(lanshare.Receipt{RoomID: "shared-room", RequestID: req.RequestID, Fingerprint: lanshare.Fingerprint(r.TLS.PeerCertificates[0])}), Room: &lanshare.RoomInfo{RoomID: "shared-room", Name: "Observer Room", Slot: model.ActorSlot2, Generation: 7, BindID: "host-binding", Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: kind}, Collaboration: &collaboration}}
			}
			_ = json.NewEncoder(w).Encode(admission)
		default:
			if r.Header.Get("X-PairRoom-LAN-Bind") != "host-binding" || r.Header.Get("X-PairRoom-LAN-Generation") != "7" {
				t.Error("observer lost host membership scope")
			}
			handler.ServeHTTP(w, r)
		}
	}))
	store, err := lanclient.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	client, joined, err := store.Join(context.Background(), lanclient.JoinOptions{Invite: lanshare.EncodeInvite(invite), Workspace: workspace, Runtime: kind, SessionID: "guest-native-session", BindID: "guest-binding", CredentialHash: relay.Digest("guest-relay-secret")})
	if err != nil || joined.Status != "accepted" || joined.Binding == nil {
		t.Fatalf("public client admission: %+v, %v", joined, err)
	}
	return store, client, relay.Auth{Slot: joined.Binding.Slot, BindID: joined.Binding.BindID, Generation: joined.Binding.Generation, SessionID: "guest-native-session", Secret: "guest-relay-secret"}
}
