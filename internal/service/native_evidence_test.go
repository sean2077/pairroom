package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestNativeCLIExplicitEvidenceReachesPeerAsInertLocalFile(t *testing.T) {
	f := nativeHTTP(t)
	from := f.bind(t, model.ActorSlot1)
	to := f.bind(t, model.ActorSlot2)
	path := filepath.Join(f.project.Root, "repro.sh")
	body := "#!/bin/sh\nprintf 'bug reproduction evidence\\n'\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := f.runAs(t, model.RuntimeClaude, from.SessionID, []string{"send", "--room", f.room.ID, "--slot", "slot1", "--text", "Please inspect this repro", "--file", path}, nil)
	if err != nil {
		t.Fatalf("explicit evidence CLI upload: %v", err)
	}
	claim, err := f.native.engine.Claim(context.Background(), to, false)
	if err != nil || claim == nil {
		t.Fatalf("peer claim: %+v, %v", claim, err)
	}
	if !strings.Contains(claim.Envelope, `name: "repro.sh"; type: "text/plain"`) || strings.Contains(claim.Envelope, body) {
		t.Fatalf("evidence did not remain a separate local artifact: %s", claim.Envelope)
	}
	if !strings.Contains(claim.Envelope, f.native.media.Root()) {
		t.Fatal("receiving local harness was not given its own verified file path")
	}
}
