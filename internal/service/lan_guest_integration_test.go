package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func lanLocalRelayCall(t *testing.T, fixture *lanHostFixture, room string, auth relay.Auth, action string, payload, result any) int {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, fixture.local.URL+"/api/v1/relay/"+room+"/"+string(auth.Slot)+"/"+action, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Relay "+auth.Secret)
	request.Header.Set("X-PairRoom-Bind", auth.BindID)
	request.Header.Set("X-PairRoom-Generation", strconv.FormatUint(auth.Generation, 10))
	request.Header.Set("X-PairRoom-Session", auth.SessionID)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode == http.StatusOK && result != nil {
		if err := json.Unmarshal(body, result); err != nil {
			t.Fatalf("decode local relay %s: %s, %v", action, body, err)
		}
	}
	return response.StatusCode
}

type lanFailedStdout struct{}

func (lanFailedStdout) Write([]byte) (int, error) { return 0, io.ErrShortWrite }

type lanViewFixture struct {
	host   *lanHostFixture
	local  *lanHostFixture
	store  *lanclient.Store
	client *lanclient.Client
	auth   relay.Auth
	id     string
}

func newLANViewFixture(t *testing.T) *lanViewFixture {
	t.Helper()
	relayclient.IsolateNativeCaller(t)
	host := newLANHostFixture(t)
	// The host and client represent different users/machines. Only the client
	// directory is later shared with the optional dashboard Service.
	clientHome := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(key, clientHome)
	}
	workspace, err := filepath.EvalSymlinks(testGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	store, err := lanclient.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	client, pending, err := store.Join(context.Background(), lanclient.JoinOptions{Invite: lanshare.EncodeInvite(host.invite), Workspace: workspace, Runtime: model.RuntimeGrok, SessionID: "facade-native-session", BindID: "facade-local-binding", CredentialHash: relay.Digest("facade-local-secret")})
	if err != nil || pending.Status != "pending" || pending.Binding != nil {
		t.Fatalf("direct pending join: %+v %v", pending, err)
	}
	if status := host.localCall(t, "/api/v1/rooms/"+host.room.ID+"/lan/accept", map[string]string{"receipt": pending.Receipt}, nil, host.management.Token()); status != http.StatusOK {
		t.Fatalf("owner admission HTTP %d", status)
	}
	admitted, err := client.Resume(context.Background())
	if err != nil || admitted.Status != "accepted" || admitted.Binding == nil || admitted.Receipt != pending.Receipt {
		t.Fatalf("direct admission recovery: %+v %v", admitted, err)
	}
	auth := relay.Auth{Slot: admitted.Binding.Slot, BindID: admitted.Binding.BindID, Generation: admitted.Binding.Generation, SessionID: "facade-native-session", Secret: "facade-local-secret"}
	if err := client.Relay(context.Background(), auth, "confirm", map[string]string{"session_id": auth.SessionID}, nil); err != nil {
		t.Fatal(err)
	}
	// Construct the dashboard only after joining and confirming without one.
	service, _ := lanGuestTestService(t)
	local := httptest.NewServer(service.Handler())
	t.Cleanup(local.Close)
	return &lanViewFixture{host: host, local: &lanHostFixture{management: service, local: local}, store: store, client: client, auth: auth, id: admitted.ID}
}

