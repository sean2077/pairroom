// Package archive provides strict, dependency-free verification, backup,
// restore, and diagnostics for one PairRoom data directory.
package archive

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/version"
)

const (
	backupFormat        = "pairroom-backup"
	backupFormatVersion = 1
	maxRestoreFiles     = 100_000
	maxRestoreBytes     = int64(8 << 30)
)

type VerifyReport struct {
	DataDir               string         `json:"data_dir"`
	OK                    bool           `json:"ok"`
	Format                string         `json:"format,omitempty"`
	SchemaVersion         int            `json:"schema_version,omitempty"`
	ApplicationVersion    string         `json:"application_version,omitempty"`
	EventCount            int            `json:"event_count"`
	FirstSequence         uint64         `json:"first_sequence,omitempty"`
	LastSequence          uint64         `json:"last_sequence,omitempty"`
	RoomID                string         `json:"room_id,omitempty"`
	AttachmentCount       int            `json:"attachment_count"`
	ReferencedAttachments int            `json:"referenced_attachments"`
	TotalBytes            int64          `json:"total_bytes"`
	EventKinds            map[string]int `json:"event_kinds,omitempty"`
	Errors                []string       `json:"errors,omitempty"`
	Warnings              []string       `json:"warnings,omitempty"`
	VerifiedAt            time.Time      `json:"verified_at"`
}

type manifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type BackupManifest struct {
	Format          string         `json:"format"`
	FormatVersion   int            `json:"format_version"`
	PairRoomVersion string         `json:"pairroom_version"`
	CreatedAt       time.Time      `json:"created_at"`
	Files           []manifestFile `json:"files"`
}

type attachmentManifest struct {
	Attachment model.Attachment `json:"attachment"`
	Filename   string           `json:"filename"`
}

type diagnosticSummary struct {
	PairRoomVersion string        `json:"pairroom_version"`
	GeneratedAt     time.Time     `json:"generated_at"`
	OS              string        `json:"os,omitempty"`
	Architecture    string        `json:"architecture,omitempty"`
	Verify          VerifyReport  `json:"verify"`
	EventTail       []eventHeader `json:"event_tail,omitempty"`
	Notes           []string      `json:"notes"`
}

type eventHeader struct {
	Seq       uint64        `json:"seq"`
	ID        string        `json:"id"`
	RoomID    string        `json:"room_id"`
	Kind      string        `json:"kind"`
	Actor     model.ActorID `json:"actor"`
	CreatedAt time.Time     `json:"created_at"`
}

