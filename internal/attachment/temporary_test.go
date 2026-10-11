package attachment

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

func temporaryFixture(t *testing.T, root, pattern string, size int64) string {
	t.Helper()
	file, err := os.CreateTemp(root, pattern)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return file.Name()
}

func temporaryInventory(t *testing.T, root string) map[string]int64 {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]int64, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = info.Size()
	}
	return result
}

type observedTemporaryReader struct {
	called bool
}

func (r *observedTemporaryReader) Read([]byte) (int, error) {
	r.called = true
	return 0, io.EOF
}

func TestSharedTemporaryBudgetRetainsCrashDebrisAcrossReopen(t *testing.T) {
	for _, pattern := range []string{".staged-*.tmp", ".attachment-*.tmp", ".image-*.tmp", ".metadata-*.tmp"} {
		t.Run(pattern, func(t *testing.T) {
			dir := t.TempDir()
			original, err := Open(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			kept, err := original.SaveEvidence("kept.log", strings.NewReader("original cached evidence\n"), "fixture")
			if err != nil {
				t.Fatal(err)
			}
			temporaryFixture(t, original.Root(), pattern, 32<<20)
			before := temporaryInventory(t, original.Root())
			// A new Store has no in-memory knowledge of the interrupted writer.
			reopened, err := Open(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				reader := &observedTemporaryReader{}
				stage, err := reopened.StageShared("file", "new.log", reader, "lan")
				if stage != nil {
					_ = stage.Close()
				}
				if !errors.Is(err, ErrTemporaryQuota) || reader.called {
					t.Fatalf("full persisted budget allowed more upload work: reader=%v err=%v", reader.called, err)
				}
			}
			if after := temporaryInventory(t, reopened.Root()); !reflect.DeepEqual(before, after) {
				t.Fatalf("reopen/rejection changed stored files: before=%v after=%v", before, after)
			}
			if metadata, _, err := reopened.Resolve(kept.ID); err != nil || metadata != kept {
				t.Fatalf("budget handling changed valid cached evidence: %+v %v", metadata, err)
			}
		})
	}
}

func TestSharedTemporaryBudgetChargesZeroByteDebris(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	// 512 * 64 KiB consumes the finite 32 MiB budget even though the crash
	// happened between every create and first write.
	for range 512 {
		temporaryFixture(t, s.Root(), ".metadata-*.tmp", 0)
	}
	reopened, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	before := temporaryInventory(t, reopened.Root())
	reader := &observedTemporaryReader{}
	stage, err := reopened.StageShared("file", "new.log", reader, "lan")
	if stage != nil {
		_ = stage.Close()
	}
	if !errors.Is(err, ErrTemporaryQuota) || reader.called {
		t.Fatalf("empty crash debris did not consume budget: read=%v err=%v", reader.called, err)
	}
	if after := temporaryInventory(t, reopened.Root()); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected upload created another temporary or deleted existing debris")
	}
}

type heldTemporaryReader struct {
	reader  io.Reader
	started chan<- struct{}
	release <-chan struct{}
	once    sync.Once
}

func (r *heldTemporaryReader) Read(data []byte) (int, error) {
	r.once.Do(func() { r.started <- struct{}{}; <-r.release })
	return r.reader.Read(data)
}

