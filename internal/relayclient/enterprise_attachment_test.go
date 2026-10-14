package relayclient

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
)

func TestEnterpriseCLIUploadsThenPublishesAndCachesBeforeStdoutACK(t *testing.T) {
	f := newEnterpriseCLIFixture(t)
	content := []byte("explicit UTF-8 evidence\n")
	meta, err := attachment.ValidateObjectBytes("evidence.txt", "file", content)
	if err != nil {
		t.Fatal(err)
	}
	meta.ID, meta.Source, meta.CreatedAt = "att-0123456789abcdef01234567", "upload", time.Now().UTC()
	var uploaded bool
	var downloads int
	f.attachmentMetadata = []model.Attachment{meta}
	f.objectHandler = func(w http.ResponseWriter, r *http.Request) bool {
		base := "/enterprise/device/v1/rooms/server-room/attachments"
		if !strings.HasPrefix(r.URL.Path, base) {
			return false
		}
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		receipt := func(kind, state string) {
			write(map[string]any{"outcome": "committed", "receipt": map[string]any{"version": 1, "kind": kind, "room_id": "server-room", "object_id": meta.ID, "state": state, "metadata": meta, "expires_at": time.Now().Add(time.Hour)}})
		}
		switch {
		case r.URL.Path == base:
			receipt("reserve", "reserved")
		case strings.HasSuffix(r.URL.Path, "/content") && r.Method == http.MethodPut:
			data, _ := io.ReadAll(r.Body)
			if !bytes.Equal(data, content) || r.ContentLength != int64(len(content)) {
				t.Error("CLI upload changed selected evidence")
			}
			uploaded = true
			write(map[string]any{"status": "uploaded", "object_id": meta.ID, "size": meta.Size, "sha256": meta.SHA256})
		case strings.HasSuffix(r.URL.Path, "/verify"):
			if !uploaded {
				t.Error("verification before bytes")
			}
			receipt("verify", "verified")
		case strings.HasSuffix(r.URL.Path, "/content") && r.Method == http.MethodGet:
			if f.calls["claim"] != 0 {
				t.Error("claim preceded download")
			}
			downloads++
			_, _ = w.Write(content)
		default:
			t.Errorf("unexpected object route %s", r.URL.Path)
			w.WriteHeader(404)
		}
		return true
	}
	path := filepath.Join(f.root, "evidence.txt")
	if os.WriteFile(path, content, 0o600) != nil {
		t.Fatal("write fixture")
	}
	var out bytes.Buffer
	if err := f.run([]string{"send", "--repo", f.root, "--room", "server-room", "--id", "upload-send", "--text", "please inspect", "--file", path}, nil, &out); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	selected := f.intents["send/upload-send"].AttachmentIDs
	f.mu.Unlock()
	if selected[0] != meta.ID || f.count("send") != 1 {
		t.Fatal("CLI did not publish verified ID")
	}
	f.mu.Lock()
	f.queued, f.attachmentIncoming = true, true
	f.mu.Unlock()
	printed := &atomic.Bool{}
	f.stdoutComplete = printed
	writer := &attachmentEnvelopeWriter{t: t, f: f, printed: printed}
	if err := f.run([]string{"wait", "--repo", f.root, "--room", "server-room", "--timeout", "1"}, nil, writer); err != nil {
		t.Fatal(err)
	}
	if downloads != 1 || f.count("ack") != 1 || !strings.Contains(writer.String(), "complete inbound evidence") || !strings.Contains(writer.String(), "path:") {
		t.Fatal("attachment collection did not preserve stdout-after-cache ACK")
	}
}

type attachmentEnvelopeWriter struct {
	bytes.Buffer
	t       *testing.T
	f       *enterpriseCLIFixture
	printed *atomic.Bool
}

func (w *attachmentEnvelopeWriter) Write(data []byte) (int, error) {
	var journal map[string]json.RawMessage
	if err := privatefile.ReadJSON(filepath.Join(w.f.recordDir, "delivery.json"), 2<<20, &journal); err != nil || len(journal["structured_claim"]) == 0 {
		w.t.Fatal("structured receipt not durable before stdout")
	}
	if w.f.count("ack") != 0 {
		w.t.Fatal("ACK before stdout")
	}
	n, err := w.Buffer.Write(data)
	if n == len(data) && err == nil {
		w.printed.Store(true)
	}
	return n, err
}

func TestEnterpriseCLIRejectsAttachmentAggregateBeforeUpload(t *testing.T) {
	f := newEnterpriseCLIFixture(t)
	var contacts atomic.Int64
	f.objectHandler = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/attachments") {
			contacts.Add(1)
			w.WriteHeader(500)
			return true
		}
		return false
	}
	path := filepath.Join(f.root, "evidence.txt")
	if os.WriteFile(path, []byte("x"), 0o600) != nil {
		t.Fatal("write fixture")
	}
	args := []string{"send", "--repo", f.root, "--room", "server-room", "--id", "too-many", "--text", "body"}
	for range 9 {
		args = append(args, "--file", path)
	}
	if err := f.run(args, nil, &bytes.Buffer{}); err == nil {
		t.Fatal("ninth file accepted")
	}
	if contacts.Load() != 0 || f.count("send") != 0 {
		t.Fatal("invalid aggregate performed upload or publication")
	}
}
