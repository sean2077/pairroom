package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

// Changing the fixture's user directory models separate machines. Each Service
// captures its own private stores at construction; CLI invocations use the
// current user's store. No guest Service is constructed by directLANJoin.
func directLANUser(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(key, root)
	}
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return config
}

type directLANFixture struct {
	root, session, room string
	host                *lanHostFixture
}

func (f *directLANFixture) run(t *testing.T, withRepo bool, input any, args ...string) ([]byte, error) {
	t.Helper()
	clearNativeSessionEnv(t)
	t.Setenv("CODEX_SESSION_ID", f.session)
	var data []byte
	if input != nil {
		data, _ = json.Marshal(input)
	}
	if withRepo {
		args = append(args, "--repo", f.root)
	}
	var out, diagnostic bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := relayclient.Run(ctx, args, bytes.NewReader(data), &out, &diagnostic)
	if err != nil {
		return out.Bytes(), err
	}
	return out.Bytes(), nil
}

func directLANJoin(t *testing.T, host *lanHostFixture, root, session string) *directLANFixture {
	t.Helper()
	f := &directLANFixture{root: root, session: session, room: lanclient.ID(host.invite), host: host}
	if output, err := f.run(t, true, nil, "install", "--runtime", "codex"); err != nil {
		t.Fatalf("install direct hooks: %s, %v", output, err)
	}
	invite := lanshare.EncodeInvite(host.invite)
	output, err := f.run(t, true, nil, "join", invite)
	var pending struct{ Status, Receipt, Room string }
	if err != nil || json.Unmarshal(output, &pending) != nil || pending.Status != "pending" || pending.Room != f.room {
		t.Fatalf("join without guest Service: %s, %v", output, err)
	}
	var accepted lanshare.JoinResponse
	if status := host.localCall(t, "/api/v1/rooms/"+host.room.ID+"/lan/accept", map[string]string{"receipt": pending.Receipt}, &accepted, host.management.Token()); status != http.StatusOK {
		t.Fatalf("approve direct client: %d", status)
	}
	output, err = f.run(t, true, nil, "join", invite)
	var joined struct {
		Status  string
		Room    string
		Binding relay.Binding
	}
	if err != nil || json.Unmarshal(output, &joined) != nil || joined.Status != "accepted" || joined.Room != f.room || joined.Binding.SessionID != session {
		t.Fatalf("confirm direct client: %s, %v", output, err)
	}
	return f
}

