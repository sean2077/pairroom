package server

import (
	"io/fs"
	"strings"
	"testing"
)

func TestRoomShellBatchesTransientRuntimeRenderingAndExposesExit(t *testing.T) {
	index, err := fs.ReadFile(embeddedAssets, "assets/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(index)
	shellPosition := strings.Index(html, `src="room-shell.js"`)
	activityPosition := strings.Index(html, `src="activity-view.js"`)
	appPosition := strings.Index(html, `src="app.js"`)
	if shellPosition < 0 || appPosition < 0 || shellPosition > appPosition {
		t.Fatalf("Room shell must load before app.js: shell=%d app=%d", shellPosition, appPosition)
	}
	if activityPosition < 0 || activityPosition > appPosition {
		t.Fatal("keyed Activity renderer must load before app.js")
	}
	if !strings.Contains(html, `id="leave-room"`) || !strings.Contains(html, `data-i18n="ui.leaveRoom"`) {
		t.Fatal("Room header does not expose an explicit exit control")
	}

	script, err := fs.ReadFile(embeddedAssets, "assets/room-shell.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(script)
	if strings.Contains(source, "captureActivityState") || strings.Contains(source, "restoreActivityState") {
		t.Fatal("transport must not own Inspector DOM state")
	}
	for _, marker := range []string{
		"TRANSIENT_RENDER_INTERVAL_MS",
		"command.output",
		"diff.updated",
		"usage.updated",
		"flushTransientEvents",
		"window.EventSource = PairRoomEventSource",
		"window.history.back()",
		"window.close()",
		"pairroom-surface",
		"close-tab",
	} {
		if !strings.Contains(source, marker) {
			t.Fatalf("Room shell omitted %q", marker)
		}
	}
}
