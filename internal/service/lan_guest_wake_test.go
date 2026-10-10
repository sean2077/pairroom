package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

type lanWakeFixture struct {
	mu           sync.Mutex
	service      *ManagementServer
	guest        *lanGuest
	adapter      *lanGuestWakeRelay
	admission    lanshare.JoinResponse
	candidate    relay.WakeCandidate
	reserved     bool
	reserveCalls int
	lostResponse bool
	recordIDs    []string
}

func newLANGuestWakeFixture(t *testing.T) *lanWakeFixture {
	t.Helper()
	s, project := lanGuestTestService(t)
	f := &lanWakeFixture{service: s, candidate: relay.WakeCandidate{
		MessageID: "wake-message", Target: model.ActorSlot2, Runtime: model.RuntimeCodex,
		BindID: "host-binding", Generation: 7, Enabled: true, QueueStart: true, Remote: true,
	}}
	invite, _ := lanGuestTestRemote(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		for _, private := range []string{"guest-native-session", "guest-relay-secret", project.Root, nativeWakeNudge} {
			if strings.Contains(string(body), private) || strings.Contains(r.Header.Get("Authorization"), private) {
				t.Errorf("wake coordination exported private local data %q", private)
			}
		}
		if r.Header.Get("X-PairRoom-LAN-Bind") != "host-binding" || r.Header.Get("X-PairRoom-LAN-Generation") != "7" {
			t.Error("wake operation lost admitted binding scope")
		}
		var request map[string]string
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch filepath.Base(r.URL.Path) {
		case "join-status":
			_ = json.NewEncoder(w).Encode(f.admission)
		case "wake-candidate":
			// Deliberately omit the host reservation here: even a stale response
			// must not clear the guest's durable spent record after a restart.
			_ = json.NewEncoder(w).Encode(map[string]any{"candidate": f.candidate})
		case "wake-reserve":
			f.reserveCalls++
			if f.reserved {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"code":"wake_reserved","error":"already reserved"}`)
				return
			}
			f.reserved = true
			if f.lostResponse {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":"response unavailable after reservation"}`)
				return
			}
			_, _ = io.WriteString(w, `{"reserved":true}`)
		case "wake-record":
			f.recordIDs = append(f.recordIDs, request["id"])
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Errorf("wake maintenance attempted conversation operation %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	request := lanLocalJoinRequest{Workspace: project.Root, Runtime: model.RuntimeCodex,
		SessionID: "guest-native-session", BindID: "guest-binding", CredentialHash: relay.Digest("guest-relay-secret")}
	guest, err := s.lanGuests.prepare(invite, request)
	if err != nil {
		t.Fatal(err)
	}
	// Drive maintenance explicitly so tests observe ordering without timing a
	// two-second poll or launching a vendor process.
	s.lanGuests.cancel()
	s.lanGuests.wg.Wait()
	key, err := guest.record.Identity.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	collaboration, err := (model.Collaboration{}).ForCreation()
	if err != nil {
		t.Fatal(err)
	}
	f.admission = lanshare.JoinResponse{Status: "accepted", Receipt: lanshare.EncodeReceipt(lanshare.Receipt{
		RoomID: invite.RoomID, RequestID: guest.record.RequestID, Fingerprint: key,
	}), Room: &lanshare.RoomInfo{RoomID: invite.RoomID, Name: "Wake Room", Slot: model.ActorSlot2,
		Generation: 7, BindID: "host-binding", Runtimes: map[model.ActorID]model.RuntimeKind{
			model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex,
		}, Collaboration: &collaboration}}
	if err := s.lanGuests.updateAdmission(guest, f.admission); err != nil {
		t.Fatal(err)
	}
	f.guest = guest
	f.adapter = &lanGuestWakeRelay{guest: guest, ctx: context.Background()}
	return f
}

