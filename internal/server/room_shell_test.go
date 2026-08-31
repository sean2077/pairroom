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
	shellPosition := strings.Index(html, `<script src="/room-shell.js" defer></script>`)
	appPosition := strings.Index(html, `<script src="/app.js" defer></script>`)
	if shellPosition < 0 || appPosition < 0 || shellPosition > appPosition {
		t.Fatalf("Room shell must load before app.js: shell=%d app=%d", shellPosition, appPosition)
	}
	if !strings.Contains(html, `id="leave-room"`) || !strings.Contains(html, `退出 Room`) {
		t.Fatal("Room header does not expose an explicit exit control")
	}

	script, err := fs.ReadFile(embeddedAssets, "assets/room-shell.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(script)
	for _, marker := range []string{
		"TRANSIENT_RENDER_INTERVAL_MS",
		"command.output",
		"diff.updated",
		"usage.updated",
		"flushTransientEvents",
		"captureActivityState",
		"window.EventSource = PairRoomEventSource",
		"window.history.back()",
		"window.close()",
	} {
		if !strings.Contains(source, marker) {
			t.Fatalf("Room shell omitted %q", marker)
		}
	}
}
