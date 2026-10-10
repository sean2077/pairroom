package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

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
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = manager.Shutdown(ctx)
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

func admitLANGuestFixture(t *testing.T, s *ManagementServer, project Project, invite lanshare.Invite) (*lanGuest, relay.Auth) {
	t.Helper()
	request := lanLocalJoinRequest{Workspace: project.Root, Runtime: model.RuntimeGrok, SessionID: "official-guest-session", BindID: "local-binding", CredentialHash: relay.Digest("local-relay-secret")}
	guest, err := s.lanGuests.prepare(invite, request)
	if err != nil {
		t.Fatal(err)
	}
	// This fixture exercises foreground delivery independently of background
	// wake scheduling, which has its own reservation/effect tests.
	s.lanGuests.cancel()
	key, err := guest.record.Identity.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	collaboration, err := (model.Collaboration{}).ForCreation()
	if err != nil {
		t.Fatal(err)
	}
	admission := lanshare.JoinResponse{Status: "accepted", Receipt: lanshare.EncodeReceipt(lanshare.Receipt{RoomID: invite.RoomID, RequestID: guest.record.RequestID, Fingerprint: key}), Room: &lanshare.RoomInfo{RoomID: invite.RoomID, Name: "Shared test", Slot: model.ActorSlot2, Generation: 7, BindID: "remote-binding", Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeGrok}, Collaboration: &collaboration}}
	if err := s.lanGuests.updateAdmission(guest, admission); err != nil {
		t.Fatal(err)
	}
	return guest, relay.Auth{Slot: model.ActorSlot2, BindID: request.BindID, Generation: 7, SessionID: request.SessionID, Secret: "local-relay-secret"}
}

