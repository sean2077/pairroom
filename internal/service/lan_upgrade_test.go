package service

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func TestLANUpgradePreservesRealPreLANLocalRooms(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	root, workspace := t.TempDir(), testGitRepo(t)
	fixture := filepath.Join("testdata", "pre-lan-local")
	readFixture := func(name string) []byte {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(fixture, name))
		if err != nil {
			t.Fatal(err)
		}
		for placeholder, value := range map[string]string{"__DATA_ROOT__": root, "__WORKSPACE__": workspace, "__PROJECT_ID__": projectID(workspace)} {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.ReplaceAll(data, []byte(placeholder), encoded[1:len(encoded)-1])
		}
		// Captured data-directory paths include a separator after the placeholder.
		// JSON accepts slash on both platforms; the derived index uses native paths.
		if os.PathSeparator != '/' {
			var snapshot RegistrySnapshot
			if name == "service-registry.json" {
				if err := json.Unmarshal(data, &snapshot); err != nil {
					t.Fatal(err)
				}
				for i := range snapshot.Rooms {
					snapshot.Rooms[i].DataDir = filepath.Join(root, "rooms", snapshot.Rooms[i].ID)
				}
				data, err = json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		return data
	}
	checkpoint := readFixture("service-registry.json")
	var before RegistrySnapshot
	if err := json.Unmarshal(checkpoint, &before); err != nil {
		t.Fatal(err)
	}
	if before.Schema != 3 || len(before.Rooms) != 2 {
		t.Fatal("pre-LAN fixture is not the captured current local format")
	}
	if err := os.WriteFile(filepath.Join(root, "service-registry.json"), checkpoint, 0o600); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, room := range before.Rooms {
		dir := filepath.Join(root, "rooms", room.ID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"metadata.json", "events.jsonl"} {
			data := readFixture(string(room.HostMode) + "-" + name)
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			files[path] = data
		}
	}
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: root})
	if err != nil {
		t.Fatalf("current pre-LAN root was rejected: %v", err)
	}
	for _, expected := range before.Rooms {
		actual, ok := registry.Room(expected.ID)
		if !ok || actual.HostMode != expected.HostMode || actual.Name != expected.Name || actual.Sharing != "" || actual.OwnerSlot != "" {
			t.Fatalf("existing Room changed: %+v", actual)
		}
		for _, slot := range model.SlotActors() {
			if actual.Agents[slot] != expected.Agents[slot] || actual.Bindings[slot] != expected.Bindings[slot] {
				t.Fatalf("existing Runtime or binding changed for %s/%s", actual.ID, slot)
			}
		}
	}
	for path, want := range files {
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("upgrade rewrote Room facts %s: %v", filepath.Base(path), err)
		}
	}
	if schema, _, err := readSchemaHeader(filepath.Join(root, "service-registry.json")); err != nil || schema != 4 {
		t.Fatalf("derived checkpoint did not mark the new reader: schema=%d error=%v", schema, err)
	}
}

func TestFutureLANIdentitiesFailBeforeRegistryCleanup(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	for _, kind := range []string{"host", "guest", "native"} {
		t.Run(kind, func(t *testing.T) {
			relayclient.IsolateNativeCaller(t)
			root := t.TempDir()
			dir := filepath.Join(root, "lan")
			if err := privatefile.Mkdir(dir); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "host.json")
			if kind == "guest" {
				clients, err := lanclient.Open()
				if err != nil {
					t.Fatal(err)
				}
				defer clients.Close()
				if err := os.MkdirAll(filepath.Dir(clients.Root()), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := privatefile.Mkdir(clients.Root()); err != nil {
					t.Fatal(err)
				}
				dir = filepath.Join(clients.Root(), "lan_"+strings.Repeat("a", 32))
				if err := privatefile.Mkdir(dir); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(dir, "client.json")
			}
			if kind == "native" {
				identities, err := nativeidentity.Open()
				if err != nil {
					t.Fatal(err)
				}
				claim := nativeidentity.Claim{Runtime: model.RuntimeCodex, SessionID: "future-native", Association: nativeidentity.Remote("peer", "room"), BindID: "pending"}
				if err := identities.Reserve(context.Background(), claim); err != nil {
					t.Fatal(err)
				}
				base, err := os.UserConfigDir()
				if err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(base, "pairroom", "native-identities", relay.Digest(string(claim.Runtime)+"\x00"+claim.SessionID), "claim.json")
			}
			if err := privatefile.WriteJSON(path, map[string]int{"schema": 2}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(root, ".service-registry-future.tmp")
			if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: root}); err == nil || !strings.Contains(err.Error(), "data was not modified") {
				t.Fatalf("future LAN identity was not rejected before recovery: %v", err)
			}
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, before) {
				t.Fatalf("future identity was replaced: %v", err)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("root cleanup ran before LAN format check: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "service-registry.json")); !os.IsNotExist(err) {
				t.Fatalf("unsupported LAN root gained checkpoint: %v", err)
			}
		})
	}
}

func TestLANPreflightPreservesIPv6ZoneIdentity(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	root := t.TempDir()
	dir := filepath.Join(root, "lan")
	if err := privatefile.Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	identity, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "host.json")
	if err := privatefile.WriteJSON(path, lanHostConfig{Schema: 1, Enabled: true, Address: "[fe80::1%en0]:8877", Identity: identity}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := preflightLANState(root); err != nil {
		t.Fatalf("configured IPv6 zone identity rejected during read-only restart preflight: %v", err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatal("IPv6 identity changed during preflight")
	}
}

func TestLANFormatMismatchFailsBeforeRootCleanup(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	for _, payload := range []string{
		`{"schema":6}`, // new provisioning may not be smuggled into Store 12
		`{"schema":5,"sharing":"lan","owner_slot":"slot1"}`,
		`{"schema":5,"agents":{"slot2":{"awaiting_peer":true}}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "rooms", ".provision-incompatible")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			metadata := []byte(`{"format":"pairroom-jsonl","schema_version":12}`)
			events := []byte("{\"kind\":\"room.created\"}\n{\"kind\":\"service.room.provisioned\",\"data\":" + payload + "}\n")
			for name, data := range map[string][]byte{"metadata.json": metadata, "events.jsonl": events} {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := OpenRegistry(context.Background(), RegistryConfig{Root: root})
			if err == nil || !strings.Contains(err.Error(), "data was not modified") {
				t.Fatalf("invalid permission/schema boundary was not rejected: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(dir, "events.jsonl")); err != nil || !bytes.Equal(got, events) {
				t.Fatalf("incompatible staged Room was changed: %v", err)
			}
		})
	}
}
