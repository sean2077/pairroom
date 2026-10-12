package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func TestLANProvisioningKeepsPeerUnselectedUntilRealAdmission(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	kinds := []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok, model.RuntimeGemini}
	for _, owner := range model.SlotActors() {
		for _, localKind := range kinds {
			for _, remoteKind := range kinds {
				t.Run(string(owner)+"/"+string(localKind)+"/"+string(remoteKind), func(t *testing.T) {
					relayclient.IsolateNativeCaller(t)
					ctx := context.Background()
					registry, project := testRegistry(t, testGitRepo(t))
					peer := model.OtherParticipant(owner)
					noSpawn := ProvisionerFunc(func(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error) {
						t.Error("native LAN provisioning attempted to start a vendor runtime")
						return Binding{}, nil, errors.New("unexpected native process")
					})
					room, err := registry.ProvisionRoom(ctx, ProvisionRequest{ProjectID: project.ID, Name: "Shared work", HostMode: model.HostNative, Sharing: "lan", OwnerSlot: owner}, noSpawn)
					if err != nil {
						t.Fatal(err)
					}
					assertPending := func(room Room) {
						t.Helper()
						if !room.Agents[peer].AwaitingPeer || room.Agents[peer].Runtime != "" || room.RuntimeNames[peer] != "" {
							t.Fatalf("unadmitted peer acquired a runtime or native name: %+v", room)
						}
						if !room.Agents[owner].AwaitingBinding || room.Agents[owner].Runtime != "" || room.RuntimeNames[owner] != "" {
							t.Fatalf("owner runtime was preselected before binding: %+v", room.RuntimeNames)
						}
					}
					assertPending(room)
					events, err := readEventsReadOnly(filepath.Join(room.DataDir, "events.jsonl"))
					if err != nil {
						t.Fatal(err)
					}
					seen := map[model.ActorID]bool{}
					for _, event := range events {
						if event.Kind != "participant.updated" {
							continue
						}
						var participant model.ParticipantSnapshot
						if err := json.Unmarshal(event.Data, &participant); err != nil {
							t.Fatal(err)
						}
						seen[participant.ID] = true
						identity := model.NativeParticipantIdentityFor(participant.ID)
						if participant.RuntimeKind != "" || participant.MentionHandle != identity.MentionHandle || participant.DisplayName != identity.DisplayName {
							t.Fatalf("initial participant preselected a Runtime or lost its stable seat: %+v", participant)
						}
					}
					if len(seen) != 2 {
						t.Fatal("initial participant facts are incomplete")
					}
					// The checkpoint accepts the explicitly empty peer name; cold
					// reconstruction preserves the unselected durable runtime.
					registry, err = OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
					if err != nil {
						t.Fatal(err)
					}
					room, _ = registry.Room(room.ID)
					assertPending(room)
					runtime, err := startNativeHostRuntime(ctx, registry, project, room, "127.0.0.1", nativeWakerConfig{Mock: true}, nil)
					if err != nil {
						t.Fatal(err)
					}
					native := runtime.(*nativeHostRuntime)
					t.Cleanup(func() {
						if err := native.Close(context.Background()); err != nil {
							t.Error(err)
						}
					})
					initial := native.snapshot()
					identities := initial["identities"].(map[model.ActorID]model.ParticipantIdentity)
					if identities[peer].MentionHandle != model.NativeParticipantIdentityFor(peer).MentionHandle || identities[owner].MentionHandle != model.NativeParticipantIdentityFor(owner).MentionHandle {
						t.Fatalf("live initial identities disagree with the durable selection: %+v", identities)
					}
					if _, err := native.engine.Bind(owner, relay.BindRequest{Runtime: localKind, BindID: "actual-owner", SessionID: "owner-session", CredentialHash: relay.Digest("owner-secret")}); err != nil {
						t.Fatal(err)
					}
					invite, err := native.engine.CreateLANInvite()
					if err != nil {
						t.Fatal(err)
					}
					key := relay.Digest("actual-peer")
					if _, status, err := native.engine.RequestLANJoin(relay.LANJoinRequest{InviteID: invite.ID, RequestID: "actual-request", Key: key, Runtime: remoteKind}); err != nil || status != "pending" {
						t.Fatalf("request: %s, %v", status, err)
					}
					if got := native.engine.Runtimes()[peer]; got != model.RuntimeAwaitingPeer {
						t.Fatal("request-only invitation selected a runtime before owner approval")
					}
					if _, err := native.engine.AcceptLANJoin("actual-request", key); err != nil {
						t.Fatal(err)
					}
					wantHandle := func(slot model.ActorID) string {
						return model.NativeParticipantIdentityFor(slot).MentionHandle
					}
					assertAdmitted := func(room Room) {
						t.Helper()
						if room.Agents[peer].AwaitingPeer || room.Agents[peer].Runtime != remoteKind || room.Agents[owner].Runtime != localKind {
							t.Fatalf("actual admission did not determine the peer runtime: %+v", room.Agents)
						}
						for _, slot := range model.SlotActors() {
							if !strings.Contains(room.RuntimeNames[slot], " · "+wantHandle(slot)+" · ") {
								t.Fatalf("actual runtime names were not finalized: %+v", room.RuntimeNames)
							}
						}
					}
					room, _ = registry.Room(room.ID)
					assertAdmitted(room)
					live := native.snapshot()
					assertAdmitted(live["room"].(Room))
					identities = live["identities"].(map[model.ActorID]model.ParticipantIdentity)
					for _, slot := range model.SlotActors() {
						if identities[slot].MentionHandle != wantHandle(slot) {
							t.Fatalf("live identities stayed at pre-admission values: %+v", identities)
						}
					}
					if err := native.Close(ctx); err != nil {
						t.Fatal(err)
					}
					registry, err = OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
					if err != nil {
						t.Fatal(err)
					}
					room, _ = registry.Room(room.ID)
					assertAdmitted(room)
				})
			}
		}
	}
}