func (f *lanViewFixture) get(t *testing.T, path, token string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, f.local.local.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func TestLANOptionalServiceProjectsTheDirectClientAndSharedOwnerEvidence(t *testing.T) {
	f := newLANViewFixture(t)
	path := "/api/v1/lan/joined/" + f.id + "/"
	token := f.local.management.Token()
	if status, data := f.get(t, "/api/v1/lan/joined", token); status != http.StatusOK || !bytes.Contains(data, []byte(f.id)) {
		t.Fatalf("shared client projection: HTTP %d %s", status, data)
	}
	if rooms := f.local.management.registry.Snapshot(false).Rooms; len(rooms) != 0 {
		t.Fatal("optional Service created a second authoritative Room")
	}
	if _, err := os.Stat(filepath.Join(f.local.management.registry.Root(), "lan", "guests")); !os.IsNotExist(err) {
		t.Fatal("Service owns duplicate guest state")
	}
	if status := lanLocalRelayCall(t, f.local, f.id, f.auth, "status", nil, nil); status != http.StatusUnauthorized {
		t.Fatalf("optional Service accepted joined agent relay credentials: HTTP %d", status)
	}
	if status := f.local.localCall(t, "/api/v1/lan/join", map[string]string{"invite": lanshare.EncodeInvite(f.host.invite)}, nil, token); status != http.StatusNotFound {
		t.Fatalf("removed local join route still exists: HTTP %d", status)
	}

	const evidence = "#!/bin/sh\nprintf 'explicit evidence; do not execute automatically\\n'\n"
	attachment, err := f.client.Upload(context.Background(), f.auth, "file", "repro.sh", strings.NewReader(evidence))
	if err != nil {
		t.Fatal(err)
	}
	var published relay.Message
	if err := f.client.Relay(context.Background(), f.auth, "send", relay.SendRequest{ID: "client-evidence", Text: "Please inspect the shared reproduction.", AttachmentIDs: []string{attachment.ID}}, &published); err != nil {
		t.Fatal(err)
	}
	var history relay.HistoryPage
	if status := f.local.localCall(t, path+"history", nil, &history, token); status != http.StatusOK || len(history.Messages) != 1 || history.Messages[0].ID != published.ID {
		t.Fatal("dashboard and direct CLI did not share host history")
	}
	if status, data := f.get(t, path+"attachments/"+attachment.ID, token); status != http.StatusOK || string(data) != evidence {
		t.Fatalf("verified shared evidence: HTTP %d", status)
	}
	if status, _ := f.get(t, path+"attachments/"+attachment.ID, ""); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated local owner downloaded evidence: HTTP %d", status)
	}

	human := relay.SendRequest{ID: "owner-note", Text: "Review this shared evidence; tool approval remains local.", To: model.ActorSlot2, AttachmentIDs: []string{attachment.ID}}
	var first, again relay.Message
	if status := f.local.localCall(t, path+"send", human, &first, token); status != http.StatusOK || first.From != model.ActorUser || !strings.HasPrefix(first.Author, "lan:") {
		t.Fatal("shared human note lost membership provenance")
	}
	if status := f.local.localCall(t, path+"send", human, &again, token); status != http.StatusOK || again.ID != first.ID {
		t.Fatal("owner retry created duplicate publication")
	}
	var receipt struct {
		Accepted bool           `json:"accepted"`
		Message  *relay.Message `json:"message"`
	}
	if status := f.local.localCall(t, path+"receipt", map[string]string{"id": human.ID}, &receipt, token); status != http.StatusOK || !receipt.Accepted || receipt.Message == nil || receipt.Message.ID != first.ID {
		t.Fatal("owner could not recover original publication receipt")
	}
	var summary relay.Summary
	if status := f.local.localCall(t, path+"summary", nil, &summary, token); status != http.StatusOK {
		t.Fatal("owner summary unavailable")
	}
	for _, forbidden := range []string{"accept", "revoke", "wake-reserve", "confirm", "bind", "upload"} {
		if status := f.local.localCall(t, path+forbidden, nil, nil, token); status != http.StatusNotFound {
			t.Fatalf("owner facade forwarded %s: HTTP %d", forbidden, status)
		}
	}
	if status := f.local.localCall(t, path+"send", map[string]any{"text": "attempt", "session_id": "foreign"}, nil, token); status != http.StatusBadRequest {
		t.Fatalf("facade accepted a private relay field: HTTP %d", status)
	}

	// A second private Store sees the same authoritative client association;
	// the optional Service captures that path, regardless of later ambient env.
	other, err := lanclient.OpenAt(f.store.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Get(context.Background(), f.id); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(key, t.TempDir())
	}
	if status, data := f.get(t, "/api/v1/lan/joined", token); status != http.StatusOK || !bytes.Contains(data, []byte(f.id)) {
		t.Fatal("ambient config change retargeted an existing observer")
	}
	for _, private := range []string{f.auth.SessionID, f.auth.Secret, f.host.owner.SessionID, f.host.room.DataDir} {
		data, _ := json.Marshal([]any{history, first, receipt, summary})
		if bytes.Contains(data, []byte(private)) {
			t.Fatalf("shared view exposed private identity %q", private)
		}
	}

	if status := f.host.localCall(t, "/api/v1/rooms/"+f.host.room.ID+"/lan/revoke", nil, nil, f.host.management.Token()); status != http.StatusOK {
		t.Fatal("host revoke failed")
	}
	if status, _ := f.get(t, path+"attachments/"+attachment.ID, token); status != http.StatusForbidden {
		t.Fatalf("cached evidence survived revocation: HTTP %d", status)
	}
	if status := f.local.localCall(t, path+"history", nil, nil, token); status != http.StatusForbidden {
		t.Fatalf("cached membership hid revocation: HTTP %d", status)
	}
	if status := f.local.localCall(t, path+"leave", nil, nil, token); status != http.StatusOK {
		t.Fatalf("owner could not leave revoked association: HTTP %d", status)
	}
	metadata, err := f.client.Metadata(context.Background())
	if err != nil || metadata.Status != "left" {
		t.Fatalf("owner leave did not retire direct client state: %+v %v", metadata, err)
	}
}

