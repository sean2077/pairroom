package lanclient

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/relay"
)

func TestContactObservationSeparatesHTTPDenialFromUnreachableHost(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, _ := f.join(t, s)
	before, err := os.ReadFile(filepath.Join(c.dir, "client.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		f.setHandler(func(w http.ResponseWriter, _ *http.Request, _ string, _ []byte) {
			w.WriteHeader(status)
			reply(w, map[string]string{"error": "private remote diagnostic"})
		})
		if err := c.Relay(ctx, auth, "summary", nil, new(relay.Summary)); err == nil {
			t.Fatal("HTTP denial was treated as a successful operation")
		}
		snapshot, err := c.Snapshot(ctx)
		if err != nil || !snapshot.Connected || snapshot.LastSeen.IsZero() || snapshot.Status != "accepted" {
			t.Fatalf("HTTP %d was misreported as a network failure: %+v %v", status, snapshot, err)
		}
	}
	last, err := c.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.server.Close()
	if err := c.Relay(ctx, auth, "summary", nil, new(relay.Summary)); !errors.Is(err, ErrTransportUnavailable) {
		t.Fatalf("no HTTP response classification: %v", err)
	}
	offline, err := c.Snapshot(ctx)
	if err != nil || offline.Connected || offline.LastSeen != last.LastSeen || offline.Status != "accepted" {
		t.Fatalf("network failure corrupted saved membership/contact history: %+v %v", offline, err)
	}
	after, err := os.ReadFile(filepath.Join(c.dir, "client.json"))
	if err != nil || string(after) != string(before) {
		t.Fatal("network observation rewrote private membership or delivery records")
	}
}

func TestDeniedUploadAndRetiredMembershipStillReportHostContact(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, _ := f.join(t, s)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, _ string, _ []byte) {
		w.WriteHeader(http.StatusForbidden)
		reply(w, map[string]string{"error": "revoked"})
	})
	r, err := c.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Upload(ctx, auth, "file", "repro.sh", strings.NewReader("echo repro")); !errors.Is(err, relay.ErrAuth) {
		t.Fatalf("denial lost status classification: %v", err)
	}
	if err := c.retire(ctx, r, "revoked"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := c.Snapshot(ctx)
	if err != nil || snapshot.Status != "revoked" || !snapshot.Connected || snapshot.LastSeen.IsZero() {
		t.Fatalf("retired membership was conflated with offline transport: %+v %v", snapshot, err)
	}
	if err := c.Relay(ctx, auth, "summary", nil, nil); !errors.Is(err, relay.ErrAuth) {
		t.Fatalf("reachable host revived retired authority: %v", err)
	}
	// Pending contact also cannot imply membership or native-session acceptance.
	other := newRemote(t)
	other.status = "pending"
	options := other.options(t)
	options.SessionID = "pending-native-session"
	options.BindID = "pending-local-binding"
	pending, result, err := s.Join(ctx, options)
	if err != nil || result.Status != "pending" {
		t.Fatalf("pending request: %+v %v", result, err)
	}
	got, err := pending.Snapshot(ctx)
	if err != nil || !got.Connected || got.Status != "pending" || result.Binding != nil {
		t.Fatalf("contact manufactured admission: %+v %v", got, err)
	}
}