// Verify checks the event log, metadata, and immutable attachment objects
// without repairing or mutating the data directory.
func Verify(dataDir string) VerifyReport {
	report := VerifyReport{
		DataDir: filepath.Clean(dataDir), EventKinds: make(map[string]int),
		VerifiedAt: time.Now().UTC(),
	}
	absolute, err := filepath.Abs(dataDir)
	if err != nil {
		report.Errors = append(report.Errors, "resolve data directory: "+err.Error())
		return finishReport(report)
	}
	report.DataDir = absolute
	info, err := os.Stat(absolute)
	if err != nil {
		report.Errors = append(report.Errors, "stat data directory: "+err.Error())
		return finishReport(report)
	}
	if !info.IsDir() {
		report.Errors = append(report.Errors, "data path is not a directory")
		return finishReport(report)
	}

	var metadata struct {
		Format        string `json:"format"`
		SchemaVersion int    `json:"schema_version"`
		AppVersion    string `json:"app_version"`
	}
	metadataPath := filepath.Join(absolute, "metadata.json")
	if err := decodeStrictFile(metadataPath, &metadata); err != nil {
		report.Errors = append(report.Errors, "metadata.json: "+err.Error())
	} else {
		report.Format = metadata.Format
		report.SchemaVersion = metadata.SchemaVersion
		report.ApplicationVersion = metadata.AppVersion
		if metadata.Format != "pairroom-jsonl" {
			report.Errors = append(report.Errors, fmt.Sprintf("unsupported metadata format %q", metadata.Format))
		}
		if metadata.SchemaVersion < 1 {
			report.Errors = append(report.Errors, "metadata schema version must be positive")
		}
		if metadata.SchemaVersion != version.StoreSchema {
			report.Errors = append(report.Errors, fmt.Sprintf("schema %d is unsupported; this build requires schema %d", metadata.SchemaVersion, version.StoreSchema))
		}
	}

	referenced := make(map[string]struct{})
	eventsPath := filepath.Join(absolute, "events.jsonl")
	file, err := os.Open(eventsPath)
	if err != nil {
		report.Errors = append(report.Errors, "events.jsonl: "+err.Error())
	} else {
		defer file.Close()
		reader := bufio.NewReaderSize(file, 128*1024)
		seenIDs := make(map[string]struct{})
		var previous uint64
		lineNo := 0
		for {
			line, readErr := reader.ReadBytes('\n')
			if len(line) > 0 {
				lineNo++
				if line[len(line)-1] != '\n' {
					report.Errors = append(report.Errors, fmt.Sprintf("events.jsonl line %d is unterminated", lineNo))
					break
				}
				var event model.Event
				if err := json.Unmarshal(line, &event); err != nil {
					report.Errors = append(report.Errors, fmt.Sprintf("events.jsonl line %d: %v", lineNo, err))
					break
				}
				report.EventCount++
				report.EventKinds[event.Kind]++
				if report.FirstSequence == 0 {
					report.FirstSequence = event.Seq
				}
				report.LastSequence = event.Seq
				if event.Seq == 0 || (previous != 0 && event.Seq != previous+1) {
					report.Errors = append(report.Errors, fmt.Sprintf("event sequence at line %d is %d after %d", lineNo, event.Seq, previous))
				}
				previous = event.Seq
				if strings.TrimSpace(event.ID) == "" {
					report.Errors = append(report.Errors, fmt.Sprintf("event line %d has an empty id", lineNo))
				} else if _, ok := seenIDs[event.ID]; ok {
					report.Errors = append(report.Errors, fmt.Sprintf("duplicate event id %q", event.ID))
				} else {
					seenIDs[event.ID] = struct{}{}
				}
				if report.RoomID == "" {
					report.RoomID = event.RoomID
				} else if event.RoomID != report.RoomID {
					report.Errors = append(report.Errors, fmt.Sprintf("event line %d has room id %q; expected %q", lineNo, event.RoomID, report.RoomID))
				}
				if event.Kind == "message.created" {
					var message model.Message
					if err := json.Unmarshal(event.Data, &message); err != nil {
						report.Errors = append(report.Errors, fmt.Sprintf("message event at line %d: %v", lineNo, err))
					} else {
						for _, attachment := range message.Attachments {
							if attachment.ID != "" {
								referenced[attachment.ID] = struct{}{}
							}
						}
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				report.Errors = append(report.Errors, "read events.jsonl: "+readErr.Error())
				break
			}
		}
	}

	attachmentFiles, attachmentErrs, attachmentWarnings, total := verifyAttachments(absolute, referenced)
	report.AttachmentCount = len(attachmentFiles) / 2
	report.ReferencedAttachments = len(referenced)
	report.TotalBytes = total
	report.Errors = append(report.Errors, attachmentErrs...)
	report.Warnings = append(report.Warnings, attachmentWarnings...)
	for id := range referenced {
		if _, ok := attachmentFiles[filepath.ToSlash(filepath.Join("attachments", id+".json"))]; !ok {
			report.Errors = append(report.Errors, fmt.Sprintf("referenced attachment %q has no manifest", id))
		}
	}
	return finishReport(report)
}

func finishReport(report VerifyReport) VerifyReport {
	sort.Strings(report.Errors)
	sort.Strings(report.Warnings)
	report.OK = len(report.Errors) == 0
	if len(report.EventKinds) == 0 {
		report.EventKinds = nil
	}
	return report
}

func verifyAttachments(root string, referenced map[string]struct{}) (map[string]manifestFile, []string, []string, int64) {
	files := make(map[string]manifestFile)
	var errs, warnings []string
	var total int64
	attachmentsDir := filepath.Join(root, "attachments")
	entries, err := os.ReadDir(attachmentsDir)
	if errors.Is(err, os.ErrNotExist) {
		if len(referenced) > 0 {
			errs = append(errs, "attachments directory is missing")
		}
		return files, errs, warnings, total
	}
	if err != nil {
		return files, []string{"read attachments: " + err.Error()}, warnings, total
	}
	known := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		manifestPath := filepath.Join(attachmentsDir, entry.Name())
		if info, err := os.Lstat(manifestPath); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			errs = append(errs, fmt.Sprintf("attachment manifest %q is not a regular file", entry.Name()))
			continue
		}
		var value attachmentManifest
		if err := decodeStrictFile(manifestPath, &value); err != nil {
			errs = append(errs, fmt.Sprintf("attachment manifest %q: %v", entry.Name(), err))
			continue
		}
		if value.Attachment.ID == "" || entry.Name() != value.Attachment.ID+".json" {
			errs = append(errs, fmt.Sprintf("attachment manifest %q has inconsistent id %q", entry.Name(), value.Attachment.ID))
			continue
		}
		if filepath.Base(value.Filename) != value.Filename || value.Filename == "" {
			errs = append(errs, fmt.Sprintf("attachment %q has unsafe filename %q", value.Attachment.ID, value.Filename))
			continue
		}
		contentPath := filepath.Join(attachmentsDir, value.Filename)
		info, err := os.Lstat(contentPath)
		if err != nil {
			errs = append(errs, fmt.Sprintf("attachment %q content: %v", value.Attachment.ID, err))
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			errs = append(errs, fmt.Sprintf("attachment %q content is not a regular file", value.Attachment.ID))
			continue
		}
		hash, size, err := hashFile(contentPath)
		if err != nil {
			errs = append(errs, fmt.Sprintf("attachment %q content: %v", value.Attachment.ID, err))
			continue
		}
		if size != value.Attachment.Size {
			errs = append(errs, fmt.Sprintf("attachment %q size is %d; expected %d", value.Attachment.ID, size, value.Attachment.Size))
		}
		if !strings.EqualFold(hash, value.Attachment.SHA256) {
			errs = append(errs, fmt.Sprintf("attachment %q hash mismatch", value.Attachment.ID))
		}
		manifestHash, manifestSize, err := hashFile(manifestPath)
		if err != nil {
			errs = append(errs, fmt.Sprintf("attachment manifest %q hash: %v", entry.Name(), err))
			continue
		}
		manifestRel := filepath.ToSlash(filepath.Join("attachments", entry.Name()))
		contentRel := filepath.ToSlash(filepath.Join("attachments", value.Filename))
		files[manifestRel] = manifestFile{Path: manifestRel, Size: manifestSize, SHA256: manifestHash}
		files[contentRel] = manifestFile{Path: contentRel, Size: size, SHA256: hash}
		known[entry.Name()] = struct{}{}
		known[value.Filename] = struct{}{}
		total += manifestSize + size
		if _, ok := referenced[value.Attachment.ID]; !ok {
			warnings = append(warnings, fmt.Sprintf("attachment %q is not referenced by the transcript", value.Attachment.ID))
		}
	}
	for _, entry := range entries {
		if _, ok := known[entry.Name()]; ok || entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		warnings = append(warnings, fmt.Sprintf("orphan attachment file %q is excluded from backup", entry.Name()))
	}
	return files, errs, warnings, total
}

