package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func decodeJoin(t *testing.T, body []byte) map[model.ActorID]string {
	t.Helper()
	var value struct {
		Join map[model.ActorID]string `json:"join"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	return value.Join
}

// The Room page hands this text to a human to paste into an Agent session, so it
// must name the Service that actually hosts the Room.
func TestNativeSnapshotProjectsThisServicesJoinCommand(t *testing.T) {
	f := nativeHTTP(t)
	res := inspectionRequest(t, f, "/api/v1/snapshot?tail=1")
	if res.Code != 200 {
		t.Fatalf("snapshot: %d", res.Code)
	}
	join := decodeJoin(t, res.Body.Bytes())
	for _, slot := range model.SlotActors() {
		want := relay.LocalBindCommand(f.endpoint, f.room.ID, slot)
		if join[slot] != want {
			t.Fatalf("slot %s join command = %q, want %q", slot, join[slot], want)
		}
		if !strings.Contains(join[slot], "--service-file "+relay.QuoteShellPath(f.endpoint)) {
			t.Fatalf("a custom data root lost its Service selector: %q", join[slot])
		}
	}
}

// The conditional refresh is what an open Room page polls; it must carry the
// same guidance rather than dropping it between sequences.
func TestNativeJoinGuidanceSurvivesTheConditionalRefresh(t *testing.T) {
	f := nativeHTTP(t)
	full := decodeJoin(t, inspectionRequest(t, f, "/api/v1/snapshot").Body.Bytes())
	conditional := decodeJoin(t, inspectionRequest(t, f, "/api/v1/snapshot?tail=1&known_sequence="+strconv.FormatUint(f.native.engine.Sequence(), 10)).Body.Bytes())
	if len(full) == 0 || len(conditional) != len(full) {
		t.Fatalf("conditional refresh dropped the join guide: %v vs %v", conditional, full)
	}
	for slot, command := range full {
		if conditional[slot] != command {
			t.Fatalf("slot %s command changed between reads: %q vs %q", slot, conditional[slot], command)
		}
	}
}

func TestNativeJoinDropsTheSelectorForADefaultServiceRoot(t *testing.T) {
	f := nativeHTTP(t)
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	t.Setenv("APPDATA", base)
	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base)
	dir, err := relay.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	f.native.endpointPath = filepath.Join(dir, relay.EndpointFile)
	join := decodeJoin(t, inspectionRequest(t, f, "/api/v1/snapshot").Body.Bytes())
	for _, slot := range model.SlotActors() {
		if strings.Contains(join[slot], "--service-file") {
			t.Fatalf("the CLI already resolves this endpoint: %q", join[slot])
		}
	}
}

// An endpoint this runtime cannot name would render a command aimed at whatever
// Service happens to answer, so no command may be projected at all.
func TestNativeJoinProjectsNothingWithoutAResolvableEndpoint(t *testing.T) {
	f := nativeHTTP(t)
	f.native.endpointPath = ""
	var value map[string]json.RawMessage
	if err := json.Unmarshal(inspectionRequest(t, f, "/api/v1/snapshot").Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if _, ok := value["join"]; ok {
		t.Fatal("an unresolvable endpoint still projected join guidance")
	}
}

// A LAN guest joins through its invitation; a local bind command would name the
// wrong machine's Service.
func TestNativeJoinGuideNeverGuidesALANGuestSlot(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	ctx := context.Background()
	registry, project := testRegistry(t, testGitRepo(t))
	noSpawn := ProvisionerFunc(func(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error) {
		t.Error("native LAN provisioning attempted to start a vendor runtime")
		return Binding{}, nil, errors.New("unexpected native process")
	})
	room, err := registry.ProvisionRoom(ctx, ProvisionRequest{ProjectID: project.ID, Name: "Shared work", HostMode: model.HostNative, Sharing: "lan", OwnerSlot: model.ActorSlot1}, noSpawn)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := startNativeHostRuntime(ctx, registry, project, room, "127.0.0.1", nativeWakerConfig{QueuePending: emptyNativeWakeQueue, Mock: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	native, ok := runtime.(*nativeHostRuntime)
	if !ok {
		t.Fatal("native Room became another runtime")
	}
	t.Cleanup(func() {
		if err := native.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	join := decodeJoin(t, marshalSnapshot(t, native.snapshot()))
	if _, ok := join[model.ActorSlot2]; ok {
		t.Fatal("a LAN guest slot was offered a local bind command")
	}
	want := relay.LocalBindCommand(filepath.Join(registry.Root(), relay.EndpointFile), room.ID, model.ActorSlot1)
	if join[model.ActorSlot1] != want {
		t.Fatalf("LAN owner command = %q, want %q", join[model.ActorSlot1], want)
	}
}

func marshalSnapshot(t *testing.T, value map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
