package main

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/desktop/internal/host"
	"github.com/wailsapp/wails/v3/pkg/application"
)

func TestEmbeddedStartupAssetsAreServedFromWebviewRoot(t *testing.T) {
	handler := application.BundledAssetFileServer(frontend)
	for _, path := range []string{"/", "/styles.css", "/wails/runtime.js"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("embedded startup asset %s status = %d, want 200", path, rec.Code)
		}
		if path == "/" {
			body, err := io.ReadAll(rec.Result().Body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `href="/styles.css"`) {
				t.Fatal("embedded startup page does not load its stylesheet from the webview root")
			}
			if !strings.Contains(string(body), `src="/wails/runtime.js"`) {
				t.Fatal("embedded startup page does not load the Wails runtime")
			}
		}
		if path == "/wails/runtime.js" {
			body, err := io.ReadAll(rec.Result().Body)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), `wails:runtime:ready`) {
				t.Fatal("embedded Wails runtime does not announce readiness")
			}
		}
	}
}

func TestDesktopWindowGateReplaysActionsAfterRuntimeReady(t *testing.T) {
	var got []int
	gate := &desktopWindowGate{}
	gate.submit(func() { got = append(got, 1) })
	gate.submit(func() { got = append(got, 2) })
	if len(got) != 0 {
		t.Fatalf("actions ran before runtime ready: %v", got)
	}
	gate.markReady()
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("replayed actions = %v, want [1 2]", got)
	}
	gate.submit(func() { got = append(got, 3) })
	if len(got) != 3 || got[2] != 3 {
		t.Fatalf("ready action = %v, want [1 2 3]", got)
	}
}

func TestDesktopWindowGateDoesNotStrandSubmissionAtDrainBoundary(t *testing.T) {
	gate := &desktopWindowGate{ready: true, draining: true}
	if _, ok := gate.nextAction(); ok {
		t.Fatal("empty gate returned an action")
	}

	called := false
	gate.submit(func() { called = true })
	if !called {
		t.Fatal("submission after drain boundary was not dispatched")
	}
}

func TestDesktopControllerCancelsStartupBeforeItPublishesAHost(t *testing.T) {
	controller := &desktopController{}
	started := make(chan struct{})
	ready := make(chan struct{})
	startCtx, startCancel := context.WithCancel(context.Background())
	controller.start(
		startCtx,
		startCancel,
		func(ctx context.Context) (*host.Host, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		func(*host.Host) { close(ready) },
		nil,
	)
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := controller.shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
		t.Fatal("startup published a host after shutdown")
	default:
	}
}

type fakeDrainHost struct {
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
	errs    []error
}

func (f *fakeDrainHost) Shutdown(ctx context.Context) error {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()
	if call == 1 && f.entered != nil {
		close(f.entered)
	}
	if call == 1 && f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if call <= len(f.errs) {
		return f.errs[call-1]
	}
	return nil
}

func (f *fakeDrainHost) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestDesktopControllerQuitWaitsForRestartDrain(t *testing.T) {
	controller := &desktopController{}
	fake := &fakeDrainHost{entered: make(chan struct{}), release: make(chan struct{})}
	drained := make(chan error, 1)
	controller.hostMu.Lock()
	controller.beginDrainLocked(fake, func(err error) { drained <- err })
	controller.hostMu.Unlock()
	<-fake.entered

	// A second bootstrap must not race the draining Service for its lock.
	started := false
	controller.start(context.Background(), nil,
		func(context.Context) (*host.Host, error) { started = true; return nil, nil }, nil, nil)
	if started {
		t.Fatal("startup ran while a restart drain was pending")
	}

	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if err := controller.shutdown(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown during drain = %v, want it to keep waiting", err)
	}

	close(fake.release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := controller.shutdown(ctx); err != nil {
		t.Fatalf("shutdown after drain = %v", err)
	}
	if err := <-drained; err != nil {
		t.Fatalf("drain callback = %v", err)
	}
	if got := fake.callCount(); got != 1 {
		t.Fatalf("Shutdown calls = %d, want 1 for a clean drain", got)
	}
}

func TestDesktopControllerQuitRetriesFailedRestartDrain(t *testing.T) {
	controller := &desktopController{}
	fake := &fakeDrainHost{errs: []error{errors.New("drain timed out")}}
	drained := make(chan error, 1)
	controller.hostMu.Lock()
	controller.beginDrainLocked(fake, func(err error) { drained <- err })
	controller.hostMu.Unlock()
	if err := <-drained; err == nil {
		t.Fatal("first drain unexpectedly succeeded")
	}

	controller.hostMu.Lock()
	pending := controller.drainPendingLocked()
	controller.hostMu.Unlock()
	if !pending {
		t.Fatal("failed drain was not retained for retry")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := controller.shutdown(ctx); err != nil {
		t.Fatalf("shutdown = %v, want the retried drain to succeed", err)
	}
	if got := fake.callCount(); got != 2 {
		t.Fatalf("Shutdown calls = %d, want the failed drain retried once", got)
	}
}
