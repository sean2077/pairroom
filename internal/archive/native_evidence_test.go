package archive

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/store"
)

func TestNativeEvidenceBackupRestoresExactInertContent(t *testing.T) {
	dataDir, _ := makeValidDataDir(t)
	media, err := attachment.Open(dataDir, "")
	if err != nil {
		t.Fatal(err)
	}
	const script = "#!/bin/sh\nprintf 'repro evidence only\\n'\n"
	evidence, err := media.SaveEvidence("repro.sh", strings.NewReader(script), "native-relay")
	if err != nil {
		t.Fatal(err)
	}
	log, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	event, err := model.NewEvent("room-test", "native.message.updated", model.ActorSlot1, relay.Message{
		ID: "native-evidence", From: model.ActorSlot1, To: model.ActorSlot2, State: "queued",
		Text: "Review the evidence", Attachments: []model.Attachment{evidence}, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(&event); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "native-evidence.tar.gz")
	if _, err := Backup(dataDir, backup); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	report, err := Restore(backup, restored, false)
	if err != nil || !report.OK || report.ReferencedAttachments != 2 {
		t.Fatalf("Native evidence round trip: report=%+v err=%v", report, err)
	}
	restoredMedia, err := attachment.Open(restored, "")
	if err != nil {
		t.Fatal(err)
	}
	got, path, err := restoredMedia.Resolve(evidence.ID)
	if err != nil || got != evidence || filepath.Ext(path) != ".data" {
		t.Fatalf("evidence identity changed during restore: %+v %v", got, err)
	}
	bytes, err := os.ReadFile(path)
	if err != nil || string(bytes) != script {
		t.Fatal("restore changed the shared evidence bytes")
	}
	info, err := os.Stat(path)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0o177 != 0 {
		t.Fatal("restored script evidence became executable or broadly readable")
	}
	for _, name := range []string{evidence.ID + ".sh", evidence.ID + ".exe", evidence.ID + ".data:stream", evidence.ID + ".data/child"} {
		if restorableRoomPath("attachments/"+name) == nil {
			t.Fatalf("evidence support broadened the restore file set: %s", name)
		}
	}
}
