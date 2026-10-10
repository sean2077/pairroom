package nativeidentity

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenAt(filepath.Join(t.TempDir(), "identities"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func testClaim(owner string) Claim {
	return Claim{Runtime: model.RuntimeCodex, SessionID: "native-session", Association: Remote(owner, "room"), BindID: "operation-one"}
}

func TestPendingAdmissionAndExactRelease(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	pending := testClaim("host-one")
	if err := s.Reserve(ctx, pending); err != nil {
		t.Fatal(err)
	}
	other := testClaim("host-two")
	if err := s.Reserve(ctx, other); !errors.Is(err, ErrOwned) {
		t.Fatalf("pending did not reserve session: %v", err)
	}
	admitted := pending
	admitted.Generation = 7
	if err := s.Reserve(ctx, admitted); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(ctx, admitted); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(ctx, pending); !errors.Is(err, ErrUnowned) {
		t.Fatal("pending credentials accepted as admitted")
	}
	if err := s.Release(ctx, pending); !errors.Is(err, ErrOwned) {
		t.Fatal("stale pending release cleared admitted membership")
	}
	if err := s.Reserve(ctx, pending); !errors.Is(err, ErrOwned) {
		t.Fatal("admitted generation downgraded")
	}
	if err := s.Release(ctx, admitted); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, admitted); !errors.Is(err, ErrOwned) {
		t.Fatal("old owner release cleared newer association")
	}
}

func TestCommitRetainsAmbiguousOwnershipAndReplacesExactly(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	previous := testClaim("host")
	previous.Association = Hosted(t.TempDir(), "room-one", model.ActorSlot1)
	previous.Generation = 1
	if err := s.Reserve(ctx, previous); err != nil {
		t.Fatal(err)
	}
	next := previous
	next.SessionID, next.BindID, next.Generation = "replacement-session", "replacement-binding", 2
	lost := errors.New("append outcome uncertain")
	if err := s.Commit(ctx, &previous, &next, func() error { return lost }); !errors.Is(err, lost) {
		t.Fatal(err)
	}
	if err := s.Check(ctx, previous); err != nil {
		t.Fatal("ambiguous replace released its old session")
	}
	if err := s.Check(ctx, next); err != nil {
		t.Fatal("ambiguous replace failed to reserve new session")
	}
	if err := s.Commit(ctx, &previous, &next, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(ctx, previous); !errors.Is(err, ErrUnowned) {
		t.Fatal("successful replace did not release old identity")
	}
	if err := s.Check(ctx, next); err != nil {
		t.Fatal(err)
	}
}

func TestCompetingProcessReservationsHaveOneWinner(t *testing.T) {
	if root := os.Getenv("PAIRROOM_TEST_IDENTITY_CHILD_ROOT"); root != "" {
		s, err := OpenAt(root)
		if err != nil {
			os.Exit(2)
		}
		if err := s.Reserve(context.Background(), testClaim(os.Getenv("PAIRROOM_TEST_IDENTITY_CHILD_OWNER"))); err != nil {
			if errors.Is(err, ErrOwned) {
				os.Exit(3)
			}
			os.Exit(2)
		}
		return
	}
	s := testStore(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, owner := range []string{"left", "right"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCompetingProcessReservationsHaveOneWinner$")
			cmd.Env = append(os.Environ(), "PAIRROOM_TEST_IDENTITY_CHILD_ROOT="+s.root, "PAIRROOM_TEST_IDENTITY_CHILD_OWNER="+owner)
			results <- cmd.Run()
		}()
	}
	wg.Wait()
	close(results)
	won, lost := 0, 0
	for err := range results {
		if err == nil {
			won++
			continue
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 3 {
			lost++
			continue
		}
		t.Fatalf("child reservation failed unexpectedly: %v", err)
	}
	if won != 1 || lost != 1 {
		t.Fatalf("winners %d; conflicts %d", won, lost)
	}
}

func TestMalformedReservationIsNeverRepaired(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	c := testClaim("host")
	if err := s.Reserve(ctx, c); err != nil {
		t.Fatal(err)
	}
	dir, err := s.directory(c, false)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "claim.json")
	if err := privatefile.WriteJSON(path, record{Schema: 2, Claim: c}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []func() error{func() error { return s.Reserve(ctx, c) }, func() error { return s.Check(ctx, c) }, func() error { return s.Release(ctx, c) }} {
		if err := operation(); err == nil {
			t.Fatal("future reservation was trusted")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("future reservation was rewritten")
	}
}

func TestExplicitPendingReplacementAndStaleReleasePreserveOtherOwners(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	old := testClaim("host")
	old.Generation = 7
	if err := s.Reserve(ctx, old); err != nil {
		t.Fatal(err)
	}
	next := old
	next.BindID, next.Generation = "replacement", 0
	called := false
	if err := s.Commit(ctx, &old, &next, func() error { called = true; return nil }); !errors.Is(err, ErrOwned) || called {
		t.Fatal("ordinary replacement regressed the admission generation")
	}
	if err := s.ReplacePending(ctx, &old, next, func() error { called = true; return nil }); err != nil || !called {
		t.Fatal(err)
	}
	if err := s.Check(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, old); !errors.Is(err, ErrOwned) {
		t.Fatal("old admitted operation released fresh pending request")
	}
	if err := s.Release(ctx, next); err != nil {
		t.Fatal(err)
	}
	other := testClaim("different-host")
	if err := s.Reserve(ctx, other); err != nil {
		t.Fatal(err)
	}
	fresh := next
	fresh.SessionID, fresh.BindID = "new-local-session", "fresh-local-binding"
	if err := s.ReplacePending(ctx, nil, fresh, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(ctx, other); err != nil {
		t.Fatal("replacement of a detached record touched its old session's new owner")
	}
}

func TestReadOnlyPreflightAndArchivedRetentionKeepExactOperations(t *testing.T) {
	s, ctx := testStore(t), context.Background()
	if err := s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.root); !os.IsNotExist(err) {
		t.Fatal("read-only preflight created identity directory")
	}
	c := testClaim("host")
	c.Association, c.Generation = Hosted(t.TempDir(), "archived-room", model.ActorSlot1), 8
	if err := s.Reserve(ctx, c); err != nil {
		t.Fatal(err)
	}
	probe := c
	probe.BindID, probe.Generation = "archived-checkpoint", 0
	retained, ok, err := s.Retained(ctx, probe)
	if err != nil || !ok || retained != c {
		t.Fatal("checkpoint retention invented a binding instead of preserving the exact operation")
	}
	if err := s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	probe.Association = Remote("other", "room")
	if _, _, err := s.Retained(ctx, probe); !errors.Is(err, ErrOwned) {
		t.Fatal("archive inherited another association's ownership")
	}
}
