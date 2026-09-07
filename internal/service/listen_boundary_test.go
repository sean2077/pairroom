package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedRuntimeRejectsNonNumericLoopbackBeforeOpeningStore(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	project, ok := registry.Project(rooms[0].ProjectID)
	if !ok {
		t.Fatal("test Project missing")
	}
	for _, host := range []string{"localhost", "LOCALHOST", "pairroom.local", "0.0.0.0", "::", "192.0.2.1", "127.0.0.1:1234"} {
		t.Run(host, func(t *testing.T) {
			durable := rooms[0]
			durable.DataDir = filepath.Join(t.TempDir(), "must-not-be-created")
			runtime, err := startEmbeddedRuntime(context.Background(), registry, project, durable, EmbeddedRuntimeConfig{Mock: true, ListenHost: host})
			if runtime != nil {
				_ = runtime.Close(context.Background())
			}
			if err == nil || !strings.Contains(err.Error(), "numeric loopback") {
				t.Errorf("invalid listener did not fail at its boundary: %v", err)
			}
			if _, err := os.Stat(durable.DataDir); !os.IsNotExist(err) {
				t.Errorf("invalid listener touched Room storage: %v", err)
			}
		})
	}
}