// Backup writes a verified, self-describing tar.gz archive. Runtime caches,
// reviewer worktrees, browser sessions, and temporary uploads are excluded.
func Backup(dataDir, output string) (BackupManifest, error) {
	report := Verify(dataDir)
	if !report.OK {
		return BackupManifest{}, fmt.Errorf("data verification failed: %s", strings.Join(report.Errors, "; "))
	}
	root, err := filepath.Abs(dataDir)
	if err != nil {
		return BackupManifest{}, err
	}
	files, err := collectBackupFiles(root)
	if err != nil {
		return BackupManifest{}, err
	}
	manifest := BackupManifest{
		Format: backupFormat, FormatVersion: backupFormatVersion,
		PairRoomVersion: version.Current, CreatedAt: time.Now().UTC(), Files: files,
	}
	if strings.TrimSpace(output) == "" {
		return BackupManifest{}, errors.New("backup output path is required")
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return BackupManifest{}, err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return BackupManifest{}, fmt.Errorf("create backup output directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), ".pairroom-backup-*.tmp")
	if err != nil {
		return BackupManifest{}, fmt.Errorf("create backup temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return BackupManifest{}, err
	}
	gzipWriter, err := gzip.NewWriterLevel(tmp, gzip.BestSpeed)
	if err != nil {
		_ = tmp.Close()
		return BackupManifest{}, err
	}
	tarWriter := tar.NewWriter(gzipWriter)
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	manifestData = append(manifestData, '\n')
	if err := writeTarBytes(tarWriter, "manifest.json", manifestData, manifest.CreatedAt); err != nil {
		return BackupManifest{}, closeArchiveOnError(tmp, tarWriter, gzipWriter, err)
	}
	for _, entry := range manifest.Files {
		if err := writeTarFile(tarWriter, root, entry); err != nil {
			return BackupManifest{}, closeArchiveOnError(tmp, tarWriter, gzipWriter, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		_ = gzipWriter.Close()
		_ = tmp.Close()
		return BackupManifest{}, fmt.Errorf("close backup tar: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		_ = tmp.Close()
		return BackupManifest{}, fmt.Errorf("close backup gzip: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return BackupManifest{}, fmt.Errorf("sync backup: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return BackupManifest{}, fmt.Errorf("close backup: %w", err)
	}
	validationParent, err := os.MkdirTemp(filepath.Dir(output), ".pairroom-backup-validate-*")
	if err != nil {
		return BackupManifest{}, fmt.Errorf("create backup validation directory: %w", err)
	}
	validationTarget := filepath.Join(validationParent, "room")
	_, validationErr := Restore(tmpName, validationTarget, false)
	_ = os.RemoveAll(validationParent)
	if validationErr != nil {
		return BackupManifest{}, fmt.Errorf("validate completed backup: %w", validationErr)
	}
	if err := replaceFile(tmpName, output); err != nil {
		return BackupManifest{}, fmt.Errorf("commit backup: %w", err)
	}
	return manifest, nil
}

func collectBackupFiles(root string) ([]manifestFile, error) {
	result := make([]manifestFile, 0, 16)
	for _, rel := range []string{"events.jsonl", "metadata.json"} {
		path := filepath.Join(root, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("backup %s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("backup %s: not a regular file", rel)
		}
		hash, size, err := hashFile(path)
		if err != nil {
			return nil, err
		}
		result = append(result, manifestFile{Path: rel, Size: size, SHA256: hash})
	}
	attachmentFiles, errs, _, _ := verifyAttachments(root, nil)
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "; "))
	}
	for _, value := range attachmentFiles {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result, nil
}

// Restore validates the entire archive before replacing the target data
// directory. It rejects links, traversal, undeclared files, hash mismatches,
// oversized archives, and invalid PairRoom data.
func Restore(input, target string, force bool) (VerifyReport, error) {
	if strings.TrimSpace(input) == "" || strings.TrimSpace(target) == "" {
		return VerifyReport{}, errors.New("restore input and target data directory are required")
	}
	input, err := filepath.Abs(input)
	if err != nil {
		return VerifyReport{}, err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return VerifyReport{}, err
	}
	archiveFile, err := os.Open(input)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("open backup: %w", err)
	}
	defer archiveFile.Close()
	gzipReader, err := gzip.NewReader(archiveFile)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("open backup gzip: %w", err)
	}
	defer gzipReader.Close()

	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return VerifyReport{}, err
	}
	tmp, err := os.MkdirTemp(parent, ".pairroom-restore-*")
	if err != nil {
		return VerifyReport{}, err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o700); err != nil {
		return VerifyReport{}, err
	}

	reader := tar.NewReader(gzipReader)
	var manifest BackupManifest
	seen := make(map[string]manifestFile)
	var total int64
	for count := 0; ; count++ {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return VerifyReport{}, fmt.Errorf("read backup tar: %w", err)
		}
		if count >= maxRestoreFiles {
			return VerifyReport{}, errors.New("backup contains too many files")
		}
		rel, err := safeArchivePath(header.Name)
		if err != nil {
			return VerifyReport{}, err
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return VerifyReport{}, fmt.Errorf("backup entry %q is not a regular file", header.Name)
		}
		if header.Size < 0 || header.Size > maxRestoreBytes-total {
			return VerifyReport{}, errors.New("backup exceeds restore size limit")
		}
		total += header.Size
		if rel == "manifest.json" {
			if manifest.Format != "" {
				return VerifyReport{}, errors.New("backup contains duplicate manifest")
			}
			data, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
			if err != nil || int64(len(data)) != header.Size {
				return VerifyReport{}, errors.New("read backup manifest")
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				return VerifyReport{}, fmt.Errorf("decode backup manifest: %w", err)
			}
			continue
		}
		if _, ok := seen[rel]; ok {
			return VerifyReport{}, fmt.Errorf("duplicate backup entry %q", rel)
		}
		path := filepath.Join(tmp, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return VerifyReport{}, err
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return VerifyReport{}, err
		}
		hash := sha256.New()
		written, copyErr := io.CopyN(io.MultiWriter(out, hash), reader, header.Size)
		closeErr := out.Close()
		if copyErr != nil || written != header.Size {
			return VerifyReport{}, fmt.Errorf("extract %q: %v", rel, copyErr)
		}
		if closeErr != nil {
			return VerifyReport{}, closeErr
		}
		seen[rel] = manifestFile{Path: rel, Size: written, SHA256: hex.EncodeToString(hash.Sum(nil))}
	}
	if manifest.Format != backupFormat || manifest.FormatVersion != backupFormatVersion {
		return VerifyReport{}, fmt.Errorf("unsupported backup format %q version %d", manifest.Format, manifest.FormatVersion)
	}
	declared := make(map[string]manifestFile, len(manifest.Files))
	for _, entry := range manifest.Files {
		rel, err := safeArchivePath(entry.Path)
		if err != nil || rel == "manifest.json" {
			return VerifyReport{}, fmt.Errorf("invalid manifest path %q", entry.Path)
		}
		if _, ok := declared[rel]; ok {
			return VerifyReport{}, fmt.Errorf("duplicate manifest path %q", rel)
		}
		entry.Path = rel
		declared[rel] = entry
	}
	if len(declared) != len(seen) {
		return VerifyReport{}, errors.New("backup file set does not match manifest")
	}
	for rel, want := range declared {
		got, ok := seen[rel]
		if !ok {
			return VerifyReport{}, fmt.Errorf("manifest file %q is missing", rel)
		}
		if got.Size != want.Size || !strings.EqualFold(got.SHA256, want.SHA256) {
			return VerifyReport{}, fmt.Errorf("backup file %q failed integrity check", rel)
		}
	}
	report := Verify(tmp)
	if !report.OK {
		return report, fmt.Errorf("restored data failed verification: %s", strings.Join(report.Errors, "; "))
	}

	if info, statErr := os.Stat(target); statErr == nil {
		if !info.IsDir() {
			return VerifyReport{}, errors.New("restore target exists and is not a directory")
		}
		entries, readErr := os.ReadDir(target)
		if readErr != nil {
			return VerifyReport{}, readErr
		}
		if len(entries) > 0 && !force {
			return VerifyReport{}, errors.New("restore target is not empty; use --force to replace it")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return VerifyReport{}, statErr
	}
	previous := ""
	if _, err := os.Stat(target); err == nil {
		previous = target + ".previous-" + fmt.Sprint(time.Now().UnixNano())
		if err := os.Rename(target, previous); err != nil {
			return VerifyReport{}, fmt.Errorf("stage previous target: %w", err)
		}
	}
	if err := os.Rename(tmp, target); err != nil {
		if previous != "" {
			_ = os.Rename(previous, target)
		}
		return VerifyReport{}, fmt.Errorf("commit restored data: %w", err)
	}
	if previous != "" {
		_ = os.RemoveAll(previous)
	}
	report.DataDir = target
	return report, nil
}

// Diagnostics writes only structural metadata and event headers. Message text,
// runtime payloads, image bytes, credentials, tokens, and browser sessions are
// intentionally excluded.
func Diagnostics(dataDir, output, goos, goarch string) error {
	report := Verify(dataDir)
	events, _ := readEventHeaders(filepath.Join(dataDir, "events.jsonl"), 100)
	summary := diagnosticSummary{
		PairRoomVersion: version.Current, GeneratedAt: time.Now().UTC(), OS: goos, Architecture: goarch,
		Verify: report, EventTail: events,
		Notes: []string{
			"This bundle intentionally excludes message text and native runtime payloads.",
			"Attachments, credentials, browser sessions, and repository contents are not included.",
		},
	}
	data, _ := json.MarshalIndent(summary, "", "  ")
	data = append(data, '\n')
	output, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(output), ".pairroom-diagnostics-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	_ = tmp.Chmod(0o600)
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	if err := writeTarBytes(tw, "diagnostics.json", data, summary.GeneratedAt); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, output)
}

func readEventHeaders(path string, limit int) ([]eventHeader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 128*1024)
	result := make([]eventHeader, 0, limit)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			var event model.Event
			if json.Unmarshal(line, &event) == nil {
				result = append(result, eventHeader{Seq: event.Seq, ID: event.ID, RoomID: event.RoomID, Kind: event.Kind, Actor: event.Actor, CreatedAt: event.CreatedAt})
				if len(result) > limit {
					result = result[len(result)-limit:]
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func decodeStrictFile(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("contains trailing JSON data")
	}
	return nil
}

func hashFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func safeArchivePath(name string) (string, error) {
	name = filepath.ToSlash(strings.TrimSpace(name))
	clean := filepath.ToSlash(filepath.Clean(name))
	if name == "" || clean == "." || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(name) || strings.Contains(name, "\\") {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	return clean, nil
}

func writeTarBytes(writer *tar.Writer, name string, data []byte, modified time.Time) error {
	header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: modified.UTC(), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}

func writeTarFile(writer *tar.Writer, root string, entry manifestFile) error {
	rel, err := safeArchivePath(entry.Path)
	if err != nil {
		return err
	}
	path := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("backup file %q is not regular", rel)
	}
	hash, size, err := hashFile(path)
	if err != nil {
		return err
	}
	if size != entry.Size || !strings.EqualFold(hash, entry.SHA256) {
		return fmt.Errorf("backup source changed during verification: %s", rel)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	header := &tar.Header{Name: rel, Mode: 0o600, Size: size, ModTime: info.ModTime().UTC(), Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	copyHash := sha256.New()
	written, err := io.CopyN(io.MultiWriter(writer, copyHash), file, size)
	if err != nil || written != size {
		return fmt.Errorf("copy backup file %q: %w", rel, err)
	}
	if !strings.EqualFold(hex.EncodeToString(copyHash.Sum(nil)), entry.SHA256) {
		return fmt.Errorf("backup source changed while copying: %s", rel)
	}
	return nil
}

func replaceFile(source, target string) error {
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return os.Rename(source, target)
	} else if err != nil {
		return err
	}
	previous := target + ".previous-" + fmt.Sprint(time.Now().UnixNano())
	if err := os.Rename(target, previous); err != nil {
		return err
	}
	if err := os.Rename(source, target); err != nil {
		_ = os.Rename(previous, target)
		return err
	}
	return os.Remove(previous)
}

func closeArchiveOnError(file *os.File, tarWriter *tar.Writer, gzipWriter *gzip.Writer, cause error) error {
	_ = tarWriter.Close()
	_ = gzipWriter.Close()
	_ = file.Close()
	return cause
}
