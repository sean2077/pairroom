package relayclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type unreadMessageInput struct{ t *testing.T }

func (r unreadMessageInput) Read([]byte) (int, error) {
	r.t.Error("unexpected stdin read (would block a native tool)")
	return 0, errors.New("stdin must not be consumed")
}

func messageTestFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPublicationFileSources(t *testing.T) {
	body := strings.Repeat("review evidence\n", 8192)
	path := messageTestFile(t, "long review.md", body)
	for _, tc := range []struct {
		name  string
		input publicationInput
		stdin io.Reader
		want  string
	}{
		{"file", publicationInput{textFile: path}, unreadMessageInput{t}, body},
		{"stdin", publicationInput{}, strings.NewReader(body), body},
		{"explicit stdin", publicationInput{textFile: "-"}, strings.NewReader(body), body},
		{"text", publicationInput{text: body, textSet: true}, unreadMessageInput{t}, body},
		{"empty text", publicationInput{textSet: true}, unreadMessageInput{t}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readPublicationInput(tc.input, tc.stdin, 256<<10)
			if err != nil || got != tc.want {
				t.Fatalf("body mismatch: got %d bytes, err %v", len(got), err)
			}
		})
	}
}

func TestPublicationInputValidation(t *testing.T) {
	for _, body := range []string{strings.Repeat("x", (256<<10)+1), "a\xffb", "a\x00b"} {
		path := messageTestFile(t, "invalid.txt", body)
		for _, input := range []publicationInput{{text: body, textSet: true}, {textFile: path}, {}} {
			if _, err := readPublicationInput(input, strings.NewReader(body), 256<<10); err == nil {
				t.Fatal("accepted oversized or non-text input")
			}
		}
	}
	limit := strings.Repeat("x", 256<<10)
	if got, err := readPublicationInput(publicationInput{text: limit}, unreadMessageInput{t}, 256<<10); err != nil || got != limit {
		t.Fatalf("exact byte limit: %v", err)
	}
	if _, err := readPublicationInput(publicationInput{textSet: true, textFile: "-"}, unreadMessageInput{t}, 256<<10); err == nil {
		t.Fatal("accepted conflicting explicit body sources")
	}
	for _, path := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		if _, err := readPublicationInput(publicationInput{textFile: path}, unreadMessageInput{t}, 256<<10); err == nil {
			t.Fatal("accepted a non-file body source")
		}
	}
}

func parseMessageReferences(t *testing.T, body string) []localFileReference {
	t.Helper()
	start := strings.Index(body, "\n[")
	if start < 0 {
		t.Fatal("missing reference manifest")
	}
	var refs []localFileReference
	if err := json.Unmarshal([]byte(body[start+1:]), &refs); err != nil {
		t.Fatal(err)
	}
	return refs
}

