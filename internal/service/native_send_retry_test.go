package service

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The documented recovery for an uncertain `relay send --attach` is rerunning
// the same command with the same --id. That re-uploads the image, so it must
// still resolve to the original publication rather than a payload conflict.
func TestNativeCLISameIDAttachRetryReturnsOriginal(t *testing.T) {
	f := nativeHTTP(t)
	f.bind(t, "slot1")
	f.bind(t, "slot2")
	image, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	path := filepath.Join(t.TempDir(), "diagram.png")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	send := func(text string) (string, error) {
		t.Helper()
		out, err := f.run(t, []string{"send", "--room", f.room.ID, "--slot", "1", "--id", "attach-once", "--text", text, "--attach", path}, nil)
		if err != nil {
			return "", err
		}
		var receipt struct {
			Published string `json:"published"`
		}
		if err := json.Unmarshal(out, &receipt); err != nil || receipt.Published == "" {
			t.Fatalf("receipt: %s %v", out, err)
		}
		return receipt.Published, nil
	}
	first, err := send("see image")
	if err != nil {
		t.Fatal(err)
	}
	seq := f.native.engine.Sequence()
	again, err := send("see image")
	if err != nil || again != first {
		t.Fatalf("same-ID attach retry: %q != %q: %v", again, first, err)
	}
	if f.native.engine.Sequence() != seq {
		t.Fatal("same-ID retry appended an event")
	}
	// A real conflict is a settled answer: not "uncertain", no retry advice.
	_, err = send("changed body")
	if err == nil || !strings.Contains(err.Error(), "already used for a different message") || strings.Contains(err.Error(), "uncertain") || strings.Contains(err.Error(), "retry with the SAME") {
		t.Fatalf("conflict wording: %v", err)
	}
	if f.native.engine.Sequence() != seq || len(f.native.engine.Snapshot().Messages) != 1 {
		t.Fatal("conflicting send published")
	}
}