func TestSharedTemporaryBudgetReservesConcurrentStreamsAcrossStores(t *testing.T) {
	dir := t.TempDir()
	const streams = 6 // six maximum 5 MiB uploads fit; a seventh exceeds 32 MiB
	started := make(chan struct{}, streams)
	failures := make(chan error, streams)
	release := make(chan struct{})
	releaseAll := sync.OnceFunc(func() { close(release) })
	var writers sync.WaitGroup
	stages := make([]*Staged, streams)
	content := strings.Repeat("e", 5<<20)
	stores := make([]*Store, streams)
	for i := range stores {
		var err error
		stores[i], err = Open(dir, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		releaseAll()
		writers.Wait()
		for _, stage := range stages {
			if stage != nil {
				_ = stage.Close()
			}
		}
	})
	for i := range stores {
		writers.Add(1)
		go func() {
			defer writers.Done()
			reader := &heldTemporaryReader{reader: strings.NewReader(content), started: started, release: release}
			var err error
			stages[i], err = stores[i].StageShared("file", "large.log", reader, "lan")
			if err != nil {
				failures <- err
			}
		}()
	}
	for range streams {
		select {
		case <-started:
		case err := <-failures:
			t.Fatalf("valid concurrent stage failed: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("one stream held a directory/Store lock across its request body")
		}
	}
	// All readers are still blocked before their first byte. Their full
	// reservations must already be visible to a newly constructed Store.
	reopened, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	before := temporaryInventory(t, reopened.Root())
	if len(before) != streams {
		t.Fatalf("live reservation count=%d want=%d", len(before), streams)
	}
	for name, size := range before {
		if size != (5<<20)+1 {
			t.Fatalf("stream %s has no persisted maximum reservation: %d", name, size)
		}
	}
	reader := &observedTemporaryReader{}
	if stage, err := reopened.StageShared("file", "extra.log", reader, "lan"); stage != nil || !errors.Is(err, ErrTemporaryQuota) || reader.called {
		if stage != nil {
			_ = stage.Close()
		}
		t.Fatalf("reopened Store bypassed live reservations: read=%v err=%v", reader.called, err)
	}
	if after := temporaryInventory(t, reopened.Root()); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected concurrent upload changed temporary files")
	}
	releaseAll()
	writers.Wait()
	select {
	case err := <-failures:
		t.Fatalf("reserved stream failed after release: %v", err)
	default:
	}
	for _, stage := range stages {
		info, err := os.Stat(stage.path)
		if err != nil || info.Size() != 5<<20 {
			t.Fatalf("completed stream kept its overflow reservation: %v %v", info, err)
		}
	}
	// A successful commit releases its temporary charge in every Store;
	// closing that committed stage must leave the published content intact.
	meta, err := stages[0].Commit()
	if err != nil {
		t.Fatal(err)
	}
	if err := stages[0].Close(); err != nil {
		t.Fatal(err)
	}
	extra, err := reopened.StageShared("file", "extra.log", strings.NewReader("new upload"), "lan")
	if err != nil {
		t.Fatalf("commit did not release persistent temporary budget: %v", err)
	}
	defer extra.Close()
	if metadata, _, err := reopened.Resolve(meta.ID); err != nil || metadata != meta {
		t.Fatalf("closing a committed stage removed published evidence: %+v %v", metadata, err)
	}
}

func TestSharedTemporaryBudgetReleasesClosedAndFailedStages(t *testing.T) {
	s, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	// Leave exactly enough capacity to reserve one maximum-sized stream.
	temporaryFixture(t, s.Root(), ".attachment-*.tmp", (32<<20)-(5<<20)-1)
	baseline := temporaryInventory(t, s.Root())
	first, err := s.StageShared("file", "first.log", strings.NewReader("small completed stage"), "lan")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := s.StageShared("file", "second.log", strings.NewReader("new upload"), "lan"); second != nil || !errors.Is(err, ErrTemporaryQuota) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("completed but uncommitted stage was not charged: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if after := temporaryInventory(t, s.Root()); !reflect.DeepEqual(baseline, after) {
		t.Fatal("Close did not release only its own persistent reservation")
	}
	if _, err := s.StageShared("file", "failed.log", iotest.ErrReader(io.ErrUnexpectedEOF), "lan"); err == nil {
		t.Fatal("interrupted stream was accepted")
	}
	if after := temporaryInventory(t, s.Root()); !reflect.DeepEqual(baseline, after) {
		t.Fatal("failed stream leaked a reservation")
	}
	last, err := s.StageShared("file", "last.log", strings.NewReader("capacity recovered"), "lan")
	if err != nil {
		t.Fatalf("Close/failure did not return upload capacity: %v", err)
	}
	defer last.Close()
}

func TestSharedTemporaryBudgetLeavesUnknownFilesAndRejectsNonRegularDebris(t *testing.T) {
	s, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	unknown := temporaryFixture(t, s.Root(), ".owner-notes-*.tmp", 32<<20)
	stage, err := s.StageShared("file", "new.log", strings.NewReader("valid evidence"), "lan")
	if err != nil {
		t.Fatalf("unrecognized owner file was mistaken for a temporary: %v", err)
	}
	if err := stage.Close(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(unknown); err != nil || info.Size() != 32<<20 {
		t.Fatalf("owner file was changed: %v %v", info, err)
	}
	if err := os.Mkdir(filepath.Join(s.Root(), ".staged-invalid.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	before := temporaryInventory(t, s.Root())
	reader := &observedTemporaryReader{}
	if stage, err := s.StageShared("file", "new.log", reader, "lan"); stage != nil || err == nil || reader.called {
		if stage != nil {
			_ = stage.Close()
		}
		t.Fatalf("non-regular temporary was trusted: read=%v err=%v", reader.called, err)
	}
	if after := temporaryInventory(t, s.Root()); !reflect.DeepEqual(before, after) {
		t.Fatal("non-regular temporary rejection changed files")
	}
}
