package main

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
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
