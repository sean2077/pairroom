package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestRejectedAppendDoesNotConsumeSequence(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	event := model.Event{Kind: "test", Data: json.RawMessage(`{`)}
	if err := s.Append(&event); err == nil {
		t.Fatal("expected invalid JSON failure")
	}
	if event.Seq != 0 || s.lastSeq != 0 {
		t.Fatalf("failed append consumed sequence: event=%d store=%d", event.Seq, s.lastSeq)
	}
	event.Data = json.RawMessage(`{}`)
	if err := s.Append(&event); err != nil {
		t.Fatal(err)
	}
	if event.Seq != 1 {
		t.Fatalf("first accepted sequence = %d", event.Seq)
	}
}

func TestAppendIOFailureClosesAmbiguousWriter(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.file.Close(); err != nil {
		t.Fatal(err)
	}
	event := model.Event{Kind: "test", Data: json.RawMessage(`{}`)}
	if err := s.Append(&event); err == nil {
		t.Fatal("expected write failure")
	}
	if s.file != nil {
		t.Fatal("writer remained usable after an ambiguous I/O failure")
	}
	if event.Seq != 0 || s.lastSeq != 0 {
		t.Fatalf("failed write consumed sequence: event=%d store=%d", event.Seq, s.lastSeq)
	}
}

func TestStoreRejectsNonContiguousSequences(t *testing.T) {
	for _, sequences := range [][]uint64{{0}, {2}, {1, 1}, {1, 3}, {1, 2, 1}} {
		t.Run(fmt.Sprint(sequences), func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			var data []byte
			for _, seq := range sequences {
				encoded, err := json.Marshal(model.Event{Seq: seq, Kind: "test", Data: json.RawMessage(`{}`)})
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, encoded...)
				data = append(data, '\n')
			}
			if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenExisting(dir)
			if err == nil {
				reopened.Close()
				t.Fatal("accepted corrupt event sequence")
			}
			after, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
			if err != nil || string(after) != string(data) {
				t.Fatalf("corrupt complete records must not be repaired: %v", err)
			}
		})
	}
}

func TestAppendRejectsExplicitSequenceGap(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	event := model.Event{Seq: 3, Kind: "test", Data: json.RawMessage(`{}`)}
	if err := s.Append(&event); err == nil {
		t.Fatal("accepted sequence gap")
	}
}

func TestLoadMissingLogIsNotEmptyHistory(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.Path()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("missing event log became empty history")
	}
}