func TestLANGuestPrefetchFinishesBeforeClaimAndRendersPrivateLocalPath(t *testing.T) {
	service, project := lanGuestTestService(t)
	hostDir := t.TempDir()
	store, err := attachment.Open(hostDir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const evidence = "#!/bin/sh\nprintf 'reproduce the observed bug\\n'\n"
	metadata, err := store.SaveEvidence("repro.sh", strings.NewReader(evidence), "lan")
	if err != nil {
		t.Fatal(err)
	}
	message := relay.Message{ID: "incoming-one", From: model.ActorSlot1, To: model.ActorSlot2, Text: "Please inspect this repro before running it.", Attachments: []model.Attachment{metadata}, TargetGeneration: 7, State: "queued", Source: "explicit", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	downloadStarted, releaseDownload := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var operations []string
	invitation, _ := lanGuestTestRemote(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := filepath.Base(r.URL.Path)
		mu.Lock()
		operations = append(operations, action)
		mu.Unlock()
		if r.Header.Get("X-PairRoom-LAN-Bind") != "remote-binding" || r.Header.Get("X-PairRoom-LAN-Generation") != "7" || r.Header.Get("Authorization") != "" || r.Header.Get("X-PairRoom-Session") != "" {
			t.Error("LAN operation inherited local credentials or lost accepted membership scope")
		}
		if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
			t.Error("LAN action did not prove the per-Room client key")
		}
		switch action {
		case "head":
			_ = json.NewEncoder(w).Encode(lanshare.HeadResponse{Head: &lanshare.Head{Message: message, Digest: relay.Digest("accepted-manifest")}})
		case "download":
			close(downloadStarted)
			<-releaseDownload
			_, _ = io.WriteString(w, evidence)
		case "claim":
			var req lanshare.ClaimRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID != message.ID || req.Digest != relay.Digest("accepted-manifest") || req.Generation != 7 {
				t.Error("claim did not identify the exact prepared message")
			}
			claimed := message
			claimed.State = "delivering"
			claimed.ClaimedAt = time.Now().UTC()
			_ = json.NewEncoder(w).Encode(lanshare.ClaimResponse{Claim: &lanshare.Claim{ID: message.ID, Receipt: "original-receipt", Message: claimed}})
		default:
			t.Errorf("foreground bridge unexpectedly called %s", action)
			w.WriteHeader(404)
		}
	}))
	guest, auth := admitLANGuestFixture(t, service, project, invitation)
	output := httptest.NewRecorder()
	finished := make(chan error, 1)
	go func() {
		finished <- guest.collect(context.Background(), output, lanGuestRelayRequest{TimeoutSeconds: 30}, auth)
	}()
	select {
	case <-downloadStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("guest did not prefetch evidence")
	}
	mu.Lock()
	if strings.Join(operations, ",") != "head,download" {
		t.Errorf("an inbox claim or acknowledgement preceded complete evidence: %v", operations)
	}
	mu.Unlock()
	close(releaseDownload)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	var result struct {
		Claim *relay.Claim `json:"claim"`
	}
	if err := json.Unmarshal(output.Body.Bytes(), &result); err != nil || result.Claim == nil || result.Claim.Receipt != "original-receipt" {
		t.Fatalf("original receipt was not forwarded: %s %v", output.Body.String(), err)
	}
	_, localPath, err := guest.media.Resolve(metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(localPath)
	if err != nil || string(actual) != evidence || !strings.Contains(result.Claim.Envelope, localPath) || strings.Contains(result.Claim.Envelope, hostDir) {
		t.Fatal("guest did not render verified actual bytes at its own private path")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(operations, ",") != "head,download,claim" {
		t.Fatalf("network delivery acknowledged before collector stdout: %v", operations)
	}
}

func TestLANGuestBridgeStripsPrivateConfirmationAndMapsPublicationIdentity(t *testing.T) {
	service, project := lanGuestTestService(t)
	invitation, _ := lanGuestTestRemote(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		for _, private := range []string{"official-guest-session", "private-native-transcript", "local-relay-secret", "local-management-secret"} {
			if strings.Contains(string(data), private) || strings.Contains(r.Header.Get("Authorization"), private) {
				t.Errorf("remote request disclosed %s", private)
			}
		}
		switch filepath.Base(r.URL.Path) {
		case "confirm":
			_ = json.NewEncoder(w).Encode(relay.Binding{Slot: model.ActorSlot2, BindID: "remote-binding", Generation: 7, Active: true})
		case "report":
			_ = json.NewEncoder(w).Encode(relay.Publication{BindID: "remote-binding", Generation: 7, ReportSeq: 12})
		default:
			t.Error("unexpected remote action")
		}
	}))
	guest, auth := admitLANGuestFixture(t, service, project, invitation)
	confirmation := httptest.NewRecorder()
	if err := guest.relayAction(context.Background(), confirmation, "confirm", lanGuestRelayRequest{SessionID: auth.SessionID, TranscriptPath: filepath.Join(project.Root, "private-native-transcript.jsonl")}, auth); err != nil {
		t.Fatal(err)
	}
	var binding relay.Binding
	if err := json.Unmarshal(confirmation.Body.Bytes(), &binding); err != nil || binding.BindID != "local-binding" || binding.SessionID != auth.SessionID || binding.Generation != 7 {
		t.Fatal("local confirmation lost its own native identity")
	}
	response := httptest.NewRecorder()
	if err := guest.relayAction(context.Background(), response, "report", lanGuestRelayRequest{ReportSeq: 12, Text: "@claude reply"}, auth); err != nil {
		t.Fatal(err)
	}
	var publication relay.Publication
	if err := json.Unmarshal(response.Body.Bytes(), &publication); err != nil || publication.BindID != "local-binding" || publication.Generation != 7 || publication.ReportSeq != 12 {
		t.Fatalf("WAL receipt was not mapped to the verified local binding: %s", response.Body.String())
	}
}