func TestPublicationReferencesAreBoundedLocalPointers(t *testing.T) {
	body := strings.Repeat("report-content-must-not-be-in-manifest\n", 8192)
	path := messageTestFile(t, "review report.md", body)
	input := publicationInput{references: []string{path, path}}
	text, err := readPublicationInput(input, unreadMessageInput{t}, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > 1024 || strings.Contains(text, "report-content-must-not-be-in-manifest") {
		t.Fatal("reference expanded or echoed the file body")
	}
	refs := parseMessageReferences(t, text)
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(body))
	if len(refs) != 1 || refs[0].Path != canonical || refs[0].Bytes != int64(len(body)) || refs[0].SHA256 != hex.EncodeToString(hash[:]) {
		t.Fatalf("incorrect reference metadata: %+v", refs)
	}
	again, err := readPublicationInput(input, unreadMessageInput{t}, 256<<10)
	if err != nil || again != text {
		t.Fatal("unchanged references must produce the same idempotent body")
	}
	if err := os.WriteFile(path, []byte("updated evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := readPublicationInput(input, unreadMessageInput{t}, 256<<10)
	if err != nil || changed == text {
		t.Fatal("changed references must change the publication identity")
	}
}

func TestPublicationReferenceLimitsAndEscaping(t *testing.T) {
	path := messageTestFile(t, "evidence.bin", "\x00\xff")
	if _, err := readPublicationInput(publicationInput{references: []string{path}}, unreadMessageInput{t}, 256<<10); err != nil {
		t.Fatalf("binary references should not expand bytes as text: %v", err)
	}
	if _, err := readPublicationInput(publicationInput{references: make([]string, maxFileReferences+1)}, unreadMessageInput{t}, 256<<10); err == nil {
		t.Fatal("accepted too many references")
	}
	if _, err := readPublicationInput(publicationInput{references: []string{""}}, unreadMessageInput{t}, 256<<10); err == nil {
		t.Fatal("accepted an empty reference")
	}
	if _, err := readPublicationInput(publicationInput{text: strings.Repeat("x", 256<<10), references: []string{path}}, unreadMessageInput{t}, 256<<10); err == nil {
		t.Fatal("reference manifest bypassed the wire body limit")
	}
	large, err := os.Create(filepath.Join(t.TempDir(), "large.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(maxReferenceBytes + 1); err != nil {
		_ = large.Close()
		t.Fatal(err)
	}
	_ = large.Close()
	if _, err := describeLocalFile(large.Name()); err == nil {
		t.Fatal("accepted an oversized reference")
	}
	if runtime.GOOS != "windows" {
		odd := messageTestFile(t, "quote\"and\nnewline.txt", "payload")
		text, err := readPublicationInput(publicationInput{references: []string{odd}}, unreadMessageInput{t}, 256<<10)
		if err != nil {
			t.Fatal(err)
		}
		refs := parseMessageReferences(t, text)
		canonical, err := filepath.EvalSymlinks(odd)
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != 1 || refs[0].Path != canonical {
			t.Fatal("path did not survive JSON escaping")
		}
	}
}

func TestMessageFileResolvesSymlink(t *testing.T) {
	path := messageTestFile(t, "original.txt", "evidence")
	link := filepath.Join(t.TempDir(), "alias.txt")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	text, err := readPublicationInput(publicationInput{references: []string{path, link}}, unreadMessageInput{t}, 256<<10)
	if err != nil {
		t.Fatal(err)
	}
	if len(parseMessageReferences(t, text)) != 1 {
		t.Fatal("symlink aliases were not deduplicated")
	}
}

func TestEnvelopeFileReceiptDoesNotEchoBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incoming.txt")
	body := []byte(strings.Repeat("incoming review evidence\n", 8192))
	var out bytes.Buffer
	writer, err := newEnvelopeFileWriter(path, &out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty polls must not create output files")
	}
	if n, err := writer.Write(body); err != nil || n != len(body) {
		t.Fatalf("write: %d, %v", n, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, body) {
		t.Fatal("file did not preserve the full envelope")
	}
	var receipt envelopeFileReceipt
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	if receipt.EnvelopeFile != writer.path || receipt.Bytes != len(body) || receipt.SHA256 != hex.EncodeToString(hash[:]) {
		t.Fatalf("incorrect file receipt: %+v", receipt)
	}
	if out.Len() > 1024 || strings.Contains(out.String(), "incoming review evidence") {
		t.Fatal("receipt leaked the full envelope")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("envelope file must be owner-only")
		}
	}
	if _, err := writer.Write(body); err == nil {
		t.Fatal("accepted a second envelope for the same output file")
	}
}

type shortFileReceiptWriter struct{}

func (shortFileReceiptWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestEnvelopeFileReceiptFailurePreservesRecoveryCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incoming.txt")
	writer, err := newEnvelopeFileWriter(path, shortFileReceiptWriter{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("complete envelope\n")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("must surface short receipt writes: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "complete envelope\n" {
		t.Fatal("lost the recovery file after stdout failure")
	}
}

func TestEnvelopeFileNeverClobbersExistingPaths(t *testing.T) {
	existing := messageTestFile(t, "existing.txt", "keep me")
	for _, path := range []string{"", existing, t.TempDir(), filepath.Join(t.TempDir(), "missing", "child")} {
		if _, err := newEnvelopeFileWriter(path, io.Discard); err == nil {
			t.Fatal("accepted an invalid or existing output path")
		}
	}
	path := filepath.Join(t.TempDir(), "raced.txt")
	writer, err := newEnvelopeFileWriter(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("someone else's file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("incoming")); err == nil {
		t.Fatal("overwrote a file created after preflight")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "someone else's file" {
		t.Fatal("changed or removed someone else's file")
	}
}
