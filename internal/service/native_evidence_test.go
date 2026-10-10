package service

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestNativeCLIExplicitEvidenceReachesPeerAsInertLocalFile(t *testing.T) {
	f := nativeHTTP(t)
	t.Cleanup(func() {
		// This test inspects a claim without reporting collector stdout. Close
		// its runtime explicitly instead of acknowledging undelivered output.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := f.native.Close(ctx); err != nil {
			t.Errorf("close evidence test runtime: %v", err)
		}
	})
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
	history, err := f.native.engine.History(relay.HistoryQuery{ID: claim.ID})
	if err != nil || len(history.Messages) != 1 || len(history.Messages[0].Attachments) != 1 {
		t.Fatal("evidence claim lost its canonical attachment")
	}
	metadata, localPath, err := f.native.media.Resolve(history.Messages[0].Attachments[0].ID)
	if err != nil || metadata.Kind != "file" || filepath.Ext(localPath) != ".data" {
		t.Fatal("evidence was not stored as an inert local artifact")
	}
	if !strings.Contains(claim.Envelope, "; path: "+strconv.Quote(localPath)) || strings.Contains(claim.Envelope, strconv.Quote(path)) {
		t.Fatal("receiving local harness was not given its own verified file path")
	}
	actual, err := os.ReadFile(localPath)
	if err != nil || string(actual) != body {
		t.Fatal("receiving evidence bytes changed")
	}
}