func TestLANGuestReloadPreservesRoomKeyAndRejectsHostedSessionCollision(t *testing.T) {
	service, project := lanGuestTestService(t)
	invitation, _ := lanGuestTestRemote(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	guest, auth := admitLANGuestFixture(t, service, project, invitation)
	var stored lanGuestRecord
	if err := privatefile.ReadJSON(filepath.Join(guest.dir, "guest.json"), 2<<20, &stored); err != nil {
		t.Fatal(err)
	}
	key, _ := stored.Identity.Fingerprint()
	service.lanGuests.close()
	if err := initLANGuests(service); err != nil {
		t.Fatal(err)
	}
	service.lanGuests.cancel()
	reloaded := service.lanGuests.get(stored.ID)
	if reloaded == nil || reloaded.authenticate(auth) != nil {
		t.Fatal("Service restart lost the admitted guest association")
	}
	reloaded.mu.Lock()
	again := reloaded.record
	reloaded.mu.Unlock()
	againKey, _ := again.Identity.Fingerprint()
	if againKey != key || again.RequestID != stored.RequestID || again.Room.Generation != 7 || again.BindID != stored.BindID {
		t.Fatal("Service restart rotated room key, request, generation or local binding")
	}
	room, err := service.registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, HostMode: model.HostNative, Agents: map[model.ActorID]model.AgentSelection{model.ActorSlot1: {Runtime: model.RuntimeGrok}, model.ActorSlot2: {Runtime: model.RuntimeCodex}}}, service.provisioner)
	if err != nil {
		t.Fatal(err)
	}
	native, err := service.nativeRuntime(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := native.engine.BindReport(model.ActorSlot1, relay.BindRequest{BindID: "hosted-collision", CredentialHash: relay.Digest("other-secret"), SessionID: auth.SessionID}); !errors.Is(err, ErrBindingOwned) {
		t.Fatalf("hosted room accepted a session already joined to LAN: %v", err)
	}
	if _, err := service.lanGuests.prepare(invitation, lanLocalJoinRequest{Workspace: project.Root, Runtime: model.RuntimeGrok, SessionID: "different-session", BindID: "new-binding", CredentialHash: relay.Digest("new-secret")}); err == nil {
		t.Fatal("same room's admitted guest key was silently retargeted")
	}
	wrong := auth
	wrong.Generation--
	if reloaded.authenticate(wrong) == nil {
		t.Fatal("retired local generation authenticated after restart")
	}
	for _, summary := range service.lanGuests.summaries() {
		data, _ := json.Marshal(summary)
		if bytes.Contains(data, []byte(auth.SessionID)) || bytes.Contains(data, []byte("PRIVATE KEY")) || bytes.Contains(data, []byte(auth.Secret)) {
			t.Fatal("Management joined Room summary disclosed private transport identity")
		}
	}
}

func TestLANGuestRetiredAssociationDoesNotBlockReusedSessionAfterRestart(t *testing.T) {
	for _, status := range []string{"left", "expired"} {
		t.Run(status, func(t *testing.T) {
			service, project := lanGuestTestService(t)
			invitation, _ := lanGuestTestRemote(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
			guest, auth := admitLANGuestFixture(t, service, project, invitation)
			guest.mu.Lock()
			next := guest.record
			next.Status = status
			if status == "expired" {
				next.Room = nil // a pending request expired before membership existed
			}
			if err := privatefile.WriteJSON(filepath.Join(guest.dir, "guest.json"), next); err != nil {
				guest.mu.Unlock()
				t.Fatal(err)
			}
			guest.record = next
			guest.mu.Unlock()
			room, err := service.registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, HostMode: model.HostNative, Agents: map[model.ActorID]model.AgentSelection{model.ActorSlot1: {Runtime: model.RuntimeGrok}, model.ActorSlot2: {Runtime: model.RuntimeCodex}}}, service.provisioner)
			if err != nil {
				t.Fatal(err)
			}
			native, err := service.nativeRuntime(context.Background(), room.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := native.engine.BindReport(model.ActorSlot1, relay.BindRequest{BindID: "reused-session", CredentialHash: relay.Digest("new-secret"), SessionID: auth.SessionID}); err != nil {
				t.Fatalf("retired association kept live session ownership: %v", err)
			}
			service.lanGuests.close()
			if err := initLANGuests(service); err != nil {
				t.Fatalf("restart resurrected retired session ownership: %v", err)
			}
			service.lanGuests.cancel()
			if restored := service.lanGuests.get(guest.record.ID); restored == nil || restored.record.Status != status {
				t.Fatal("restart lost the retired association audit")
			}
		})
	}
}