func TestLANDirectCLIWithOnlyTheHostService(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	host := newLANHostFixture(t)
	guestConfig := directLANUser(t)
	f := directLANJoin(t, host, testGitRepo(t), "one-service-guest")
	endpoint := filepath.Join(guestConfig, "pairroom", relay.EndpointFile)
	if _, err := os.Stat(endpoint); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("joining created or required a guest Service endpoint")
	}
	body := "#!/bin/sh\nprintf 'explicit reproduction evidence\\n'\n"
	file := filepath.Join(f.root, "repro.sh")
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"send", "--id", "direct-evidence", "--text", "Please reproduce this failure.", "--file", file}
	output, err := f.run(t, true, nil, args...)
	var sent struct{ Published string }
	if err != nil || json.Unmarshal(output, &sent) != nil || sent.Published == "" {
		t.Fatalf("direct evidence send: %s, %v", output, err)
	}
	if again, err := f.run(t, true, nil, args...); err != nil || !bytes.Equal(output, again) {
		t.Fatalf("same-ID direct publication was not stable: %s, %v", again, err)
	}
	page, err := host.native.engine.History(relay.HistoryQuery{ID: sent.Published})
	if err != nil || len(page.Messages) != 1 || len(page.Messages[0].Attachments) != 1 {
		t.Fatal("host did not retain one canonical evidence publication")
	}
	metadata, hostPath, err := host.native.media.Resolve(page.Messages[0].Attachments[0].ID)
	if err != nil || metadata.SHA256 != relay.Digest(body) {
		t.Fatal("host evidence receipt failed integrity validation")
	}
	claim, err := host.native.engine.Claim(context.Background(), host.owner, false)
	if err != nil || claim == nil || claim.ID != sent.Published || !lanEnvelopeHasLocalPath(claim.Envelope, hostPath) || lanEnvelopeHasPathPrefix(claim.Envelope, f.root) {
		t.Fatalf("host did not receive its own verified copy: %+v, %v", claim, err)
	}
	if err := host.native.engine.Ack(host.owner, claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	reply, err := host.native.engine.Send(host.owner, relay.SendRequest{ID: "direct-reply", Text: "I reproduced it; please review this artifact.", AttachmentIDs: []string{metadata.ID}})
	if err != nil {
		t.Fatal(err)
	}
	output, err = f.run(t, true, nil, "wait", "--timeout", "1")
	if err != nil || !strings.Contains(string(output), reply.Text) || lanEnvelopeHasPathPrefix(string(output), host.room.DataDir) {
		t.Fatalf("direct guest collection failed: %s, %v", output, err)
	}
	store, err := lanclient.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	client, err := store.Get(context.Background(), f.room)
	if err != nil {
		t.Fatal(err)
	}
	copied, guestPath, err := client.Download(context.Background(), metadata.ID)
	if err != nil || copied != metadata || !lanEnvelopeHasLocalPath(string(output), guestPath) {
		t.Fatal("guest envelope did not reference its own verified file")
	}
	if content, err := os.ReadFile(guestPath); err != nil || string(content) != body {
		t.Fatal("guest evidence bytes changed")
	}
	if page, err := host.native.engine.History(relay.HistoryQuery{ID: reply.ID}); err != nil || len(page.Messages) != 1 || page.Messages[0].State != "handed_off" {
		t.Fatal("direct stdout did not acknowledge its original host receipt")
	}
	if _, err := f.run(t, true, nil, "park", "--enabled=false"); err != nil {
		t.Fatal(err)
	}
	text := "@claude The direct Stop hook published this result."
	input := map[string]any{"hook_event_name": "Stop", "session_id": f.session, "cwd": f.root, "last_assistant_message": text, "transcript_path": filepath.Join(f.root, "private-transcript.jsonl")}
	if output, err := f.run(t, true, input, "hook", "--runtime", "codex"); err != nil || strings.TrimSpace(string(output)) != "{}" {
		t.Fatalf("direct Native hook failed: %s, %v", output, err)
	}
	page, err = host.native.engine.History(relay.HistoryQuery{})
	found := false
	for _, message := range page.Messages {
		found = found || strings.Contains(message.Text, "direct Stop hook published")
	}
	if err != nil || !found {
		t.Fatal("direct hook publication never reached the canonical Room")
	}
	// A fresh invocation resolves the saved direct route after cwd changes.
	t.Chdir(testGitRepo(t))
	for _, command := range []string{"bind", "status", "history", "doctor"} {
		if output, err := f.run(t, false, nil, command); err != nil || len(output) == 0 {
			t.Fatalf("direct %s required cwd or a guest Service: %s, %v", command, output, err)
		}
	}
	if _, err := os.Stat(endpoint); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("direct operations created a local Service endpoint")
	}
	if _, err := f.run(t, false, nil, "unbind"); err != nil {
		t.Fatalf("direct leave: %v", err)
	}
}

