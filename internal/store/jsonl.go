package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/sean2077/pairroom/internal/model"
)

// JSONLStore is an append-only local event store. It deliberately avoids a
// database dependency and makes recovery/debugging possible with ordinary
// command-line tools.
type JSONLStore struct {
	mu      sync.Mutex
	dir     string
	path    string
	file    *os.File
	lastSeq uint64
}

func Open(dir string) (*JSONLStore, error) {
	if dir == "" {
		return nil, errors.New("data directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dir, "events.jsonl")
	if err := repairEventLog(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	store := &JSONLStore{dir: dir, path: path, file: file}
	events, err := store.Load()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if len(events) > 0 {
		store.lastSeq = events[len(events)-1].Seq
	}
	if err := store.ensureMetadata(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return store, nil
}

// repairEventLog makes append-after-crash safe. A process can be terminated
// after writing only part of the final JSON object; simply ignoring that tail
// during replay is insufficient because the next append would concatenate a
// valid event onto the broken object. We truncate only an invalid unterminated
// final line. A valid final object without a newline is normalized by adding
// one. Corruption before the final line remains a hard error.
func repairEventLog(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open event log for repair: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 128*1024)
	var lastGood int64
	for lineNo := 1; ; lineNo++ {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var event model.Event
			if err := json.Unmarshal(line, &event); err != nil {
				if errors.Is(readErr, io.EOF) {
					if err := file.Truncate(lastGood); err != nil {
						return fmt.Errorf("truncate partial event log tail: %w", err)
					}
					return file.Sync()
				}
				return fmt.Errorf("decode event log line %d during repair: %w", lineNo, err)
			}
			lastGood += int64(len(line))
			if errors.Is(readErr, io.EOF) && line[len(line)-1] != '\n' {
				if _, err := file.WriteAt([]byte{'\n'}, lastGood); err != nil {
					return fmt.Errorf("normalize event log newline: %w", err)
				}
				return file.Sync()
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read event log during repair: %w", readErr)
		}
	}
}

func (s *JSONLStore) ensureMetadata() error {
	const name = "metadata.json"
	if _, err := os.Stat(filepath.Join(s.dir, name)); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat event metadata: %w", err)
	}
	return s.SaveJSON(name, map[string]any{
		"format":         "pairroom-jsonl",
		"schema_version": 1,
	})
}

func (s *JSONLStore) Dir() string  { return s.dir }
func (s *JSONLStore) Path() string { return s.path }

func (s *JSONLStore) Append(event *model.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return errors.New("event store is closed")
	}
	if event.Seq == 0 {
		s.lastSeq++
		event.Seq = s.lastSeq
	} else if event.Seq > s.lastSeq {
		s.lastSeq = event.Seq
	} else {
		return fmt.Errorf("event sequence %d is not greater than last sequence %d", event.Seq, s.lastSeq)
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	data = append(data, '\n')
	if _, err := s.file.Write(data); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("sync event log: %w", err)
	}
	return nil
}

func (s *JSONLStore) Load() ([]model.Event, error) {
	file, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open event log for reading: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 128*1024)
	events := make([]model.Event, 0, 256)
	lineNo := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			var event model.Event
			if err := json.Unmarshal(line, &event); err != nil {
				// A crash can leave one partial final line. Ignore only that exact case.
				if errors.Is(readErr, io.EOF) {
					break
				}
				return nil, fmt.Errorf("decode event log line %d: %w", lineNo, err)
			}
			events = append(events, event)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read event log: %w", readErr)
		}
	}
	return events, nil
}

func (s *JSONLStore) SaveJSON(name string, value any) error {
	if filepath.Base(name) != name {
		return errors.New("metadata name must be a base filename")
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(s.dir, name+".*.tmp")
	if err != nil {
		return fmt.Errorf("create metadata temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod metadata temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write metadata: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close metadata: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(s.dir, name)); err != nil {
		return fmt.Errorf("replace metadata: %w", err)
	}
	return nil
}

func (s *JSONLStore) LoadJSON(name string, value any) error {
	if filepath.Base(name) != name {
		return errors.New("metadata name must be a base filename")
	}
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode metadata %s: %w", name, err)
	}
	return nil
}

func (s *JSONLStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}
