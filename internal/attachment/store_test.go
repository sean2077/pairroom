package attachment

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

func pngBytes(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSaveResolveAndOpenImage(t *testing.T) {
	dataDir := t.TempDir()
	repo := t.TempDir()
	store, err := Open(dataDir, repo)
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := store.SaveImage("screen.png", bytes.NewReader(pngBytes(t)), "upload")
	if err != nil {
		t.Fatal(err)
	}
	if attachment.MediaType != "image/png" || attachment.Width != 1 || attachment.Height != 1 {
		t.Fatalf("unexpected metadata: %#v", attachment)
	}
	resolved, err := store.ResolveMany([]string{attachment.ID, attachment.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 {
		t.Fatalf("expected deduplicated attachment, got %d", len(resolved))
	}
	meta, file, err := store.OpenFile(attachment.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if meta.SHA256 != attachment.SHA256 {
		t.Fatalf("metadata mismatch")
	}
}

func TestRejectsNonImageAndOversizedImage(t *testing.T) {
	store, err := Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveImage("bad.txt", strings.NewReader("not an image"), "upload"); err == nil {
		t.Fatal("expected non-image rejection")
	}
	large := ioLimitPattern{remaining: MaxImageBytes + 1}
	if _, err := store.SaveImage("large.png", &large, "upload"); err == nil {
		t.Fatal("expected oversized image rejection")
	}
}

func TestRejectsSignatureOnlyAndExcessiveDimensions(t *testing.T) {
	store, err := Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	truncatedPNG := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3}
	if _, err := store.SaveImage("truncated.png", bytes.NewReader(truncatedPNG), "upload"); err == nil {
		t.Fatal("expected truncated PNG rejection")
	}
	if err := validateImageDimensions(MaxImageDimension+1, 1); err == nil {
		t.Fatal("expected excessive dimension rejection")
	}
	if err := validateImageDimensions(20_000, 20_000); err == nil {
		t.Fatal("expected excessive pixel count rejection")
	}
}

func TestWebPDimensionsParsesExtendedHeader(t *testing.T) {
	data := make([]byte, 30)
	copy(data[:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8X")
	binary.LittleEndian.PutUint32(data[16:20], 10)
	width, height := 640, 480
	data[24] = byte(width - 1)
	data[25] = byte((width - 1) >> 8)
	data[26] = byte((width - 1) >> 16)
	data[27] = byte(height - 1)
	data[28] = byte((height - 1) >> 8)
	data[29] = byte((height - 1) >> 16)
	gotWidth, gotHeight, err := webPDimensions(data)
	if err != nil {
		t.Fatal(err)
	}
	if gotWidth != width || gotHeight != height {
		t.Fatalf("unexpected WebP dimensions: %dx%d", gotWidth, gotHeight)
	}
}

type ioLimitPattern struct{ remaining int64 }

func (r *ioLimitPattern) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, os.ErrClosed
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = 'x'
	}
	r.remaining -= int64(len(p))
	return len(p), nil
}

func TestDiscoverRepoImagesStaysInsideRepo(t *testing.T) {
	dataDir := t.TempDir()
	repo := t.TempDir()
	inside := filepath.Join(repo, "docs", "screen.png")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, pngBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.png")
	if err := os.WriteFile(outside, pngBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dataDir, repo)
	if err != nil {
		t.Fatal(err)
	}
	text := "Generated ![preview](docs/screen.png) and `" + outside + "`."
	found := store.DiscoverRepoImages(text, "agent-artifact")
	if len(found) != 1 || found[0].Name != "screen.png" {
		t.Fatalf("unexpected discovered images: %#v", found)
	}
}

func TestResolveRejectsInvalidID(t *testing.T) {
	store, err := Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Resolve("../../etc/passwd"); err == nil {
		t.Fatal("expected invalid id rejection")
	}
}

func TestResolveRejectsSameSizeContentTampering(t *testing.T) {
	store, err := Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := store.SaveImage("screen.png", bytes.NewReader(pngBytes(t)), "upload")
	if err != nil {
		t.Fatal(err)
	}
	_, path, err := store.Resolve(attachment.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Resolve(attachment.ID); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatalf("expected content-hash rejection, got %v", err)
	}
}

func TestResolveRejectsSymlinkedAttachmentContent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated Windows privileges")
	}
	dataDir := t.TempDir()
	store, err := Open(dataDir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := store.SaveImage("screen.png", bytes.NewReader(pngBytes(t)), "upload")
	if err != nil {
		t.Fatal(err)
	}
	_, path, err := store.Resolve(attachment.ID)
	if err != nil {
		t.Fatal(err)
	}
	original := path + ".original"
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(original, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Resolve(attachment.ID); err == nil {
		t.Fatal("expected symlinked attachment rejection")
	}
}
