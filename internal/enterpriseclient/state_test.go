package enterpriseclient

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPrivateStateLockSubprocess(t *testing.T) {
	mode := os.Getenv("PAIRROOM_ENTERPRISE_CLIENT_LOCK_CHILD")
	if mode == "" {
		return
	}
	client, e := New(Config{Origin: "https://locked.example.test", DeploymentID: "dep-test", Directory: os.Getenv("PAIRROOM_ENTERPRISE_CLIENT_TEST_DIR")})
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, e = client.Logout(ctx)
	if mode == "blocked" {
		if !errors.Is(e, context.DeadlineExceeded) {
			t.Fatalf("another process bypassed private state lock: %v", e)
		}
	} else if e != nil {
		t.Fatal(e)
	}
}
func TestPrivateStoreSerializesAcrossProcesses(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	client, e := New(Config{Origin: "https://locked.example.test", DeploymentID: "dep-test", Directory: directory})
	if e != nil {
		t.Fatal(e)
	}
	defer client.Close()
	if _, e = client.Logout(context.Background()); e != nil {
		t.Fatal(e)
	}
	before, _ := client.Status()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	run := func(mode string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestPrivateStateLockSubprocess$", "-test.count=1")
		cmd.Env = append(os.Environ(), "PAIRROOM_ENTERPRISE_CLIENT_LOCK_CHILD="+mode, "PAIRROOM_ENTERPRISE_CLIENT_TEST_DIR="+directory)
		data, e := cmd.CombinedOutput()
		if e != nil {
			t.Logf("private subprocess output: %s", data)
		}
		return e
	}
	unlock, e := client.lock(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	childErr := run("blocked")
	unlock()
	if childErr != nil {
		t.Fatal(childErr)
	}
	after, _ := client.Status()
	if after.Revision != before.Revision {
		t.Fatal("blocked process changed state")
	}
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() { defer wait.Done(); results <- run("update") }()
	}
	wait.Wait()
	close(results)
	for e := range results {
		if e != nil {
			t.Fatal(e)
		}
	}
	after, _ = client.Status()
	if after.Revision != before.Revision+2 {
		t.Fatalf("concurrent processes lost a state update: %d -> %d", before.Revision, after.Revision)
	}
}
func TestStatusValidatesLiveServiceTupleWithoutDisclosingCredential(t *testing.T) {
	f := newWireFixture(t)
	f.login(t)
	status, e := f.client.CheckStatus(context.Background())
	if e != nil || status.Scope != "server" || status.State != "authenticated" || status.UserID != "user-test" {
		t.Fatalf("server status: %+v %v", status, e)
	}
}
