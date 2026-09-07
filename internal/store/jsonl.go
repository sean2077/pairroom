package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	roomID  string
}

func Open(dir string) (*JSONLStore, error) {
	return open(dir, true, "")
}

// OpenExisting opens an already-published event store without creating a
// missing data directory or events.jsonl. Lifecycle mutations use this stricter
// boundary so external data loss cannot be mistaken for a new empty Room.
func OpenExisting(dir string) (*JSONLStore, error) {
	return open(dir, false, "")
}

// OpenExistingForRoom also binds the writer to the published Room identity.
// Validate identity during the existing repair scan, before changing any bytes,
// so missing/replaced history cannot become a new Room or a cross-Room append.
func OpenExistingForRoom(dir, roomID string) (*JSONLStore, error) {
	if strings.TrimSpace(roomID) == "" {
		return nil, errors.New("published Room ID is required")
	}
	return open(dir, false, roomID)
}

func open(dir string, create bool, roomID string) (*JSONLStore, error) {
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
	store := &JSONLStore{dir: dir, path: path, roomID: roomID}
	// Schema is the compatibility boundary. Check it before repairing or
	// decoding Event Log bytes so an old Room cannot be partially interpreted
	// or mutated by a build that explicitly provides no migration.
	if err := store.ensureMetadata(allowMetadataCreate); err != nil {
		return nil, err
	}
	lastSeq, err := repairEventLog(path, create, roomID)
	if err != nil {
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
	// Repair already decoded and validated every complete record. Reuse its
	// sequence instead of allocating and decoding the entire replay again.
	store.file, store.lastSeq = file, lastSeq
	return store, nil
}

// repairEventLog makes append-after-crash safe. A process can be terminated
// after writing only part of the final JSON object; simply ignoring that tail
// during replay is insufficient because the next append would concatenate a
// valid event onto the broken object. We truncate only an invalid unterminated
// final line. A valid final object without a newline is normalized by adding
// one. Corruption before the final line remains a hard error.
func repairEventLog(path string, create bool, roomID string) (uint64, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open event log for repair: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 128*1024)
	var lastGood int64
	var lastSeq uint64
	for lineNo := 1; ; lineNo++ {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var event model.Event
			if err := json.Unmarshal(line, &event); err != nil {
				if errors.Is(readErr, io.EOF) {
					if roomID != "" && lastSeq == 0 {
						return 0, errors.New("published Room has no complete identity event")
					}
					if err := file.Truncate(lastGood); err != nil {
						return 0, fmt.Errorf("truncate partial event log tail: %w", err)
					}
					return lastSeq, file.Sync()
				}
				return 0, fmt.Errorf("decode event log line %d during repair: %w", lineNo, err)
			}
			if err := checkSequence(event.Seq, lastSeq); err != nil {
				return 0, fmt.Errorf("event log line %d during repair: %w", lineNo, err)
			}
			if err := checkRoomIdentity(event, roomID, lastSeq == 0); err != nil {
				return 0, err
			}
			lastSeq = event.Seq
			lastGood += int64(len(line))
			if errors.Is(readErr, io.EOF) && line[len(line)-1] != '\n' {
				if _, err := file.WriteAt([]byte{'\n'}, lastGood); err != nil {
					return 0, fmt.Errorf("normalize event log newline: %w", err)
				}
				return lastSeq, file.Sync()
			}
		}
		if errors.Is(readErr, io.EOF) {
			if roomID != "" && lastSeq == 0 {
				return 0, errors.New("published Room event log is empty")
			}
			return lastSeq, nil
		}
		if readErr != nil {
			return 0, fmt.Errorf("read event log during repair: %w", readErr)
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
		if !version.SupportsStoreSchema(metadata.SchemaVersion) {
			return fmt.Errorf("event store schema %d is unsupported; this build requires schema %d (schema 9 is also readable) and provides no migration", metadata.SchemaVersion, version.StoreSchema)
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
	if event == nil {
		return errors.New("event is required")
	}
	candidate := *event
	if candidate.Seq == 0 {
		candidate.Seq = s.lastSeq + 1
	}
	if err := checkSequence(candidate.Seq, s.lastSeq); err != nil {
		return err
	}
	if err := checkRoomIdentity(candidate, s.roomID, s.lastSeq == 0); err != nil {
		return err
	}
	data, err := json.Marshal(candidate)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	data = append(data, '\n')
	if _, err := s.file.Write(data); err != nil {
		return s.failWriteLocked("append event", err)
	}
	if err := s.file.Sync(); err != nil {
		return s.failWriteLocked("sync event log", err)
	}
	// Only acknowledge a sequence after the complete record is durable. A
	// rejected marshal must not poison a retry or create a replay-breaking gap.
	s.lastSeq, event.Seq = candidate.Seq, candidate.Seq
	return nil
}

func checkRoomIdentity(event model.Event, roomID string, first bool) error {
	if roomID == "" {
		return nil
	}
	if event.RoomID != roomID {
		return fmt.Errorf("event belongs to Room %q, expected %q", event.RoomID, roomID)
	}
	if first {
		var meta model.RoomMeta
		if event.Kind != "room.created" || json.Unmarshal(event.Data, &meta) != nil || meta.ID != roomID {
			return fmt.Errorf("published Room %q has no matching room.created identity", roomID)
		}
	} else if event.Kind == "room.created" {
		return errors.New("published Room has multiple room.created identities")
	}
	return nil
}

func checkSequence(seq, previous uint64) error {
	if seq == 0 || seq != previous+1 {
		return fmt.Errorf("event sequence %d must immediately follow %d", seq, previous)
	}
	return nil
}

func (s *JSONLStore) failWriteLocked(operation string, err error) error {
	// The write may have reached disk partially, or Sync may have failed after
	// a complete write. Continuing to append would concatenate onto an unknown
	// tail or reuse an uncertain sequence. Reopening repairs/replays disk truth.
	_ = s.file.Close()
	s.file = nil
	return fmt.Errorf("%s: %w; event store closed, reopen before retrying", operation, err)
}

func (s *JSONLStore) Load() ([]model.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	file, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("open event log for reading: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 128*1024)
	events := make([]model.Event, 0, 256)
	lineNo := 0
	var lastSeq uint64
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
			if err := checkSequence(event.Seq, lastSeq); err != nil {
				return nil, fmt.Errorf("event log line %d: %w", lineNo, err)
			}
			if err := checkRoomIdentity(event, s.roomID, lastSeq == 0); err != nil {
				return nil, err
			}
			lastSeq = event.Seq
			events = append(events, event)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read event log: %w", readErr)
		}
	}
	if s.roomID != "" && len(events) == 0 {
		return nil, errors.New("published Room event log is empty")
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