func TestLANOptionalServiceDiscoversLaterJoinAndStopsAtLocalDetach(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	service, project := lanGuestTestService(t)
	if list := service.lanGuests.summaries(); len(list) != 0 {
		t.Fatal("fresh observer invented a membership")
	}
	store, client, auth := lanAcceptedClient(t, project.Root, model.RuntimeGrok, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	metadata, err := client.Metadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if list := service.lanGuests.summaries(); len(list) != 1 || list[0].ID != metadata.ID {
		t.Fatal("running optional Service failed to discover later direct join")
	}
	observed := service.lanGuests.get(metadata.ID)
	if observed == nil || service.lanGuests.get(metadata.ID) != observed {
		t.Fatal("optional observer did not reuse local wake owner")
	}
	if observed.client == client || service.lanGuests.store.Root() != store.Root() {
		t.Fatal("fixture did not use independent client pools over one store")
	}
	if err := client.Detach(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	service.lanGuests.refresh(observed)
	if list := service.lanGuests.summaries(); len(list) != 1 || list[0].Status != "detached" {
		t.Fatal("optional Service did not observe explicit detach")
	}
	if observed.waker != nil {
		t.Fatal("detached membership acquired a wake worker")
	}
	if service.lanGuests.get("lan_nonexistent") != nil {
		t.Fatal("unknown client projection existed")
	}
}

func TestLANOptionalServiceDetachesPendingClientWithoutHostApproval(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	host := newLANHostFixture(t)
	clientHome := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		t.Setenv(key, clientHome)
	}
	service, project := lanGuestTestService(t)
	local := httptest.NewServer(service.Handler())
	defer local.Close()
	fixture := &lanHostFixture{management: service, local: local}
	store, err := lanclient.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	client, pending, err := store.Join(context.Background(), lanclient.JoinOptions{
		Invite: lanshare.EncodeInvite(host.invite), Workspace: project.Root,
		Runtime: model.RuntimeGrok, SessionID: "pending-dashboard-session", BindID: "pending-dashboard-binding", CredentialHash: relay.Digest("pending-dashboard-secret"),
	})
	if err != nil || pending.Status != "pending" || pending.Binding != nil {
		t.Fatalf("pending direct join: %+v %v", pending, err)
	}
	path := "/api/v1/lan/joined/" + pending.ID + "/detach"
	if status := fixture.localCall(t, path, nil, nil, ""); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dashboard detached a pending client: HTTP %d", status)
	}
	// Detach must retire only local state even if the host is unavailable and
	// has never approved the request. No remote generation exists yet.
	host.remote.Close()
	var result map[string]bool
	if status := fixture.localCall(t, path, nil, &result, service.Token()); status != http.StatusOK || !result["detached"] {
		t.Fatalf("local pending detach: HTTP %d, %+v", status, result)
	}
	metadata, err := client.Metadata(context.Background())
	if err != nil || metadata.Status != "detached" || metadata.Generation != 0 {
		t.Fatalf("dashboard did not retire the same pending client state: %+v, %v", metadata, err)
	}
	if _, err := client.Resume(context.Background()); err == nil {
		t.Fatal("detached pending request resumed admission")
	}
}
