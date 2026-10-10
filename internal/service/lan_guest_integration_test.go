package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

type lanRecordedRequest struct {
	Action  string
	ID      string
	Receipt string
}

// Lose an answer only after the real remote handler committed its effect.
// This never substitutes a successful response for the Service implementation.
type lanLostResponseTransport struct {
	base    http.RoundTripper
	mu      sync.Mutex
	drop    string
	dropped bool
	seen    []lanRecordedRequest
}

func (tr *lanLostResponseTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	observed := lanRecordedRequest{Action: filepath.Base(r.URL.Path)}
	if r.GetBody != nil && observed.Action == "ack" {
		body, err := r.GetBody()
		if err != nil {
			return nil, err
		}
		var req struct{ ID, Receipt string }
		err = json.NewDecoder(body).Decode(&req)
		_ = body.Close()
		if err != nil {
			return nil, err
		}
		observed.ID, observed.Receipt = req.ID, req.Receipt
	}
	tr.mu.Lock()
	tr.seen = append(tr.seen, observed)
	tr.mu.Unlock()
	response, err := tr.base.RoundTrip(r)
	if err != nil {
		return response, err
	}
	tr.mu.Lock()
	lose := observed.Action == tr.drop && !tr.dropped && response.StatusCode == http.StatusOK
	if lose {
		tr.dropped = true
	}
	tr.mu.Unlock()
	if lose {
		_ = response.Body.Close()
		return nil, errors.New("fixture dropped a committed LAN response")
	}
	return response, nil
}

func (tr *lanLostResponseTransport) CloseIdleConnections() {
	if base, ok := tr.base.(interface{ CloseIdleConnections() }); ok {
		base.CloseIdleConnections()
	}
}

type lanBridgeFixture struct {
	local, host *lanHostFixture
	project     Project
	endpoint    string
	session     string
	id          string
	guest       *lanGuest
	auth        relay.Auth
}

func (f *lanBridgeFixture) run(t *testing.T, output io.Writer, args ...string) error {
	t.Helper()
	clearNativeSessionEnv(t)
	t.Setenv("GROK_SESSION_ID", f.session)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	args = append(args, "--repo", f.project.Root, "--service-file", f.endpoint)
	return relayclient.Run(ctx, args, strings.NewReader(""), output, io.Discard)
}

func (f *lanBridgeFixture) cli(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	var output bytes.Buffer
	err := f.run(t, &output, args...)
	return output.Bytes(), err
}

