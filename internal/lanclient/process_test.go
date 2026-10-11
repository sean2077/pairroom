package lanclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/privatelock"
	"github.com/sean2077/pairroom/internal/relay"
)

// The child uses the real exported client and on-disk record. Its lock attempt
// must expire while the parent owns the same stable directory, on Unix and
// Windows alike; atomic JSON replacement must not create a second lock domain.
func TestLANClientProcess(t *testing.T) {
	if os.Getenv("PAIRROOM_TEST_LAN_CHILD") != "1" {
		return
	}
	s, err := OpenAt(os.Getenv("PAIRROOM_TEST_LAN_STORE"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.Get(context.Background(), os.Getenv("PAIRROOM_TEST_LAN_ID"))
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := c.Metadata(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("ready")
	if !scanner.Scan() || scanner.Text() != "check" {
		t.Fatal("missing lock check barrier")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err = c.Metadata(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("independent process bypassed held directory lock: %v", err)
	}
	fmt.Println("locked")
	if !scanner.Scan() || scanner.Text() != "continue" {
		t.Fatal("missing lock release barrier")
	}
	auth := relay.Auth{Slot: metadata.Slot, Generation: metadata.Generation, BindID: metadata.BindID, SessionID: metadata.SessionID, Secret: fixtureSecret}
	if err := c.Relay(context.Background(), auth, "confirm", map[string]string{"session_id": metadata.SessionID, "transcript_path": filepath.Join(metadata.Workspace, "private-transcript.jsonl")}, nil); err != nil {
		t.Fatal(err)
	}
	fmt.Println("done")
}

func TestIndependentCLIProcessLocksAndReloadsAtomicRecord(t *testing.T) {
	s, f := newStore(t), newRemote(t)
	c, _, options := f.join(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLANClientProcess$")
	command.Env = append(os.Environ(), "PAIRROOM_TEST_LAN_CHILD=1", "PAIRROOM_TEST_LAN_STORE="+s.Root(), "PAIRROOM_TEST_LAN_ID="+c.id)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill() }()
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		_ = command.Wait()
		t.Fatalf("client process did not initialize: %s", stderr.String())
	}
	unlock, err := privatelock.Lock(ctx, c.dir)
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	if _, err := fmt.Fprintln(stdin, "check"); err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() || scanner.Text() != "locked" {
		unlock()
		locked = false
		_ = command.Wait()
		t.Fatalf("child did not respect record lock: %s", stderr.String())
	}
	r, err := readRecord(c.dir, c.id)
	if err != nil {
		t.Fatal(err)
	}
	r.ParkEnabled = false
	if err := privatefile.WriteJSON(filepath.Join(c.dir, "client.json"), r); err != nil {
		t.Fatal(err)
	}
	unlock()
	locked = false
	if _, err := fmt.Fprintln(stdin, "continue"); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if !scanner.Scan() || scanner.Text() != "done" {
		_ = command.Wait()
		t.Fatalf("child could not resume with new record: %s", stderr.String())
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("child failed: %v %s", err, stderr.String())
	}
	after, err := c.read(ctx)
	if err != nil || after.ParkEnabled || after.TranscriptPath != filepath.Join(options.Workspace, "private-transcript.jsonl") || after.LastActivity.IsZero() {
		t.Fatal("child overwrote the parent's atomic record update with cached state")
	}
}

func TestResumeRepairsInterruptedNativeIdentityPromotion(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	f.mu.Lock()
	f.status = "pending"
	f.mu.Unlock()
	options := f.options(t)
	c, result, err := s.Join(ctx, options)
	if err != nil || result.Status != "pending" {
		t.Fatalf("pending join: %+v %v", result, err)
	}
	r, err := c.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.status = "accepted"
	f.mu.Unlock()
	var admission lanshare.JoinResponse
	if err := c.call(ctx, r, "join-status", lanshare.JoinStatusRequest{RequestID: r.RequestID}, &admission); err != nil {
		t.Fatal(err)
	}
	// Reproduce the durable state after client.json promotion and before the
	// separate pending reservation is upgraded. Neither key nor BindID changes.
	r.Status, r.Room, r.Receipt = admission.Status, admission.Room, admission.Receipt
	if err := privatefile.WriteJSON(filepath.Join(c.dir, "client.json"), r); err != nil {
		t.Fatal(err)
	}
	admitted := reservation(r)
	auth := relay.Auth{Slot: r.Room.Slot, Generation: r.Room.Generation, BindID: r.BindID, SessionID: r.SessionID, Secret: fixtureSecret}
	before := len(f.actions())
	if err := c.Relay(ctx, auth, "summary", nil, nil); !errors.Is(err, nativeidentity.ErrUnowned) {
		t.Fatalf("partially promoted identity authorized a member operation: %v", err)
	}
	if len(f.actions()) != before {
		t.Fatal("unpromoted identity reached remote member operation")
	}
	result, err = c.Resume(ctx)
	if err != nil || result.Status != "accepted" || result.Binding.BindID != auth.BindID {
		t.Fatalf("same association did not repair promotion: %+v %v", result, err)
	}
	if err := s.identities.Check(ctx, admitted); err != nil {
		t.Fatal(err)
	}
	if err := c.Relay(ctx, auth, "summary", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestStoreCapturesIdentityRootAndDoesNotFollowAmbientConfigChanges(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, _ := f.join(t, s)
	otherUser := localDir(t)
	t.Setenv("XDG_CONFIG_HOME", otherUser)
	t.Setenv("APPDATA", otherUser)
	t.Setenv("HOME", otherUser)
	if err := c.Relay(ctx, auth, "summary", nil, nil); err != nil {
		t.Fatalf("captured client followed an ambient config change: %v", err)
	}
	if _, err := os.Stat(filepath.Join(otherUser, "pairroom")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("existing client created a second user's identity store")
	}
}
