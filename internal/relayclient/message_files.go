package relayclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

const (
	maxFileReferences = 16
	maxReferenceBytes = 64 << 20
)

// publicationInput keeps file contents out of tool arguments and receipts.
// A reference is deliberately a local pointer, not a second attachment store.
type publicationInput struct {
	text       string
	textSet    bool
	textFile   string
	references []string
}

type localFileReference struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

func readPublicationInput(input publicationInput, in io.Reader, limit int64) (string, error) {
	if input.textSet && input.textFile != "" {
		return "", errors.New("choose --text or --text-file, not both")
	}
	if len(input.references) > maxFileReferences {
		return "", fmt.Errorf("at most %d --ref files per publication", maxFileReferences)
	}
	text := input.text
	var source io.Reader
	switch {
	case input.textFile == "-":
		source = in
	case input.textFile != "":
		f, _, _, err := openRegularMessageFile(input.textFile)
		if err != nil {
			return "", err
		}
		defer f.Close()
		source = f
	case !input.textSet && text == "" && len(input.references) == 0:
		source = in
	}
	if source != nil {
		data, err := io.ReadAll(io.LimitReader(source, limit+1))
		if err != nil {
			return "", fmt.Errorf("read message body: %w", err)
		}
		text = string(data)
	}
	if int64(len(text)) > limit {
		return "", fmt.Errorf("message exceeds %d KiB", limit>>10)
	}
	// encoding/json otherwise silently replaces invalid UTF-8 on the wire.
	if !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') {
		return "", errors.New("message body must be UTF-8 text without NUL bytes; use --ref for binary files")
	}
	refs := make([]localFileReference, 0, len(input.references))
	seen := make(map[string]bool, len(input.references))
	for _, path := range input.references {
		if seen[path] {
			continue
		}
		ref, err := describeLocalFile(path)
		if err != nil {
			return "", err
		}
		if !seen[ref.Path] {
			refs = append(refs, ref)
			seen[ref.Path] = true
		}
		seen[path] = true
	}
	if len(refs) > 0 {
		data, err := json.Marshal(refs)
		if err != nil {
			return "", err
		}
		if text != "" {
			text += "\n\n"
		}
		text += "Local file references (not uploaded; verify SHA-256 and read only needed sections):\n" + string(data)
	}
	if int64(len(text)) > limit {
		return "", fmt.Errorf("message with file references exceeds %d KiB", limit>>10)
	}
	return text, nil
}

// Resolve aliases once and reject special files before opening them: an
// accidental FIFO must not hang a native tool call waiting for another writer.
func openRegularMessageFile(path string) (*os.File, os.FileInfo, string, error) {
	if path == "" {
		return nil, nil, "", errors.New("message file path must not be empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, "", err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, nil, "", fmt.Errorf("resolve message file: %w", err)
	}
	if !utf8.ValidString(absolute) {
		return nil, nil, "", errors.New("message file path must be valid UTF-8")
	}
	before, err := os.Lstat(absolute)
	if err != nil {
		return nil, nil, "", err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, "", errors.New("message file must be a regular file")
	}
	f, err := os.Open(absolute)
	if err != nil {
		return nil, nil, "", err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(before, info) {
		_ = f.Close()
		return nil, nil, "", errors.New("message file changed while opening; retry after the writer finishes")
	}
	return f, info, absolute, nil
}

func describeLocalFile(path string) (localFileReference, error) {
	f, before, absolute, err := openRegularMessageFile(path)
	if err != nil {
		return localFileReference{}, err
	}
	defer f.Close()
	if before.Size() > maxReferenceBytes {
		return localFileReference{}, errors.New("--ref file exceeds 64 MiB; reference a smaller relevant extract")
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(f, maxReferenceBytes+1))
	if err != nil {
		return localFileReference{}, fmt.Errorf("hash referenced file: %w", err)
	}
	after, err := f.Stat()
	if err != nil || n > maxReferenceBytes || n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return localFileReference{}, errors.New("referenced file changed while hashing; retry after the writer finishes")
	}
	return localFileReference{Path: absolute, Bytes: n, SHA256: hex.EncodeToString(digest.Sum(nil))}, nil
}

// envelopeFileWriter persists the complete incoming envelope before exposing a
// small stdout receipt. deliverOnce can acknowledge only after Write succeeds.
// No file is created for an empty poll, and no existing path is overwritten.
type envelopeFileWriter struct {
	path string
	out  io.Writer
	used bool
}

type envelopeFileReceipt struct {
	EnvelopeFile string `json:"envelope_file"`
	Bytes        int    `json:"bytes"`
	SHA256       string `json:"sha256"`
	Notice       string `json:"notice"`
}

func newEnvelopeFileWriter(path string, out io.Writer) (*envelopeFileWriter, error) {
	if path == "" {
		return nil, errors.New("--output-file requires a nonempty path")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return nil, fmt.Errorf("--output-file parent must already exist: %w", err)
	}
	info, err := os.Stat(parent)
	if err != nil || !info.IsDir() {
		return nil, errors.New("--output-file parent must be a directory")
	}
	absolute = filepath.Join(parent, filepath.Base(absolute))
	if !utf8.ValidString(absolute) {
		return nil, errors.New("--output-file path must be valid UTF-8")
	}
	if _, err := os.Lstat(absolute); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = os.ErrExist
		}
		return nil, fmt.Errorf("--output-file must be a new file: %w", err)
	}
	return &envelopeFileWriter{path: absolute, out: out}, nil
}

func (w *envelopeFileWriter) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if w.used {
		return 0, errors.New("--output-file accepts one complete envelope only")
	}
	w.used = true
	if err := persistEnvelopeFile(w.path, data); err != nil {
		return 0, err
	}
	digest := sha256.Sum256(data)
	receipt, err := json.Marshal(envelopeFileReceipt{
		EnvelopeFile: w.path,
		Bytes:        len(data),
		SHA256:       hex.EncodeToString(digest[:]),
		Notice:       "Read envelope_file before acting.",
	})
	if err == nil {
		receipt = append(receipt, '\n')
		var n int
		n, err = w.out.Write(receipt)
		if err == nil && n != len(receipt) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		// Preserve the complete private file for explicit recovery, but withhold
		// ack when the model/tool runner did not receive its locator.
		return 0, fmt.Errorf("envelope saved to %q but receipt output failed: %w; acknowledgement withheld", w.path, err)
	}
	return len(data), nil
}

func persistEnvelopeFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create envelope file without overwriting: %w", err)
	}
	complete := false
	defer func() {
		_ = f.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		return fmt.Errorf("persist envelope file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close envelope file: %w", err)
	}
	// Match relay.AtomicJSON's directory durability boundary. Windows does
	// not support directory Sync; its destination ACLs are inherited.
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		err = dir.Sync()
		closeErr := dir.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	complete = true
	return nil
}
