package attachment

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

type gatedEvidenceReader struct {
	reader  io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *gatedEvidenceReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started); <-r.release })
	return r.reader.Read(p)
}

func TestSharedStagingDoesNotHoldStoreLockOrPublishBeforeCommit(t *testing.T) {
	s, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	r := &gatedEvidenceReader{reader: strings.NewReader("#!/bin/sh\nprintf '中文 \uFFFD'\n"), started: make(chan struct{}), release: make(chan struct{})}
	release := sync.OnceFunc(func() { close(r.release) })
	t.Cleanup(release)
	type result struct {
		stage *Staged
		err   error
	}
	finished := make(chan result, 1)
	go func() { staged, err := s.StageShared("file", "repro.sh", r, "lan"); finished <- result{staged, err} }()
	<-r.started
	localDone := make(chan error, 1)
	go func() {
		_, err := s.SaveEvidence("local.txt", strings.NewReader("independent local upload"), "local")
		localDone <- err
	}()
	select {
	case err := <-localDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("untrusted streaming held the Store lock")
	}
	release()
	staged := <-finished
	if staged.err != nil {
		t.Fatal(staged.err)
	}
	defer staged.stage.Close()
	if _, _, err := s.Resolve(staged.stage.meta.ID); !errors.Is(err, ErrUnknown) {
		t.Fatalf("uncommitted stage was published: %v", err)
	}
	meta, err := staged.stage.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := staged.stage.Commit(); err == nil {
		t.Fatal("stage committed a second time")
	}
	if err := staged.stage.Close(); err != nil {
		t.Fatal(err)
	}
	_, path, err := s.Resolve(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "#!/bin/sh\nprintf '中文 \uFFFD'\n" || filepath.Ext(path) != ".data" {
		t.Fatalf("committed evidence changed: %q %v", data, err)
	}
}

func TestSharedStagingRejectsInvalidOrInterruptedBytesWithoutFiles(t *testing.T) {
	for name, fixture := range map[string]struct {
		kind string
		read io.Reader
	}{
		"empty":        {"file", strings.NewReader("")},
		"nul":          {"file", strings.NewReader("a\x00b")},
		"invalid_utf8": {"file", bytes.NewReader([]byte{0xc3, 0x28})},
		"too_large":    {"file", strings.NewReader(strings.Repeat("a", int(MaxEvidenceBytes)+1))},
		"read_error":   {"file", iotest.ErrReader(io.ErrUnexpectedEOF)},
		"fake_image":   {"image", strings.NewReader("not a PNG")},
		"bad_kind":     {"archive", strings.NewReader("content")},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := Open(t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.StageShared(fixture.kind, "attachment", fixture.read, "lan"); err == nil {
				t.Fatal("invalid stage was accepted")
			}
			entries, err := os.ReadDir(s.Root())
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected stage left data: %v %v", entries, err)
			}
		})
	}
}

func TestSharedStageQuotaSerializesCommitsAndLocalEvidenceIsIndependent(t *testing.T) {
	s, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(s.Root(), "quota.data")
	filler, err := os.Create(quotaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := filler.Truncate(MaxRoomSharedBytes - 2200); err != nil {
		t.Fatal(err)
	}
	if err := filler.Close(); err != nil {
		t.Fatal(err)
	}
	stages := make([]*Staged, 2)
	for i := range stages {
		stages[i], err = s.StageShared("file", "report.txt", strings.NewReader(strings.Repeat("a", 1500)), "lan")
		if err != nil {
			t.Fatal(err)
		}
		defer stages[i].Close()
	}
	results := make(chan error, 2)
	for _, stage := range stages {
		go func() { _, err := stage.Commit(); results <- err }()
	}
	passed := 0
	for range stages {
		if err := <-results; err == nil {
			passed++
		}
	}
	if passed != 1 {
		t.Fatalf("concurrent commits bypassed the shared quota: successes=%d", passed)
	}
	// Local non-LAN evidence does not acquire the LAN Room lifetime quota.
	if _, err := s.SaveEvidence("local.log", strings.NewReader(strings.Repeat("b", 4096)), "native-relay"); err != nil {
		t.Fatalf("local evidence incorrectly inherited LAN quota: %v", err)
	}
	if _, err := s.SaveSharedEvidence("shared.log", strings.NewReader("shared"), "native-relay"); err == nil {
		t.Fatal("shared evidence bypassed a full Room")
	}
}

func TestSharedStageCloseDiscardsOnlyItsUncommittedUpload(t *testing.T) {
	s, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	stage, err := s.StageShared("image", "screen.png", bytes.NewReader(pngBytes(t)), "lan")
	if err != nil {
		t.Fatal(err)
	}
	if err := stage.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := stage.Commit(); err == nil {
		t.Fatal("closed stage committed")
	}
	entries, err := os.ReadDir(s.Root())
	if err != nil || len(entries) != 0 {
		t.Fatalf("abandoned stage left a quota charge: %v %v", entries, err)
	}
	stage, err = s.StageShared("image", "screen.png", bytes.NewReader(pngBytes(t)), "lan")
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	meta, err := stage.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if meta.Kind != "image" || meta.MediaType != "image/png" || meta.Width <= 0 || meta.Height <= 0 {
		t.Fatalf("staged image lost validation: %+v", meta)
	}
}
