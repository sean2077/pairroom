package lanclient

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

func replacementOptions(f *remoteFixture, old JoinOptions) JoinOptions {
	invite := f.invite
	invite.InviteID = "fresh-replacement-invitation"
	old.Invite, old.BindID, old.SessionID, old.Replace = lanshare.EncodeInvite(invite), "replacement-local-binding", "replacement-native-session", true
	return old
}

func retireAndAllowReplacement(f *remoteFixture) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.nextJoinStatus = "revoked", "accepted"
	f.room.Generation++
	f.room.BindID = "replacement-remote-binding"
}

func TestReplacementRequiresFreshInviteAndAuthoritativeTerminalProof(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, options := f.join(t, s)
	candidate := replacementOptions(f, options)
	file := filepath.Join(c.dir, "client.json")
	before, _ := os.ReadFile(file)
	if _, _, err := s.Join(ctx, candidate); err == nil {
		t.Fatal("active membership was replaced without host retirement")
	}
	after, _ := os.ReadFile(file)
	if string(before) != string(after) {
		t.Fatal("rejected replacement changed the original private binding")
	}
	if _, err := os.Stat(filepath.Join(c.dir, "replacement.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("active membership rejection installed a replacement key")
	}
	if err := c.Detach(ctx, auth); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Join(ctx, candidate); err == nil {
		t.Fatal("offline local detach overrode active host membership")
	}
	f.server.Close()
	if _, _, err := s.Join(ctx, candidate); !errors.Is(err, ErrTransportUnavailable) {
		t.Fatalf("offline detached replacement bypassed host proof: %v", err)
	}
	if snapshot, err := c.Snapshot(ctx); err != nil || snapshot.Status != "detached" {
		t.Fatal("failed replacement reactivated local observer")
	}
}

func TestExplicitReplacementArchivesOriginalAndRepeatUsesNewOriginalRequest(t *testing.T) {
	for _, lifecycle := range []string{"left", "revoked", "detached"} {
		t.Run(lifecycle, func(t *testing.T) {
			ctx := context.Background()
			s, f := newStore(t), newRemote(t)
			c, auth, options := f.join(t, s)
			original, _ := c.read(ctx)
			if lifecycle == "left" {
				if err := c.Relay(ctx, auth, "unbind", nil, nil); err != nil {
					t.Fatal(err)
				}
			} else if lifecycle == "detached" {
				if err := c.Detach(ctx, auth); err != nil {
					t.Fatal(err)
				}
			}
			retireAndAllowReplacement(f)
			candidate := replacementOptions(f, options)
			if lifecycle == "revoked" {
				candidate.Runtime = model.RuntimeCodex
			}
			client, joined, err := s.Join(ctx, candidate)
			if err != nil || joined.Status != "accepted" || joined.PreviousBindID != original.BindID || joined.Binding.BindID != candidate.BindID || joined.Binding.Generation != 8 || joined.Binding.Runtime != candidate.Runtime {
				t.Fatalf("explicit replacement failed: %+v %v", joined, err)
			}
			current, _ := client.read(ctx)
			if current.Identity == original.Identity || current.RequestID == original.RequestID || current.PreviousBindID != original.BindID || len(current.Deliveries) != 0 {
				t.Fatal("fresh owner admission reused retired authority or lost archive link")
			}
			archive, err := readRecord(filepath.Join(c.dir, "retired", original.BindID), c.id)
			if err != nil || archive.Identity != original.Identity || archive.BindID != original.BindID || archive.RequestID != original.RequestID || archive.Room.Generation != 7 {
				t.Fatal("replacement did not retain complete original private binding")
			}
			metadata, err := client.Metadata(ctx)
			if err != nil || metadata.PreviousBindID != original.BindID || metadata.Invite.Endpoint != original.Invite.Endpoint || metadata.Workspace != original.Workspace {
				t.Fatal("replacement lost workspace promotion recovery metadata")
			}
			before := len(f.actions())
			if _, again, err := s.Join(ctx, candidate); err != nil || again.Receipt != joined.Receipt {
				t.Fatalf("replacement retry rotated enrollment: %+v %v", again, err)
			}
			if got := strings.Join(f.actions()[before:], ","); got != "join-status" {
				t.Fatalf("replacement retry requested new approval: %s", got)
			}
			if err := c.Relay(ctx, auth, "summary", nil, nil); !errors.Is(err, relay.ErrAuth) {
				t.Fatalf("old workspace capability acquired new membership: %v", err)
			}
			if got := strings.Join(f.actions()[before:], ","); got != "join-status" {
				t.Fatal("retired local auth reached replacement host scope")
			}
		})
	}
}

func TestReplacementPendingIdentityAndKeyRecoverArchiveWriteFailure(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, _, options := f.join(t, s)
	original, _ := c.read(ctx)
	retireAndAllowReplacement(f)
	candidate := replacementOptions(f, options)
	archive, err := c.archiveDirectory(original.BindID)
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(archive, "client.json")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	client, _, err := s.Join(ctx, candidate)
	if err == nil || client == nil {
		t.Fatal("archive failure was treated as a completed cutover or lost client handle")
	}
	current, _ := c.read(ctx)
	if !sameLocalAssociation(current, original) {
		t.Fatal("archive failure overwrote original client")
	}
	var plan replacementPlan
	if err := privatefile.ReadJSON(filepath.Join(c.dir, "replacement.json"), 2<<20, &plan); err != nil {
		t.Fatal(err)
	}
	if err := s.identities.Check(ctx, reservation(plan.Candidate)); err != nil {
		t.Fatal("ambiguous cutover did not retain exact candidate reservation")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	client, joined, err := s.Join(ctx, candidate)
	if err != nil || joined.Status != "accepted" {
		t.Fatalf("reserved cutover did not recover: %+v %v", joined, err)
	}
	current, _ = client.read(ctx)
	if current.Identity != plan.Candidate.Identity || current.RequestID != plan.Candidate.RequestID {
		t.Fatal("cutover retry rotated staged private identity")
	}
	if err := s.identities.Check(ctx, reservation(original)); !errors.Is(err, nativeidentity.ErrUnowned) {
		t.Fatal("successful cutover did not release old session reservation")
	}
}

func TestReplacementLostJoinResponseLeavesDiscoverableExactCandidate(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, _, options := f.join(t, s)
	original, _ := c.read(ctx)
	retireAndAllowReplacement(f)
	f.mu.Lock()
	f.lostJoin = true
	f.mu.Unlock()
	candidate := replacementOptions(f, options)
	client, _, err := s.Join(ctx, candidate)
	if !errors.Is(err, ErrTransportUnavailable) || client == nil {
		t.Fatalf("installed uncertain candidate was not returned: %v", err)
	}
	current, _ := client.read(ctx)
	if current.Status != "pending" || current.BindID != candidate.BindID || current.PreviousBindID != original.BindID {
		t.Fatal("lost join response reverted installed local replacement")
	}
	client, joined, err := s.Join(ctx, candidate)
	if err != nil || joined.Status != "accepted" {
		t.Fatalf("replacement response loss did not recover: %+v %v", joined, err)
	}
	accepted, _ := client.read(ctx)
	if accepted.Identity != current.Identity || accepted.RequestID != current.RequestID {
		t.Fatal("lost response rotated replacement key/request")
	}
}

func TestRejectedReplacementIdentityDoesNotPoisonFreshCandidate(t *testing.T) {
	ctx := context.Background()
	s, f, occupied := newStore(t), newRemote(t), newRemote(t)
	c, _, options := f.join(t, s)
	otherOptions := occupied.options(t)
	otherOptions.SessionID, otherOptions.BindID = "already-used-session", "already-used-binding"
	other, _, err := s.Join(ctx, otherOptions)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := c.read(ctx)
	retireAndAllowReplacement(f)
	candidate := replacementOptions(f, options)
	candidate.SessionID = otherOptions.SessionID
	if _, _, err := s.Join(ctx, candidate); !errors.Is(err, nativeidentity.ErrOwned) {
		t.Fatalf("conflicting replacement identity was admitted: %v", err)
	}
	current, _ := c.read(ctx)
	if !sameLocalAssociation(current, original) {
		t.Fatal("ownership rejection changed original association")
	}
	candidate.SessionID, candidate.BindID = "available-new-session", "available-new-binding"
	if _, joined, err := s.Join(ctx, candidate); err != nil || joined.Status != "accepted" {
		t.Fatalf("rejected candidate poisoned a fresh replacement: %+v %v", joined, err)
	}
	occupiedRecord, _ := other.read(ctx)
	if err := s.identities.Check(ctx, reservation(occupiedRecord)); err != nil {
		t.Fatal("fresh replacement released an unrelated Room's session")
	}
}

func TestStaleAdmissionAndLeaveResponsesCannotMutateReplacement(t *testing.T) {
	for _, action := range []string{"admission", "leave"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			s, f := newStore(t), newRemote(t)
			c, auth, options := f.join(t, s)
			started, releaseResponse := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(releaseResponse) })
			t.Cleanup(release)
			var first atomic.Bool
			if action == "admission" {
				f.mu.Lock()
				f.admissionHook = func(w http.ResponseWriter, r *http.Request, response lanshare.JoinResponse) bool {
					if filepath.Base(r.URL.Path) != "join-status" || first.Swap(true) {
						return false
					}
					close(started)
					<-releaseResponse
					reply(w, response)
					return true
				}
				f.mu.Unlock()
			} else {
				f.setHandler(func(w http.ResponseWriter, _ *http.Request, op string, _ []byte) {
					if op != "unbind" {
						t.Errorf("unexpected stale operation %s", op)
						w.WriteHeader(404)
						return
					}
					close(started)
					<-releaseResponse
					reply(w, map[string]bool{"unbound": true})
				})
			}
			finished := make(chan error, 1)
			go func() {
				if action == "admission" {
					_, err := c.Resume(ctx)
					finished <- err
				} else {
					finished <- c.Relay(ctx, auth, "unbind", nil, nil)
				}
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("old operation did not enter remote boundary")
			}
			retireAndAllowReplacement(f)
			candidate := replacementOptions(f, options)
			_, joined, err := s.Join(ctx, candidate)
			if err != nil || joined.Status != "accepted" {
				t.Fatalf("replacement while old response in flight: %+v %v", joined, err)
			}
			release()
			if err := <-finished; err == nil {
				t.Fatal("stale old response was treated as current binding completion")
			}
			current, err := c.read(ctx)
			if err != nil || current.Status != "accepted" || current.BindID != candidate.BindID || current.Room.Generation != 8 {
				t.Fatal("stale response retired or demoted the new membership")
			}
		})
	}
}

func TestPendingOwnerDetachIsLocalOnlyAndDisablesObservation(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	f.mu.Lock()
	f.status = "pending"
	f.mu.Unlock()
	c, result, err := s.Join(ctx, f.options(t))
	if err != nil || result.Status != "pending" {
		t.Fatal(err)
	}
	r, _ := c.read(ctx)
	before := len(f.actions())
	if err := c.Owner(ctx, "detach", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Maintenance(ctx); !errors.Is(err, ErrInactive) {
		t.Fatal("abandoned pending client remained observable")
	}
	if err := s.identities.Check(ctx, reservation(r)); !errors.Is(err, nativeidentity.ErrUnowned) {
		t.Fatal("pending abandonment retained native session")
	}
	if len(f.actions()) != before {
		t.Fatal("local pending detach contacted host or asserted host cancellation")
	}
}
