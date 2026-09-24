package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func appendFixture(t *testing.T, s *JSONLStore, roomID, body string) uint64 {
	t.Helper()
	event, err := model.NewEvent(roomID, "fixture", model.ActorSystem, map[string]string{"body": body})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(&event); err != nil {
		t.Fatal(err)
	}
	return event.Seq
}

func readBody(t *testing.T, s *JSONLStore, seq uint64) string {
	t.Helper()
	event, err := s.ReadEvent(seq)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]string
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		t.Fatal(err)
	}
	return payload["body"]
}

// The offset index is built by the open-time repair scan and extended by each
// durable append, including across reopen, repaired tails and large records.
func TestReadEventUsesDurableOffsetIndex(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("L", 300<<10)
	bodies := []string{"first", large, "third"}
	for _, body := range bodies {
		appendFixture(t, s, "room-index", body)
	}
	for i, want := range bodies {
		if got := readBody(t, s, uint64(i+1)); got != want {
			t.Fatalf("seq %d body length %d, want %d", i+1, len(got), len(want))
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash that left a partial final record; reopen must repair it
	// and index only complete records, then keep indexing new appends.
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":4,"kind":"fix`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := readBody(t, reopened, 2); got != large {
		t.Fatal("reopened index lost a large record")
	}
	if _, err := reopened.ReadEvent(4); err == nil {
		t.Fatal("a repaired partial tail must not be indexed")
	}
	seq := appendFixture(t, reopened, "room-index", "after repair")
	if seq != 4 || readBody(t, reopened, 4) != "after repair" || readBody(t, reopened, 3) != "third" {
		t.Fatal("index was not extended correctly after repair")
	}
	for _, bad := range []uint64{0, 5} {
		if _, err := reopened.ReadEvent(bad); err == nil {
			t.Fatalf("out-of-range seq %d was accepted", bad)
		}
	}
}

func TestReadEventNormalizesUnterminatedFinalRecord(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	appendFixture(t, s, "room-newline", "one")
	appendFixture(t, s, "room-newline", "two")
	_ = s.Close()
	path := filepath.Join(dir, "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)-1], 0o600); err != nil { // drop the final newline
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	appendFixture(t, reopened, "room-newline", "three")
	for seq, want := range map[uint64]string{1: "one", 2: "two", 3: "three"} {
		if got := readBody(t, reopened, seq); got != want {
			t.Fatalf("seq %d = %q, want %q", seq, got, want)
		}
	}
}

func TestReadEventFailsClosedAfterClose(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	appendFixture(t, s, "room-closed", "value")
	_ = s.Close()
	if _, err := s.ReadEvent(1); err == nil {
		t.Fatal("closed store served an indexed read")
	}
}
