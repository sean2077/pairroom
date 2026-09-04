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
	"github.com/sean2077/pairroom/internal/version"
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
	return open(dir, true)
}

// OpenExisting opens an already-published event store without creating a
// missing data directory or events.jsonl. Lifecycle mutations use this stricter
// boundary so external data loss cannot be mistaken for a new empty Room.
func OpenExisting(dir string) (*JSONLStore, error) {
	return open(dir, false)
}

func open(dir string, create bool) (*JSONLStore, error) {
	if dir == "" {
		return nil, errors.New("data directory is required")
	}
	if create {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	} else {
		info, err := os.Lstat(dir)
		if err != nil {
			return nil, fmt.Errorf("stat data directory: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("data path is not a real directory: %s", dir)
		}
	}
	path := filepath.Join(dir, "events.jsonl")
	if !create {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("stat event log: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("event log is not a regular file: %s", path)
		}
	}
	// Only a caller creating a genuinely new empty event log may create the
	// schema marker. An existing directory opened through OpenExisting is an
	// already-published Room; a missing marker there is legacy/ambiguous state
	// and must fail closed rather than being silently upgraded.
	allowMetadataCreate := create
	if info, err := os.Stat(path); err == nil {
		allowMetadataCreate = create && info.Size() == 0
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("stat event log: %w", err)
	}
	store := &JSONLStore{dir: dir, path: path}
	// Schema is the compatibility boundary. Check it before repairing or
	// decoding Event Log bytes so an old Room cannot be partially interpreted
	// or mutated by a build that explicitly provides no migration.
	if err := store.ensureMetadata(allowMetadataCreate); err != nil {
		return nil, err
	}
	if err := repairEventLog(path, create); err != nil {
		return nil, err
	}
	flags := os.O_RDWR | os.O_APPEND
	if create {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open event log: %w", err)
	}
	store.file = file
	events, err := store.Load()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if len(events) > 0 {
		store.lastSeq = events[len(events)-1].Seq
	}
	return store, nil
}

// repairEventLog makes append-after-crash safe. A process can be terminated
// after writing only part of the final JSON object; simply ignoring that tail
// during replay is insufficient because the next append would concatenate a
// valid event onto the broken object. We truncate only an invalid unterminated
// final line. A valid final object without a newline is normalized by adding
// one. Corruption before the final line remains a hard error.
func repairEventLog(path string, create bool) error {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(path, flags, 0o600)
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

func (s *JSONLStore) ensureMetadata(allowCreate bool) error {
	const name = "metadata.json"
	path := filepath.Join(s.dir, name)
	if _, err := os.Stat(path); err == nil {
		var metadata struct {
			Format        string `json:"format"`
			SchemaVersion int    `json:"schema_version"`
		}
		if err := s.LoadJSON(name, &metadata); err != nil {
			return err
		}
		if metadata.Format != "" && metadata.Format != "pairroom-jsonl" {
			return fmt.Errorf("unsupported event metadata format %q", metadata.Format)
		}
		if metadata.SchemaVersion != version.StoreSchema {
			return fmt.Errorf("event store schema %d is unsupported; this build requires schema %d and provides no migration", metadata.SchemaVersion, version.StoreSchema)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat event metadata: %w", err)
	}
	if !allowCreate {
		return errors.New("event store metadata is missing; legacy stores are not migrated")
	}
	return s.SaveJSON(name, map[string]any{
		"format":         "pairroom-jsonl",
		"schema_version": version.StoreSchema,
		"app_version":    version.Current,
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
