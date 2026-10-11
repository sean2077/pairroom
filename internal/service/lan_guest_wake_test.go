package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

type lanWakeFixture struct {
	mu                    sync.Mutex
	service               *ManagementServer
	store                 *lanclient.Store
	guest                 *lanGuest
	auth                  relay.Auth
	adapter               *lanGuestWakeRelay
	candidate             relay.WakeCandidate
	reserved              bool
	reserveCalls          int
	lostResponse          bool
	beforeReserveResponse func()
	recordIDs             []string
}

func newLANGuestWakeFixture(t *testing.T) *lanWakeFixture {
	t.Helper()
	relayclient.IsolateNativeCaller(t)
	s, project := lanGuestTestService(t)
	// Drive the optional observer explicitly; the direct client has no worker.
	s.lanGuests.cancel()
	s.lanGuests.wg.Wait()
	f := &lanWakeFixture{service: s, candidate: relay.WakeCandidate{MessageID: "wake-message", Target: model.ActorSlot2, Runtime: model.RuntimeCodex, BindID: "host-binding", Generation: 7, Enabled: true, QueueStart: true, Remote: true}}
	store, client, auth := lanAcceptedClient(t, project.Root, model.RuntimeCodex, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		for _, private := range []string{"guest-native-session", "guest-relay-secret", project.Root, nativeWakeNudge} {
			if strings.Contains(string(body), private) || strings.Contains(r.Header.Get("Authorization"), private) {
				t.Errorf("wake coordination exported private local data %q", private)
			}
		}
		var request map[string]string
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		switch filepath.Base(r.URL.Path) {
		case "wake-candidate":
			// A stale host candidate must not clear the durable local spent fact.
			_ = json.NewEncoder(w).Encode(map[string]any{"candidate": f.candidate})
		case "wake-reserve":
			f.reserveCalls++
			if f.reserved {
				w.WriteHeader(http.StatusConflict)
				_, _ = io.WriteString(w, `{"code":"wake_reserved","error":"already reserved"}`)
				return
			}
			f.reserved = true
			if f.beforeReserveResponse != nil {
				f.beforeReserveResponse()
			}
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
			t.Errorf("observer attempted a conversation operation %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	f.store, f.auth, f.guest = store, auth, &lanGuest{client: client}
	f.adapter = &lanGuestWakeRelay{guest: f.guest, ctx: context.Background()}
	t.Cleanup(func() {
		f.guest.mu.Lock()
		waker := f.guest.waker
		f.guest.mu.Unlock()
		if waker != nil {
			waker.Close()
		}
	})
	return f
}

func (f *lanWakeFixture) reopened(t *testing.T) *lanGuestWakeRelay {
	t.Helper()
	metadata, err := f.guest.client.Metadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	store, err := lanclient.OpenAt(f.store.Root())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	client, err := store.Get(context.Background(), metadata.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &lanGuestWakeRelay{guest: &lanGuest{client: client}, ctx: context.Background()}
}

func TestLANGuestWakeCommitsBothReservationsBeforeLocalEffectAndRestart(t *testing.T) {
	f := newLANGuestWakeFixture(t)
	effects := 0
	waker := newNativeWaker(nativeWakerConfig{Relay: f.adapter,
		Wait: func(context.Context, time.Duration) error { return nil },
		Run: func(_ context.Context, command string, args ...string) error {
			// A separately opened Store must read the spent fact before effect.
			spent := f.reopened(t).WakeReservations()
			if len(spent) != 1 || spent[0].MessageID != "wake-message" {
				t.Fatal("effect preceded durable local spent record")
			}
			f.mu.Lock()
			hostReserved := f.reserved
			f.mu.Unlock()
			if !hostReserved || command != "codex" || strings.Join(args, "\x00") != strings.Join([]string{"queue", "--thread", "guest-native-session", "--message", nativeWakeNudge}, "\x00") {
				t.Fatal("wake did not use reserved fixed nudge and local session")
			}
			effects++
			return nil
		}})
	defer waker.Close()
	if err := waker.Wake(context.Background(), "wake-message"); err != nil || effects != 1 {
		t.Fatalf("wake effects=%d error=%v", effects, err)
	}
	f.mu.Lock()
	if len(f.recordIDs) != 1 || f.recordIDs[0] != "wake-message" {
		t.Error("outcome lost original reservation identity")
	}
	f.mu.Unlock()
	restarted := f.reopened(t)
	head, ok := restarted.WakeCandidate("wake-message")
	if !ok || !head.Reserved || head.Remote || head.BindID != "guest-binding" || head.Generation != 7 || head.SessionID != "guest-native-session" {
		t.Fatal("restart lost spent receipt or confused host and local binding")
	}
	if err := restarted.ReserveWake("wake-message", model.ActorSlot2); !errors.Is(err, relay.ErrWakeReserved) {
		t.Fatalf("repeated effect: %v", err)
	}
	reservations := restarted.WakeReservations()
	reservations[0].MessageID = "caller-mutation"
	if restarted.WakeReservations()[0].MessageID != "wake-message" {
		t.Fatal("diagnostic caller changed durable journal")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveCalls != 1 {
		t.Fatal("spent reservation retried at host")
	}
}

func TestLANGuestWakeUncertainReservationAndPrivateWriteFailureNeverAuthorizeEffect(t *testing.T) {
	for _, failure := range []string{"lost-response", "private-write"} {
		t.Run(failure, func(t *testing.T) {
			f := newLANGuestWakeFixture(t)
			metadata, err := f.guest.client.Metadata(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(f.store.Root(), metadata.ID, "client.json")
			if failure == "lost-response" {
				f.lostResponse = true
			} else {
				f.beforeReserveResponse = func() {
					if err := os.Rename(path, path+".saved"); err != nil {
						t.Error(err)
						return
					}
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Error(err)
					}
				}
			}
			if err := f.adapter.ReserveWake("wake-message", model.ActorSlot2); err == nil {
				t.Fatal("uncertain reservation authorized effect")
			}
			if failure == "private-write" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".saved", path); err != nil {
					t.Fatal(err)
				}
			}
			if len(f.adapter.WakeReservations()) != 0 {
				t.Fatal("failed local commit appeared durable")
			}
			if err := f.adapter.ReserveWake("wake-message", model.ActorSlot2); !errors.Is(err, relay.ErrWakeReserved) {
				t.Fatalf("uncertain host reservation allowed another effect: %v", err)
			}
		})
	}
}

func TestLANGuestWakeRejectsWrongTargetsAndFailsClosedAtJournalLimit(t *testing.T) {
	f := newLANGuestWakeFixture(t)
	if _, ok := f.adapter.WakeCandidate("different-message"); ok {
		t.Fatal("wrong candidate accepted")
	}
	if err := f.adapter.ReserveWake("wake-message", model.ActorSlot1); !errors.Is(err, relay.ErrAuth) {
		t.Fatal("wake reserved host slot")
	}
	if err := f.adapter.RecordWake("suppressed", "disabled", model.ActorSlot1); !errors.Is(err, relay.ErrAuth) {
		t.Fatal("guest recorded another slot outcome")
	}
	f.mu.Lock()
	f.candidate.Target = model.ActorSlot1
	f.mu.Unlock()
	if heads := f.adapter.WakeHeads(); len(heads) != 0 {
		t.Fatal("foreign target reached local observer")
	}
	metadata, err := f.guest.client.Metadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.store.Root(), metadata.ID, "client.json")
	var record map[string]any
	if err := privatefile.ReadJSON(path, 2<<20, &record); err != nil {
		t.Fatal(err)
	}
	spent := make([]relay.WakeReservation, 4096)
	for i := range spent {
		spent[i] = relay.WakeReservation{MessageID: fmt.Sprintf("spent-%d", i), Target: model.ActorSlot2, At: time.Now().UTC()}
	}
	record["spent"] = spent
	if err := privatefile.WriteJSON(path, record); err != nil {
		t.Fatal(err)
	}
	if err := f.adapter.ReserveWake("wake-message", model.ActorSlot2); err == nil {
		t.Fatal("full journal authorized unrecordable effect")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveCalls != 0 {
		t.Fatal("invalid wake state reached host reservation")
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
		t.Fatal("readiness did not establish body-free waker")
	}
	manager.refresh(f.guest)
	f.guest.mu.Lock()
	same := f.guest.waker == waker
	f.guest.mu.Unlock()
	if !same {
		t.Fatal("readiness replaced local wake ownership")
	}
	if err := f.guest.client.Detach(context.Background(), f.auth); err != nil {
		t.Fatal(err)
	}
	manager.refresh(f.guest)
	if _, ok := f.adapter.WakeCandidate("wake-message"); ok {
		t.Fatal("detached client still authorized observer")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reserveCalls != 0 || len(f.recordIDs) != 0 {
		t.Fatal("readiness reserved or acknowledged inbox work")
	}
}

// The optional observer shares the host's per-source request budget with the
// user's own commands: its period grows with the number of joined Rooms, and a
// failed pass backs off instead of retrying on every tick.
func TestObserverSpreadAndBackoffKeepTheForegroundBudgetUsable(t *testing.T) {
	if got := observerInterval(nil); got != observerPollInterval {
		t.Fatalf("no joined Rooms = %s", got)
	}
	rooms := []lanGuestSummary{{Status: "pending"}, {Status: "accepted"}, {Status: "accepted"}, {Status: "accepted"}}
	if got := observerInterval(rooms); got != 4*observerPollInterval {
		t.Fatalf("four joined Rooms = %s", got)
	}
	guest := &lanGuest{}
	guest.schedule(observerPollInterval)
	if guest.pollDelay != 0 || guest.nextPoll.IsZero() {
		t.Fatalf("successful pass kept a delay: %+v", guest)
	}
	guest.backoff(observerPollInterval)
	if guest.pollDelay != observerPollInterval {
		t.Fatalf("first failure = %s", guest.pollDelay)
	}
	guest.backoff(observerPollInterval)
	if guest.pollDelay != 2*observerPollInterval {
		t.Fatalf("second failure = %s", guest.pollDelay)
	}
	for i := 0; i < 10; i++ {
		guest.backoff(observerPollInterval)
	}
	if guest.pollDelay != observerMaxBackoff {
		t.Fatalf("repeated failures = %s, want %s", guest.pollDelay, observerMaxBackoff)
	}
	guest.schedule(observerPollInterval)
	if guest.pollDelay != 0 {
		t.Fatalf("recovered pass kept a delay: %s", guest.pollDelay)
	}
}

func TestObserverTerminalRoomsDoNotDelayLiveRooms(t *testing.T) {
	rooms := []lanGuestSummary{{Status: "accepted"}, {Status: "pending"}}
	for i := 0; i < 64; i++ {
		for _, status := range []string{"left", "detached", "revoked", "expired"} {
			rooms = append(rooms, lanGuestSummary{Status: status})
		}
	}
	if got := observerInterval(rooms); got != 4*time.Second {
		t.Fatalf("retired records delayed two live Rooms: %s", got)
	}
	if got := observerInterval(rooms[2:]); got != 2*time.Second {
		t.Fatalf("terminal-only catalog changed the base period: %s", got)
	}
}

func TestObserverFailureNeverPollsFasterThanHealthyPeriod(t *testing.T) {
	// Sixty-four live Rooms already require a period longer than the ordinary
	// one-minute failure cap. Repeated failures cannot increase that traffic.
	base := 128 * time.Second
	guest := &lanGuest{}
	previous := base
	for i := 0; i < 5; i++ {
		guest.backoff(base)
		if guest.pollDelay < previous {
			t.Fatalf("failure %d shortened the healthy/backoff period from %s to %s", i+1, previous, guest.pollDelay)
		}
		previous = guest.pollDelay
	}
}