func joinLANBridgeFixture(t *testing.T, local, host *lanHostFixture, name string) *lanBridgeFixture {
	t.Helper()
	project, ok := local.management.registry.Project(local.room.ProjectID)
	if !ok {
		t.Fatal("local project not found")
	}
	f := &lanBridgeFixture{local: local, host: host, project: project, endpoint: filepath.Join(local.management.registry.Root(), relay.EndpointFile), session: "official-guest-" + name, id: lanGuestID(host.invite)}
	if err := relay.WriteEndpoint(local.management.registry.Root(), relay.Endpoint{URL: local.local.URL, Token: local.management.cliToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cli(t, "install", "--runtime", "grok"); err != nil {
		t.Fatal(err)
	}
	invitation := lanshare.EncodeInvite(host.invite)
	data, err := f.cli(t, "join", invitation)
	var pending struct{ Status, Receipt, Room string }
	if err != nil || json.Unmarshal(data, &pending) != nil || pending.Status != "pending" || pending.Room != f.id {
		t.Fatalf("real CLI pending join: %s, %v", data, err)
	}
	f.guest = local.management.lanGuests.get(f.id)
	if f.guest == nil {
		t.Fatal("local Service did not retain pending identity")
	}
	// Deterministic fault injection below controls refresh explicitly. The two
	// Services and both TLS handlers remain real and independent throughout.
	local.management.lanGuests.cancel()
	if err := f.guest.call(context.Background(), "history", nil, new(relay.HistoryPage)); err == nil {
		t.Fatal("pending guest read host history")
	}
	statePath := filepath.Join(project.Root, ".pairroom", "rooms", f.id, "slots", "slot2", "state.json")
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending CLI promoted active native discovery")
	}
	var accepted lanshare.JoinResponse
	if status := host.localCall(t, "/api/v1/rooms/"+host.room.ID+"/lan/accept", map[string]string{"receipt": pending.Receipt}, &accepted, host.management.Token()); status != http.StatusOK {
		t.Fatalf("host owner admission: %d", status)
	}
	loss := &lanLostResponseTransport{base: f.guest.client.Transport, drop: "join"}
	f.guest.client.Transport = loss
	if output, err := f.cli(t, "join", invitation); err == nil || len(output) != 0 {
		t.Fatal("lost accepted join response was reported as success")
	}
	data, err = f.cli(t, "join", invitation)
	var joined struct {
		Status  string        `json:"status"`
		Room    string        `json:"room"`
		Binding relay.Binding `json:"binding"`
	}
	if err != nil || json.Unmarshal(data, &joined) != nil || joined.Status != "accepted" || joined.Room != f.id || joined.Binding.Runtime != model.RuntimeGrok || joined.Binding.SessionID != f.session || joined.Binding.Generation != accepted.Room.Generation {
		t.Fatalf("same request/key admission recovery: %s, %v", data, err)
	}
	credentialsPath := filepath.Join(project.Root, ".pairroom", "rooms", f.id, "slots", "slot2", "credentials")
	credentials, err := os.ReadFile(credentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	var credential struct{ BindID, Secret string }
	if err := json.Unmarshal(credentials, &credential); err != nil {
		t.Fatal(err)
	}
	f.auth = relay.Auth{Slot: model.ActorSlot2, BindID: joined.Binding.BindID, Generation: joined.Binding.Generation, SessionID: f.session, Secret: credential.Secret}
	if f.guest.authenticate(f.auth) != nil || !loss.dropped || f.guest.record.Receipt != pending.Receipt {
		t.Fatal("accepted guest changed its original receipt or private identity")
	}
	for _, private := range []string{credential.Secret, local.management.Token(), host.management.Token(), f.guest.record.Identity.PrivateKeyPEM, host.owner.SessionID, host.room.DataDir} {
		encoded, err := json.Marshal(private)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(private)) || bytes.Contains(data, encoded[1:len(encoded)-1]) {
			t.Fatalf("join output exposed private peer identity or credentials: %q", private)
		}
	}
	return f
}

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

func lanGuestOwnerGet(t *testing.T, f *lanBridgeFixture, path string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, f.local.local.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.local.management.Token())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

type lanStdoutObserver struct {
	bytes.Buffer
	before func()
}

func (w *lanStdoutObserver) Write(data []byte) (int, error) {
	w.before()
	return w.Buffer.Write(data)
}

func TestLANTwoServicesCLIAndSharedOwnerEvidence(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	left, right := newLANHostFixture(t), newLANHostFixture(t)
	bridges := []*lanBridgeFixture{joinLANBridgeFixture(t, right, left, "right"), joinLANBridgeFixture(t, left, right, "left")}
	if bridges[0].id == bridges[1].id || bridges[0].project.Root == bridges[1].project.Root {
		t.Fatal("two machines shared a routing identity or workspace")
	}
	for i, f := range bridges {
		t.Run(fmt.Sprintf("direction_%d", i), func(t *testing.T) {
			body := "#!/bin/sh\nprintf 'review evidence, never execute automatically\\n'\n"
			file := filepath.Join(f.project.Root, "repro.sh")
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			loss := &lanLostResponseTransport{base: f.guest.client.Transport, drop: "send"}
			f.guest.client.Transport = loss
			args := []string{"send", "--id", "bug-evidence", "--text", "Please inspect the explicit reproduction artifact.", "--file", file}
			if output, err := f.cli(t, args...); err == nil || len(output) != 0 {
				t.Fatal("lost send response did not preserve uncertainty")
			}
			data, err := f.cli(t, args...)
			var publication struct{ Published, ClientID, State string }
			if err != nil || json.Unmarshal(data, &publication) != nil || publication.Published == "" || !loss.dropped {
				t.Fatalf("same-ID evidence publication recovery: %s, %v", data, err)
			}
			history, err := f.host.native.engine.History(relay.HistoryQuery{ID: publication.Published})
			if err != nil || len(history.Messages) != 1 || len(history.Messages[0].Attachments) != 1 {
				t.Fatal("real host did not retain one canonical publication")
			}
			accepted := history.Messages[0]
			var hostDelivery struct{ Claim *relay.Claim }
			if status := lanLocalRelayCall(t, f.host, f.host.room.ID, f.host.owner, "wait", map[string]int{"timeout_seconds": 1}, &hostDelivery); status != 200 || hostDelivery.Claim == nil || hostDelivery.Claim.ID != accepted.ID {
				t.Fatalf("host local collector did not receive the guest evidence: %d", status)
			}
			metadata, hostPath, err := f.host.native.media.Resolve(accepted.Attachments[0].ID)
			if err != nil || metadata.Kind != "file" || metadata.SHA256 != relay.Digest(body) || !lanEnvelopeHasLocalPath(hostDelivery.Claim.Envelope, hostPath) || lanEnvelopeHasPathPrefix(hostDelivery.Claim.Envelope, f.project.Root) {
				t.Fatal("host received a guest filesystem path instead of a verified local artifact")
			}
			hostBytes, err := os.ReadFile(hostPath)
			if err != nil || string(hostBytes) != body {
				t.Fatal("host evidence bytes changed across the LAN")
			}
			if status := lanLocalRelayCall(t, f.host, f.host.room.ID, f.host.owner, "ack", map[string]string{"id": hostDelivery.Claim.ID, "receipt": hostDelivery.Claim.Receipt}, nil); status != 200 {
				t.Fatal("host stdout receipt failed")
			}
			var reply relay.Message
			if status := lanLocalRelayCall(t, f.host, f.host.room.ID, f.host.owner, "send", relay.SendRequest{ID: "verified-reply", Text: "I reproduced the bug; please review this same artifact.", AttachmentIDs: []string{metadata.ID}}, &reply); status != 200 {
				t.Fatal("host could not reply through its local binding")
			}
			ackLoss := &lanLostResponseTransport{base: f.guest.client.Transport, drop: "ack"}
			f.guest.client.Transport = ackLoss
			stdout := &lanStdoutObserver{before: func() {
				page, err := f.host.native.engine.History(relay.HistoryQuery{ID: reply.ID})
				if err != nil || len(page.Messages) != 1 || page.Messages[0].State != "delivering" {
					t.Error("delivery was acknowledged before actual CLI stdout")
				}
				f.guest.mu.Lock()
				defer f.guest.mu.Unlock()
				if len(f.guest.record.Deliveries) != 1 || f.guest.record.Deliveries[0].State != "claimed" {
					t.Error("original claim receipt was not durably retained before stdout")
				}
			}}
			if err := f.run(t, stdout, "wait", "--timeout", "1"); err == nil || !strings.Contains(err.Error(), "stdout written") {
				t.Fatalf("lost ACK response did not distinguish delivered stdout: %v", err)
			}
			guestMetadata, guestPath, err := f.guest.media.Resolve(metadata.ID)
			if err != nil || guestMetadata != metadata || !lanEnvelopeHasLocalPath(stdout.String(), guestPath) || lanEnvelopeHasPathPrefix(stdout.String(), f.host.room.DataDir) || strings.Contains(stdout.String(), body) {
				t.Fatal("guest stdout did not carry its verified inert local artifact")
			}
			bytesOnDisk, err := os.ReadFile(guestPath)
			if err != nil || string(bytesOnDisk) != body {
				t.Fatal("guest evidence bytes changed across the LAN")
			}
			original := f.guest.record.Deliveries[0]
			key, _ := f.guest.record.Identity.Fingerprint()
			if original.ID != reply.ID || original.State != "stdout" || !ackLoss.dropped {
				t.Fatal("lost acknowledgement discarded the original stdout receipt")
			}
			f.local.management.lanGuests.close()
			if err := initLANGuests(f.local.management); err != nil {
				t.Fatal(err)
			}
			f.local.management.lanGuests.cancel()
			f.guest = f.local.management.lanGuests.get(f.id)
			recovery := &lanLostResponseTransport{base: f.guest.client.Transport}
			f.guest.client.Transport = recovery
			f.guest.reconcileDeliveryReceipts(context.Background())
			f.guest.reconcileDeliveryReceipts(context.Background())
			againKey, _ := f.guest.record.Identity.Fingerprint()
			if againKey != key || len(recovery.seen) != 1 || recovery.seen[0] != (lanRecordedRequest{Action: "ack", ID: original.ID, Receipt: original.Receipt}) || f.guest.record.Deliveries[0].State != "acknowledged" {
				t.Fatal("restart did not recover exactly the original receipt without claim/body replay")
			}
			if _, err := f.cli(t, "bind"); err != nil {
				t.Fatalf("admitted CLI could not resume after Service restart: %v", err)
			}
			for _, action := range []string{"status", "doctor", "history"} {
				if output, err := f.cli(t, action); err != nil || len(output) == 0 {
					t.Fatalf("joined CLI %s: %s, %v", action, output, err)
				}
			}
			// Automatic Stop publication receipts still use the local binding
			// identity even though the canonical publication belongs to the host.
			var automatic relay.Publication
			if status := lanLocalRelayCall(t, f.local, f.id, f.auth, "report", map[string]any{"report_seq": 1, "text": "A private unaddressed final reply"}, &automatic); status != 200 || automatic.BindID != f.auth.BindID {
				t.Fatal("remote publication receipt could not reconcile the local outbox")
			}
			var receipt lanGuestPublicationReceipt
			if status := lanLocalRelayCall(t, f.local, f.id, f.auth, "publication", map[string]uint64{"report_seq": 1}, &receipt); status != 200 || !receipt.Accepted || receipt.Publication.BindID != f.auth.BindID {
				t.Fatal("original Stop receipt was not recoverable through local identity")
			}
			lanExerciseSharedOwner(t, f, metadata, body)
			bad := f.auth
			bad.Generation++
			if status := lanLocalRelayCall(t, f.local, f.id, bad, "status", nil, nil); status != http.StatusUnauthorized {
				t.Fatal("stale local generation inherited joined membership")
			}
			if status := lanLocalRelayCall(t, f.local, f.id, f.auth, "accept", map[string]string{"receipt": f.guest.record.Receipt}, nil); status != http.StatusNotFound {
				t.Fatal("guest local relay forwarded host administration")
			}
			if status := f.host.localCall(t, "/api/v1/rooms/"+f.host.room.ID+"/lan/revoke", nil, nil, f.host.management.Token()); status != 200 {
				t.Fatal("host could not revoke admitted peer")
			}
			if status := lanLocalRelayCall(t, f.local, f.id, f.auth, "ack", map[string]string{"id": original.ID, "receipt": original.Receipt}, nil); status != http.StatusUnauthorized {
				t.Fatal("cached local receipt hid remote revocation")
			}
			if status, _ := lanGuestOwnerGet(t, f, "/api/v1/lan/joined/"+f.id+"/attachments/"+metadata.ID); status != http.StatusUnauthorized {
				t.Fatal("revocation left cached evidence publicly downloadable")
			}
			if i == 0 {
				if status := f.local.localCall(t, "/api/v1/lan/joined/"+f.id+"/leave", nil, nil, f.local.management.Token()); status != 200 {
					t.Fatal("owner could not explicitly leave revoked membership")
				}
			} else if status := lanLocalRelayCall(t, f.local, f.id, f.auth, "unbind", nil, nil); status != 200 {
				t.Fatal("agent could not explicitly unbind revoked membership")
			}
		})
	}
}

func lanExerciseSharedOwner(t *testing.T, f *lanBridgeFixture, metadata model.Attachment, body string) {
	t.Helper()
	path := "/api/v1/lan/joined/" + f.id + "/"
	loss := &lanLostResponseTransport{base: f.guest.client.Transport, drop: "user-send"}
	f.guest.client.Transport = loss
	request := relay.SendRequest{ID: "human-owner-note", Text: "Review this shared evidence; local tool approval remains separate.", To: model.ActorSlot2, AttachmentIDs: []string{metadata.ID}}
	if status := f.local.localCall(t, path+"send", request, nil, f.local.management.Token()); status != 502 {
		t.Fatal("owner UI did not expose a lost shared send answer")
	}
	var receipt struct {
		Accepted bool          `json:"accepted"`
		Message  relay.Message `json:"message"`
	}
	if status := f.local.localCall(t, path+"receipt", map[string]string{"id": request.ID}, &receipt, f.local.management.Token()); status != 200 || !receipt.Accepted || receipt.Message.From != model.ActorUser || !strings.HasPrefix(receipt.Message.Author, "lan:") {
		t.Fatal("owner publication recovery lost authenticated human provenance")
	}
	var again relay.Message
	if status := f.local.localCall(t, path+"send", request, &again, f.local.management.Token()); status != 200 || again.ID != receipt.Message.ID {
		t.Fatal("same-ID owner send produced another publication")
	}
	output, err := f.cli(t, "wait", "--timeout", "1")
	if err != nil || !strings.Contains(string(output), "@user (local owner)") {
		t.Fatalf("local human provenance was not rendered: %s, %v", output, err)
	}
	if output, err = f.cli(t, "send", "--id", "shared-escalation", "--to", "@user", "--text", "Please decide the remaining behavior."); err != nil {
		t.Fatalf("shared @user escalation: %s, %v", output, err)
	}
	var page relay.HistoryPage
	if status := f.local.localCall(t, path+"history", nil, &page, f.local.management.Token()); status != 200 {
		t.Fatal("joined owner could not read shared history")
	}
	found := false
	for _, message := range page.Messages {
		if message.To == model.ActorUser && message.Text == "Please decide the remaining behavior." {
			found = true
		}
	}
	if !found {
		t.Fatal("guest owner view omitted the shared @user escalation")
	}
	if status := f.local.localCall(t, path+"summary", nil, new(relay.Summary), f.local.management.Token()); status != 200 {
		t.Fatal("joined owner could not inspect shared reachability")
	}
	if status, data := lanGuestOwnerGet(t, f, "/api/v1/lan/joined"); status != 200 || !bytes.Contains(data, []byte(f.id)) {
		t.Fatal("local Service did not expose its joined Room projection")
	}
	if status, data := lanGuestOwnerGet(t, f, path+"attachments/"+metadata.ID); status != 200 || string(data) != body {
		t.Fatal("joined owner could not download verified shared evidence")
	}
	for _, enabled := range []bool{false, true} {
		if status := lanLocalRelayCall(t, f.local, f.id, f.auth, "park", map[string]bool{"enabled": enabled}, nil); status != 200 || f.guest.record.ParkEnabled != enabled {
			t.Fatal("local hook parking did not retain its own preference")
		}
	}
	if status := lanLocalRelayCall(t, f.local, f.id, f.auth, "failure", map[string]string{"error": "fixture hook diagnostic"}, nil); status != 200 {
		t.Fatal("joined hook failure was not recorded")
	}
}

type lanFailedStdout struct{}

func (lanFailedStdout) Write([]byte) (int, error) { return 0, io.ErrShortWrite }

func TestLANGuestShortStdoutNeverAcknowledgesAndExplicitRetryKeepsNewID(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	host, local := newLANHostFixture(t), newLANHostFixture(t)
	f := joinLANBridgeFixture(t, local, host, "stdout-failure")
	message, err := host.native.engine.Send(host.owner, relay.SendRequest{ID: "stdout-failure", Text: "The original collector has not written this input."})
	if err != nil {
		t.Fatal(err)
	}
	transport := &lanLostResponseTransport{base: f.guest.client.Transport}
	f.guest.client.Transport = transport
	if err := f.run(t, lanFailedStdout{}, "wait", "--timeout", "1"); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short stdout was not a failed collection: %v", err)
	}
	if len(f.guest.record.Deliveries) != 1 || f.guest.record.Deliveries[0].ID != message.ID || f.guest.record.Deliveries[0].State != "claimed" {
		t.Fatal("failed stdout became an acknowledgement fact")
	}
	originalReceipt := f.guest.record.Deliveries[0].Receipt
	f.guest.reconcileDeliveryReceipts(context.Background())
	for _, request := range transport.seen {
		if request.Action == "ack" {
			t.Fatal("network receipt alone acknowledged a failed collector")
		}
	}
	// Closing and reopening the real host runtime conservatively settles its
	// unacknowledged lease as unknown, without waiting for a wall-clock lease.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
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
	page, err := host.native.engine.History(relay.HistoryQuery{ID: message.ID})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].State != "unknown" {
		t.Fatal("host restart silently replayed an unacknowledged delivery")
	}
	retry, err := host.native.engine.Retry(message.ID)
	if err != nil || retry.ID == message.ID || retry.RetryOf != message.ID {
		t.Fatalf("explicit owner Retry did not create its own delivery identity: %+v, %v", retry, err)
	}
	output, err := f.cli(t, "wait", "--timeout", "1")
	if err != nil || !strings.Contains(string(output), message.Text) || strings.Count(string(output), message.Text) != 1 {
		t.Fatalf("explicit Retry was not collectable exactly once: %s, %v", output, err)
	}
	if len(f.guest.record.Deliveries) != 2 || f.guest.record.Deliveries[0].State != "claimed" || f.guest.record.Deliveries[1].ID != retry.ID || f.guest.record.Deliveries[1].Receipt == originalReceipt || f.guest.record.Deliveries[1].State != "acknowledged" {
		t.Fatal("explicit Retry replaced the original uncertain receipt")
	}
	page, err = host.native.engine.History(relay.HistoryQuery{ID: message.ID})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].State != "unknown" {
		t.Fatal("Retry acknowledgement changed the original unknown delivery")
	}
}

func TestLANGuestLostClaimResponseCannotAcknowledgeOrReplay(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	host, local := newLANHostFixture(t), newLANHostFixture(t)
	f := joinLANBridgeFixture(t, local, host, "claim-loss")
	message, err := host.native.engine.Send(host.owner, relay.SendRequest{ID: "lost-claim", Text: "A network response is not collector stdout."})
	if err != nil {
		t.Fatal(err)
	}
	loss := &lanLostResponseTransport{base: f.guest.client.Transport, drop: "claim"}
	f.guest.client.Transport = loss
	if output, err := f.cli(t, "wait", "--timeout", "1"); err == nil || len(output) != 0 || !loss.dropped {
		t.Fatal("lost remote claim response reached model stdout")
	}
	f.guest.reconcileDeliveryReceipts(context.Background())
	if len(f.guest.record.Deliveries) != 0 {
		t.Fatal("unknown network claim invented an original receipt")
	}
	for _, request := range loss.seen {
		if request.Action == "ack" {
			t.Fatal("lost claim was acknowledged without local stdout")
		}
	}
	page, err := host.native.engine.History(relay.HistoryQuery{ID: message.ID})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].State != "delivering" {
		t.Fatal("lost claim response replayed or acknowledged canonical work")
	}
}