func TestLANLocalHostingAndMultipleDirectRoomsCoexist(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	directLANUser(t)
	hostA := newLANHostFixture(t)
	directLANUser(t)
	hostB := newLANHostFixture(t)
	localConfig := directLANUser(t)
	local := newLANHostFixture(t)
	project, ok := local.management.registry.Project(local.room.ProjectID)
	if !ok {
		t.Fatal("local project unavailable")
	}
	endpoint := filepath.Join(localConfig, "pairroom", relay.EndpointFile)
	if err := os.MkdirAll(filepath.Dir(endpoint), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := relay.WriteEndpoint(filepath.Dir(endpoint), relay.Endpoint{URL: local.local.URL, Token: local.management.cliToken}); err != nil {
		t.Fatal(err)
	}
	owner := &directLANFixture{root: project.Root, session: "local-room-owner"}
	if _, err := owner.run(t, true, nil, "install", "--runtime", "codex"); err != nil {
		t.Fatal(err)
	}
	created, err := owner.run(t, true, nil, "bind", "--create", "--share", "lan", "--name", "Locally hosted alongside remote rooms")
	var hosted struct{ Invite string }
	if err != nil || json.Unmarshal(created, &hosted) != nil || hosted.Invite == "" {
		t.Fatalf("local Room creation: %s, %v", created, err)
	}
	hostedInvite, err := lanshare.ParseInvite(hosted.Invite)
	if err != nil {
		t.Fatal(err)
	}
	a := directLANJoin(t, hostA, project.Root, "remote-room-a")
	// Reject reuse before creating an unusable second join attempt. A fresh
	// session must still be able to accept that same invitation afterwards.
	if _, err := a.run(t, true, nil, "join", lanshare.EncodeInvite(hostB.invite)); err == nil {
		t.Fatal("one native session acquired a second Room")
	}
	b := directLANJoin(t, hostB, project.Root, "remote-room-b")
	if a.room == b.room || a.room == hostedInvite.RoomID || b.room == hostedInvite.RoomID {
		t.Fatal("local and remote Rooms collided")
	}
	if output, err := owner.run(t, true, nil, "status"); err != nil || !bytes.Contains(output, []byte(hostedInvite.RoomID)) {
		t.Fatalf("local owner was redirected after remote joins: %s, %v", output, err)
	}
	for _, f := range []*directLANFixture{a, b} {
		if output, err := f.run(t, true, nil, "send", "--id", "same-id-in-distinct-rooms", "--text", f.session); err != nil || len(output) == 0 {
			t.Fatalf("coexisting direct send: %s, %v", output, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := local.management.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	local.local.Close()
	local.remote.Close()
	// A stopped local Service and unusable default file must be irrelevant.
	if err := os.WriteFile(endpoint, []byte("unrelated local endpoint changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Locators are disposable. Cold recovery must consult the private direct
	// catalog before this unrelated broken local endpoint, even in another cwd.
	if err := os.RemoveAll(filepath.Join(localConfig, "pairroom", "relay-sessions")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(testGitRepo(t))
	for _, f := range []*directLANFixture{a, b} {
		preflight, _ := f.run(t, false, nil, "preflight")
		var check struct{ Mode string }
		if json.Unmarshal(preflight, &check) != nil || check.Mode != "lan_direct" {
			t.Fatalf("cold preflight followed the local Service: %s", preflight)
		}
		if _, err := f.host.native.engine.Send(f.host.owner, relay.SendRequest{ID: "reply-after-local-stop", Text: "reply only for " + f.session}); err != nil {
			t.Fatal(err)
		}
		output, err := f.run(t, false, nil, "wait", "--timeout", "1")
		if err != nil || !strings.Contains(string(output), "reply only for "+f.session) {
			t.Fatalf("local Service stop broke direct collection: %s, %v", output, err)
		}
		if output, err := f.run(t, false, nil, "send", "--id", "after-local-stop", "--text", f.session+" still connected directly"); err != nil || len(output) == 0 {
			t.Fatalf("local Service stop broke direct publication: %s, %v", output, err)
		}
		page, err := f.host.native.engine.History(relay.HistoryQuery{})
		if err != nil {
			t.Fatal(err)
		}
		other := a.session
		if f == a {
			other = b.session
		}
		for _, message := range page.Messages {
			if strings.Contains(message.Text, other) {
				t.Fatal("messages crossed per-session host routing")
			}
		}
	}
}

func TestLANDirectPendingAdmissionCanBeDetachedOffline(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	hostA := newLANHostFixture(t)
	directLANUser(t)
	hostB := newLANHostFixture(t)
	directLANUser(t)
	f := &directLANFixture{root: testGitRepo(t), session: "abandoned-native-session", room: lanclient.ID(hostA.invite), host: hostA}
	if _, err := f.run(t, true, nil, "install", "--runtime", "codex"); err != nil {
		t.Fatal(err)
	}
	output, err := f.run(t, true, nil, "join", lanshare.EncodeInvite(hostA.invite))
	var pending struct{ Status string }
	if err != nil || json.Unmarshal(output, &pending) != nil || pending.Status != "pending" {
		t.Fatalf("pending direct admission: %s, %v", output, err)
	}
	hostA.remote.Close()
	other := *f
	other.session = "different-native-session"
	if _, err := other.run(t, true, nil, "unbind", "--local-only", "--room", f.room); err == nil {
		t.Fatal("a different native session detached the pending admission")
	}
	t.Chdir(testGitRepo(t))
	if output, err := f.run(t, false, nil, "unbind", "--local-only"); err != nil || !bytes.Contains(output, []byte("local-only")) {
		t.Fatalf("pending offline detach without slot state: %s, %v", output, err)
	}
	store, err := lanclient.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	client, err := store.Get(context.Background(), f.room)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := client.Metadata(context.Background())
	if err != nil || meta.Status != "detached" {
		t.Fatalf("pending association was not retained as detached: %+v, %v", meta, err)
	}
	if _, err := client.Resume(context.Background()); !errors.Is(err, lanclient.ErrInactive) {
		t.Fatalf("detached admission could reconnect: %v", err)
	}
	// The released local native identity can now be admitted to another host.
	directLANJoin(t, hostB, f.root, f.session)
}

func TestLANDirectExplicitReadmissionPreservesRetiredWAL(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	host := newLANHostFixture(t)
	directLANUser(t)
	f := directLANJoin(t, host, testGitRepo(t), "original-native-session")
	statePath := filepath.Join(f.root, ".pairroom", "rooms", f.room, "slots", "slot2", "state.json")
	var original relayclient.State
	if err := privatefile.ReadJSON(statePath, 2<<20, &original); err != nil {
		t.Fatal(err)
	}
	original.LastSeq, original.LastConfirmedSeq = 1, 0
	original.Pending = &relayclient.Pending{Seq: 1, Text: "retired-original-unknown", Unknown: true}
	if err := privatefile.WriteJSON(statePath, original); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	newSession := *f
	newSession.session = "readmitted-native-session"
	unissued := host.invite
	unissued.InviteID = "not-an-owner-issued-replacement"
	if _, err := newSession.run(t, true, nil, "join", lanshare.EncodeInvite(unissued), "--replace"); err == nil {
		t.Fatal("an active host membership was replaced")
	}
	if after, err := os.ReadFile(statePath); err != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected replacement changed the original workspace WAL")
	}
	if status := host.localCall(t, "/api/v1/rooms/"+host.room.ID+"/lan/revoke", map[string]any{}, nil, host.management.Token()); status != http.StatusOK {
		t.Fatalf("host revoke: %d", status)
	}
	var fresh struct{ Invite string }
	if status := host.localCall(t, "/api/v1/rooms/"+host.room.ID+"/lan/invite", map[string]any{}, &fresh, host.management.Token()); status != http.StatusOK {
		t.Fatalf("fresh owner invitation: %d", status)
	}
	output, err := newSession.run(t, true, nil, "join", fresh.Invite, "--replace")
	var pending struct{ Status, Receipt string }
	if err != nil || json.Unmarshal(output, &pending) != nil || pending.Status != "pending" {
		t.Fatalf("explicit fresh re-admission: %s, %v", output, err)
	}
	archive := filepath.Join(f.root, ".pairroom", "retired", original.BindID, "state.json")
	var archived relayclient.State
	if err := privatefile.ReadJSON(archive, 2<<20, &archived); err != nil || archived.BindID != original.BindID || archived.Generation != original.Generation || archived.Pending == nil || !archived.Pending.Unknown || archived.Pending.Text != original.Pending.Text {
		t.Fatalf("retired publication WAL was not preserved: %+v, %v", archived, err)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("new pending admission reused an old workspace slot")
	}
	var admitted lanshare.JoinResponse
	if status := host.localCall(t, "/api/v1/rooms/"+host.room.ID+"/lan/accept", map[string]string{"receipt": pending.Receipt}, &admitted, host.management.Token()); status != http.StatusOK {
		t.Fatalf("approve fresh exact receipt: %d", status)
	}
	if output, err := newSession.run(t, true, nil, "join", fresh.Invite, "--replace"); err != nil {
		t.Fatalf("finish fresh admission: %s, %v", output, err)
	}
	var current relayclient.State
	if err := privatefile.ReadJSON(statePath, 2<<20, &current); err != nil || current.BindID == original.BindID || current.Generation <= original.Generation || current.SessionID != newSession.session || current.LastSeq != 0 || current.Pending != nil {
		t.Fatalf("fresh admission reused the retired publication identity: %+v, %v", current, err)
	}
	if _, err := f.run(t, true, nil, "status", "--room", f.room); err == nil {
		t.Fatal("retired native session followed the new membership")
	}
	if _, err := newSession.run(t, true, nil, "send", "--id", "new-admission", "--text", "new session publication"); err != nil {
		t.Fatal(err)
	}
	page, err := host.native.engine.History(relay.HistoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range page.Messages {
		if strings.Contains(message.Text, original.Pending.Text) {
			t.Fatal("retired uncertain publication was replayed into a new admission")
		}
	}
}

func TestLANDirectShortStdoutPreservesUnknownUntilExplicitRetry(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	host := newLANHostFixture(t)
	directLANUser(t)
	f := directLANJoin(t, host, testGitRepo(t), "direct-short-stdout")
	message, err := host.native.engine.Send(host.owner, relay.SendRequest{ID: "original-short-write", Text: "This input has not been written to the native caller."})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := relayclient.Run(ctx, []string{"wait", "--timeout", "1", "--repo", f.root}, strings.NewReader(""), lanFailedStdout{}, io.Discard); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("failed stdout was not a failed delivery: %v", err)
	}
	store, err := lanclient.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	client, err := store.Get(ctx, f.room)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Maintenance(ctx); err != nil {
		t.Fatal(err)
	}
	page, err := host.native.engine.History(relay.HistoryQuery{ID: message.ID})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].State != "delivering" {
		t.Fatal("network receipt or maintenance falsely acknowledged failed stdout")
	}
	if err := host.native.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := host.management.runtimes.Suspend(ctx, host.room.ID); err != nil {
		t.Fatal(err)
	}
	host.native, err = host.management.sharedNativeRuntime(ctx, host.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	page, err = host.native.engine.History(relay.HistoryQuery{ID: message.ID})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].State != "unknown" {
		t.Fatal("host restart silently replayed the unacknowledged original")
	}
	retry, err := host.native.engine.Retry(message.ID)
	if err != nil || retry.ID == message.ID || retry.RetryOf != message.ID {
		t.Fatal("explicit host retry did not receive a new identity")
	}
	output, err := f.run(t, true, nil, "wait", "--timeout", "1")
	if err != nil || strings.Count(string(output), message.Text) != 1 {
		t.Fatalf("explicit retry was not received exactly once: %s, %v", output, err)
	}
	page, err = host.native.engine.History(relay.HistoryQuery{ID: message.ID})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].State != "unknown" {
		t.Fatal("retry acknowledgement changed the original unknown receipt")
	}
}