func TestLANGuestWakeCommitsBothReservationsBeforeLocalEffectAndRestart(t *testing.T) {
	f := newLANGuestWakeFixture(t)
	effects := 0
	waker := newNativeWaker(nativeWakerConfig{Relay: f.adapter,
		Wait: func(context.Context, time.Duration) error { return nil },
		Run: func(_ context.Context, command string, args ...string) error {
			var saved lanGuestRecord
			if err := privatefile.ReadJSON(filepath.Join(f.guest.dir, "guest.json"), 2<<20, &saved); err != nil || len(saved.Spent) != 1 || saved.Spent[0].MessageID != "wake-message" {
				t.Fatal("local wake effect preceded its durable spent record")
			}
			f.mu.Lock()
			hostReserved := f.reserved
			f.mu.Unlock()
			if !hostReserved || command != "codex" || strings.Join(args, "\x00") != strings.Join([]string{"queue", "--thread", "guest-native-session", "--message", nativeWakeNudge}, "\x00") {
				t.Fatal("wake did not use the reserved fixed nudge and the guest's own session")
			}
			effects++
			return nil
		}})
	defer waker.Close()
	if err := waker.Wake(context.Background(), "wake-message"); err != nil || effects != 1 {
		t.Fatalf("first wake: effects=%d err=%v", effects, err)
	}
	f.mu.Lock()
	if len(f.recordIDs) != 1 || f.recordIDs[0] != "wake-message" {
		t.Error("outcome lost its original reservation identity")
	}
	f.mu.Unlock()
	saved, err := readLANGuest(filepath.Join(f.guest.dir, "guest.json"))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := f.service.lanGuests.open(saved, f.guest.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.client.CloseIdleConnections()
	restarted := &lanGuestWakeRelay{guest: reopened, ctx: context.Background()}
	head, ok := restarted.WakeCandidate("wake-message")
	if !ok || !head.Reserved || head.Remote || head.BindID != "guest-binding" || head.Generation != 7 || head.SessionID != "guest-native-session" {
		t.Fatal("restart lost the spent receipt or confused local and host identities")
	}
	if err := restarted.ReserveWake("wake-message", model.ActorSlot2); !errors.Is(err, relay.ErrWakeReserved) {
		t.Fatalf("restart repeated an already spent wake: %v", err)
	}
	reservations := restarted.WakeReservations()
	reservations[0].MessageID = "caller-mutation"
	if restarted.WakeReservations()[0].MessageID != "wake-message" {
		t.Fatal("a diagnostic caller changed the spent journal")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveCalls != 1 {
		t.Fatal("local spent reservation was retried remotely")
	}
}

func TestLANGuestWakeUncertainReservationAndPrivateWriteFailureNeverAuthorizeEffect(t *testing.T) {
	for _, failure := range []string{"lost-response", "private-write"} {
		t.Run(failure, func(t *testing.T) {
			f := newLANGuestWakeFixture(t)
			if failure == "lost-response" {
				f.lostResponse = true
			} else {
				path := filepath.Join(f.guest.dir, "guest.json")
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.adapter.ReserveWake("wake-message", model.ActorSlot2); err == nil {
				t.Fatal("uncertain reservation or failed private commit authorized a wake")
			}
			if len(f.adapter.WakeReservations()) != 0 {
				t.Fatal("failed local commit appeared durable in memory")
			}
			if err := f.adapter.ReserveWake("wake-message", model.ActorSlot2); !errors.Is(err, relay.ErrWakeReserved) {
				t.Fatalf("original host reservation allowed a repeated effect: %v", err)
			}
		})
	}
}

func TestLANGuestWakeRejectsWrongTargetsAndFailsClosedAtJournalLimit(t *testing.T) {
	f := newLANGuestWakeFixture(t)
	if _, ok := f.adapter.WakeCandidate("different-message"); ok {
		t.Fatal("wrong message candidate accepted")
	}
	if err := f.adapter.ReserveWake("wake-message", model.ActorSlot1); !errors.Is(err, relay.ErrAuth) {
		t.Fatal("wake reserved the host's local slot")
	}
	if err := f.adapter.RecordWake("suppressed", "disabled", model.ActorSlot1); !errors.Is(err, relay.ErrAuth) {
		t.Fatal("guest recorded an outcome for another slot")
	}
	f.mu.Lock()
	f.candidate.Target = model.ActorSlot1
	f.mu.Unlock()
	if heads := f.adapter.WakeHeads(); len(heads) != 0 {
		t.Fatal("foreign target appeared in local wake heads")
	}
	f.guest.mu.Lock()
	f.guest.record.Spent = make([]relay.WakeReservation, 4096)
	f.guest.mu.Unlock()
	if err := f.adapter.ReserveWake("wake-message", model.ActorSlot2); !errors.Is(err, errNativeWakeAudit) {
		t.Fatal("full journal authorized an unrecordable wake")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveCalls != 0 {
		t.Fatal("invalid local wake state reached host reservation")
	}
}

func TestLANGuestReadinessRefreshKeepsLocalWakerAndDoesNotConsumeInbox(t *testing.T) {
	f := newLANGuestWakeFixture(t)
	f.mu.Lock()
	f.candidate.Enabled = false
	f.mu.Unlock()
	manager := &lanGuestManager{server: f.service, ctx: context.Background()}
	manager.refresh(f.guest)
	f.guest.mu.Lock()
	waker := f.guest.waker
	f.guest.mu.Unlock()
	if waker == nil || len(waker.relay.WakeReservations()) != 0 {
		t.Fatal("readiness refresh did not establish a body-free local waker")
	}
	manager.refresh(f.guest)
	f.guest.mu.Lock()
	same := f.guest.waker == waker
	f.guest.mu.Unlock()
	if !same {
		t.Fatal("readiness polling replaced local wake ownership")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveCalls != 0 || len(f.recordIDs) != 0 {
		t.Fatal("disabled readiness refresh reserved or acknowledged work")
	}
}
