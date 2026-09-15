package relayclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/relay"
)

// A separate process exercises kernel lock ownership (including Windows mutex
// thread ownership and process-death release), not a fake in-memory mutex.
func TestCollectorHelperProcess(t *testing.T) {
	dir := os.Getenv("PAIRROOM_TEST_COLLECTOR_DIR")
	if dir == "" {
		return
	}
	release, err := acquireCollector(context.Background(), dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println("collector-ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	release()
	os.Exit(0)
}

func holdCollectorProcess(t *testing.T, dir string) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCollectorHelperProcess$")
	cmd.Env = append(os.Environ(), "PAIRROOM_TEST_COLLECTOR_DIR="+dir)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Kill()
			_ = input.Close()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(stop)
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(output).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "collector-ready\n" {
			t.Fatalf("collector helper failed: %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("collector helper did not start")
	}
	return stop
}

func TestCollectorRejectsDuplicateBeforeExchangePublishes(t *testing.T) {
	isolateCaller(t)
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	stop := holdCollectorProcess(t, filepath.Dir(f.statePath))
	for _, action := range []string{"wait", "exchange"} {
		err := f.run(context.Background(), action, strings.NewReader("proposal"), io.Discard, io.Discard, "--id", "review-1")
		if !errors.Is(err, errCollectorBusy) || f.count("send") != 0 || f.count("wait") != 0 {
			t.Fatalf("duplicate %s published or collected: %v", action, err)
		}
	}
	// Ordinary send and the state lock remain usable while a receiver waits.
	if err := f.run(context.Background(), "send", strings.NewReader("final result"), io.Discard, io.Discard, "--id", "final"); err != nil {
		t.Fatalf("collector blocked an independent send: %v", err)
	}
	stop() // crash, rather than graceful unlock
	if err := f.run(context.Background(), "wait", strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatalf("dead process left a stale collector lock: %v", err)
	}
	if f.count("send") != 1 || f.count("wait") != 1 || f.count("ack") != 1 {
		t.Fatal("collection was duplicated")
	}
}

func TestHookPublishesButDoesNotStealForegroundInput(t *testing.T) {
	isolateCaller(t)
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	c, err := load(filepath.Dir(f.statePath))
	if err != nil {
		t.Fatal(err)
	}
	var reports, waits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Relay private-long-lived-secret" {
			t.Error("missing relay identity")
		}
		switch filepath.Base(r.URL.Path) {
		case "inspect":
			_ = json.NewEncoder(w).Encode(relay.Binding{BindID: "binding", Generation: 1, SessionID: "session"})
		case "report":
			reports.Add(1)
			var req struct {
				Seq uint64 `json:"report_seq"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(relay.Publication{BindID: "binding", Generation: 1, ReportSeq: req.Seq})
		case "wait":
			waits.Add(1)
			t.Error("hook stole foreground collector")
			_, _ = io.WriteString(w, `{"claim":null}`)
		default:
			t.Errorf("unexpected hook operation: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	if err := relay.AtomicJSON(c.State.EndpointPath, relay.Endpoint{URL: srv.URL, Token: "management-secret-not-output"}); err != nil {
		t.Fatal(err)
	}
	holdCollectorProcess(t, c.Dir)
	text := "@codex complete hook reply"
	data, _ := json.Marshal(HookInput{Event: "Stop", SessionID: "session", CWD: f.args[1], LastAssistantMessage: &text})
	var out bytes.Buffer
	if err := Run(context.Background(), []string{"hook", "--runtime", "claude"}, bytes.NewReader(data), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if reports.Load() != 1 || waits.Load() != 0 || strings.TrimSpace(out.String()) != "{}" {
		t.Fatalf("hook publication/collection were coupled: reports=%d waits=%d out=%s", reports.Load(), waits.Load(), out.String())
	}
}

func TestForegroundShortCallerDeadlineCanReceiveQueuedInput(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	c, err := load(filepath.Dir(f.statePath))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out bytes.Buffer
	got, err := deliverOnce(ctx, c, false, 30, &out)
	if err != nil || !got || f.count("ack") != 1 {
		t.Fatalf("foreground inherited hook reserve: %v", err)
	}
}

func TestCollectorAlreadyCancelledDoesNotAcquire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acquireCollector(ctx, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
