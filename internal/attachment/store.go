package attachment

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

const (
	MaxImageBytes       int64 = 5 << 20
	MaxImagesPerMessage       = 8
	MaxTotalImageBytes  int64 = 20 << 20
	MaxImageDimension         = 8_000
	MaxImagePixels      int64 = 64_000_000
)

var (
	attachmentIDPattern = regexp.MustCompile(`^att-[a-f0-9]{24}$`)
	markdownImageRE     = regexp.MustCompile(`!\[[^\]]*\]\(([^)]+)\)`)
	backtickImageRE     = regexp.MustCompile("`([^`\\r\\n]+\\.(?i:png|jpe?g|gif|webp))`")
	bareImageRE         = regexp.MustCompile(`(?i)(?:^|[\s"'(])((?:file://)?(?:[A-Za-z]:[\\/]|/|\./|\.\./)[^\r\n<>"']+?\.(?:png|jpe?g|gif|webp))(?:$|[\s)"',.;:])`)
)

type manifest struct {
	Attachment model.Attachment `json:"attachment"`
	Filename   string           `json:"filename"`
}

type Store struct {
	mu   sync.Mutex
	root string
	repo string
}

func Open(dataDir, repo string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("attachment data directory is required")
	}
	root := filepath.Join(dataDir, "attachments")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create attachment directory: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, fmt.Errorf("secure attachment directory: %w", err)
	}
	resolvedRepo := ""
	if repo != "" {
		absolute, err := filepath.Abs(repo)
		if err != nil {
			return nil, fmt.Errorf("resolve attachment repository: %w", err)
		}
		resolvedRepo = filepath.Clean(absolute)
		if evaluated, evalErr := filepath.EvalSymlinks(resolvedRepo); evalErr == nil {
			resolvedRepo = filepath.Clean(evaluated)
		}
	}
	return &Store{root: root, repo: resolvedRepo}, nil
}

func (s *Store) Root() string { return s.root }

func (s *Store) SaveImage(name string, reader io.Reader, source string) (model.Attachment, error) {
	if reader == nil {
		return model.Attachment{}, errors.New("image reader is required")
	}
	if strings.TrimSpace(source) == "" {
		source = "upload"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tmp, err := os.CreateTemp(s.root, ".image-*.tmp")
	if err != nil {
		return model.Attachment{}, fmt.Errorf("create attachment temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return model.Attachment{}, fmt.Errorf("secure attachment temp file: %w", err)
	}

	hash := sha256.New()
	limited := io.LimitReader(reader, MaxImageBytes+1)
	size, copyErr := io.Copy(io.MultiWriter(tmp, hash), limited)
	if copyErr != nil {
		_ = tmp.Close()
		return model.Attachment{}, fmt.Errorf("store image: %w", copyErr)
	}
	if size == 0 {
		_ = tmp.Close()
		return model.Attachment{}, errors.New("image is empty")
	}
	if size > MaxImageBytes {
		_ = tmp.Close()
		return model.Attachment{}, fmt.Errorf("image exceeds %d MiB limit", MaxImageBytes>>20)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return model.Attachment{}, fmt.Errorf("sync image: %w", err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		_ = tmp.Close()
		return model.Attachment{}, fmt.Errorf("rewind image: %w", err)
	}

	header := make([]byte, 512)
	n, readErr := io.ReadFull(tmp, header)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		_ = tmp.Close()
		return model.Attachment{}, fmt.Errorf("inspect image: %w", readErr)
	}
	mediaType := canonicalImageType(http.DetectContentType(header[:n]))
	if mediaType == "" {
		_ = tmp.Close()
		return model.Attachment{}, errors.New("only PNG, JPEG, GIF, and WebP images are supported")
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		_ = tmp.Close()
		return model.Attachment{}, fmt.Errorf("rewind image for metadata: %w", err)
	}
	width, height, err := decodeImageMetadata(tmp, mediaType, size)
	if err != nil {
		_ = tmp.Close()
		return model.Attachment{}, fmt.Errorf("decode image metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return model.Attachment{}, fmt.Errorf("close image temp file: %w", err)
	}

	id := model.NewID("att")
	ext := extensionForType(mediaType)
	filename := id + ext
	path := filepath.Join(s.root, filename)
	if err := os.Rename(tmpPath, path); err != nil {
		return model.Attachment{}, fmt.Errorf("commit image: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.Remove(path)
		return model.Attachment{}, fmt.Errorf("secure image: %w", err)
	}

	attachment := model.Attachment{
		ID:        id,
		Name:      safeDisplayName(name, mediaType),
		MediaType: mediaType,
		Kind:      "image",
		Size:      size,
		SHA256:    hex.EncodeToString(hash.Sum(nil)),
		Width:     width,
		Height:    height,
		Source:    source,
		CreatedAt: time.Now().UTC(),
	}
	if err := writeManifest(filepath.Join(s.root, id+".json"), manifest{Attachment: attachment, Filename: filename}); err != nil {
		_ = os.Remove(path)
		return model.Attachment{}, err
	}
	return attachment, nil
}

func (s *Store) ImportRepoImage(path, source string) (model.Attachment, error) {
	resolved, err := s.resolveRepoPath(path)
	if err != nil {
		return model.Attachment{}, err
	}
	file, err := os.Open(resolved)
	if err != nil {
		return model.Attachment{}, fmt.Errorf("open repository image: %w", err)
	}
	defer file.Close()
	return s.SaveImage(filepath.Base(resolved), file, source)
}

func (s *Store) ResolveMany(ids []string) ([]model.Attachment, error) {
	seen := make(map[string]struct{}, len(ids))
	result := make([]model.Attachment, 0, len(ids))
	var total int64
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if len(result) >= MaxImagesPerMessage {
			return nil, fmt.Errorf("a message can include at most %d images", MaxImagesPerMessage)
		}
		meta, _, err := s.Resolve(id)
		if err != nil {
			return nil, err
		}
		total += meta.Size
		if total > MaxTotalImageBytes {
			return nil, fmt.Errorf("message images exceed %d MiB total limit", MaxTotalImageBytes>>20)
		}
		result = append(result, meta)
	}
	return result, nil
}

// Resolve returns canonical metadata and the host-local content path for one
// opaque attachment ID. The path is only passed to native harness adapters.
func (s *Store) Resolve(id string) (model.Attachment, string, error) {
	return s.load(strings.TrimSpace(id))
}

func (s *Store) AgentAttachments(values []model.Attachment) []model.AgentAttachment {
	result := make([]model.AgentAttachment, 0, len(values))
	for _, value := range values {
		meta, path, err := s.Resolve(value.ID)
		if err != nil {
			continue
		}
		result = append(result, model.AgentAttachment{Attachment: meta, Path: path})
	}
	return result
}

func (s *Store) OpenFile(id string) (model.Attachment, *os.File, error) {
	meta, path, err := s.Resolve(id)
	if err != nil {
		return model.Attachment{}, nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return model.Attachment{}, nil, fmt.Errorf("open attachment: %w", err)
	}
	return meta, file, nil
}

func (s *Store) DiscoverRepoImages(text, source string) []model.Attachment {
	if s.repo == "" || strings.TrimSpace(text) == "" {
		return nil
	}
	candidates := extractImageCandidates(text)
	result := make([]model.Attachment, 0, min(len(candidates), MaxImagesPerMessage))
	seenPath := make(map[string]struct{}, len(candidates))
	seenHash := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if len(result) >= MaxImagesPerMessage {
			break
		}
		resolved, err := s.resolveRepoPath(candidate)
		if err != nil {
			continue
		}
		key := resolved
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if _, ok := seenPath[key]; ok {
			continue
		}
		seenPath[key] = struct{}{}
		attachment, err := s.ImportRepoImage(resolved, source)
		if err != nil {
			continue
		}
		if _, ok := seenHash[attachment.SHA256]; ok {
			_ = s.Remove(attachment.ID)
			continue
		}
		seenHash[attachment.SHA256] = struct{}{}
		result = append(result, attachment)
	}
	return result
}

func (s *Store) Remove(id string) error {
	m, path, err := s.load(id)
	if err != nil {
		return err
	}
	_ = m
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(filepath.Join(s.root, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) load(id string) (model.Attachment, string, error) {
	if !attachmentIDPattern.MatchString(id) {
		return model.Attachment{}, "", errors.New("invalid attachment id")
	}
	data, err := os.ReadFile(filepath.Join(s.root, id+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return model.Attachment{}, "", fmt.Errorf("unknown attachment %q", id)
		}
		return model.Attachment{}, "", fmt.Errorf("read attachment metadata: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return model.Attachment{}, "", fmt.Errorf("decode attachment metadata: %w", err)
	}
	if m.Attachment.ID != id || m.Attachment.Kind != "image" || canonicalImageType(m.Attachment.MediaType) == "" || filepath.Base(m.Filename) != m.Filename || m.Filename == "" {
		return model.Attachment{}, "", errors.New("invalid attachment metadata")
	}
	path := filepath.Join(s.root, m.Filename)
	info, err := os.Lstat(path)
	if err != nil {
		return model.Attachment{}, "", fmt.Errorf("stat attachment: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != m.Attachment.Size || info.Size() <= 0 || info.Size() > MaxImageBytes {
		return model.Attachment{}, "", errors.New("attachment content is invalid")
	}
	if err := validateImageDimensions(m.Attachment.Width, m.Attachment.Height); err != nil {
		return model.Attachment{}, "", fmt.Errorf("attachment metadata is invalid: %w", err)
	}
	// Attachments are immutable transcript artifacts. Verify the content hash on
	// every boundary crossing so a same-size local modification cannot silently
	// change what the browser or either native harness receives.
	file, err := os.Open(path)
	if err != nil {
		return model.Attachment{}, "", fmt.Errorf("open attachment content: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, io.LimitReader(file, MaxImageBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return model.Attachment{}, "", fmt.Errorf("verify attachment content: %w", copyErr)
	}
	if closeErr != nil {
		return model.Attachment{}, "", fmt.Errorf("close attachment content: %w", closeErr)
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), m.Attachment.SHA256) {
		return model.Attachment{}, "", errors.New("attachment content hash does not match its transcript metadata")
	}
	return m.Attachment, path, nil
}

func decodeImageMetadata(file io.ReadSeeker, mediaType string, size int64) (int, int, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return 0, 0, err
	}
	var width, height int
	if mediaType == "image/webp" {
		data, err := io.ReadAll(io.LimitReader(file, MaxImageBytes+1))
		if err != nil {
			return 0, 0, err
		}
		if int64(len(data)) != size {
			return 0, 0, errors.New("image changed while reading metadata")
		}
		width, height, err = webPDimensions(data)
		if err != nil {
			return 0, 0, err
		}
	} else {
		cfg, detected, err := image.DecodeConfig(file)
		if err != nil {
			return 0, 0, err
		}
		if canonicalImageType("image/"+detected) != mediaType {
			return 0, 0, fmt.Errorf("detected %q content does not match %q", detected, mediaType)
		}
		width, height = cfg.Width, cfg.Height
	}
	if err := validateImageDimensions(width, height); err != nil {
		return 0, 0, err
	}
	return width, height, nil
}

func validateImageDimensions(width, height int) error {
	if width <= 0 || height <= 0 {
		return errors.New("image dimensions must be positive")
	}
	if width > MaxImageDimension || height > MaxImageDimension {
		return fmt.Errorf("image dimensions exceed %d pixels per side", MaxImageDimension)
	}
	if int64(width)*int64(height) > MaxImagePixels {
		return fmt.Errorf("image dimensions exceed %d megapixels", MaxImagePixels/1_000_000)
	}
	return nil
}

func webPDimensions(data []byte) (int, int, error) {
	if len(data) < 20 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, errors.New("invalid WebP container")
	}
	containerSize := int64(binary.LittleEndian.Uint32(data[4:8])) + 8
	if containerSize < 20 || containerSize > int64(len(data)) {
		return 0, 0, errors.New("truncated WebP container")
	}
	chunkSize := int64(binary.LittleEndian.Uint32(data[16:20]))
	if chunkSize < 0 || 20+chunkSize > containerSize {
		return 0, 0, errors.New("truncated WebP image chunk")
	}
	switch string(data[12:16]) {
	case "VP8X":
		if chunkSize < 10 || len(data) < 30 {
			return 0, 0, errors.New("invalid VP8X header")
		}
		width := 1 + int(data[24]) + int(data[25])<<8 + int(data[26])<<16
		height := 1 + int(data[27]) + int(data[28])<<8 + int(data[29])<<16
		return width, height, nil
	case "VP8L":
		if chunkSize < 5 || len(data) < 25 || data[20] != 0x2f {
			return 0, 0, errors.New("invalid VP8L header")
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		return 1 + int(bits&0x3fff), 1 + int((bits>>14)&0x3fff), nil
	case "VP8 ":
		if chunkSize < 10 || len(data) < 30 || data[23] != 0x9d || data[24] != 0x01 || data[25] != 0x2a {
			return 0, 0, errors.New("invalid VP8 frame header")
		}
		width := int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff)
		height := int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff)
		return width, height, nil
	default:
		return 0, 0, fmt.Errorf("unsupported WebP chunk %q", data[12:16])
	}
}

func (s *Store) resolveRepoPath(value string) (string, error) {
	if s.repo == "" {
		return "", errors.New("repository is unavailable")
	}
	value = strings.TrimSpace(strings.Trim(value, "<>\"'`"))
	if value == "" {
		return "", errors.New("image path is empty")
	}
	if strings.HasPrefix(strings.ToLower(value), "file://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", err
		}
		value = parsed.Path
		if runtime.GOOS == "windows" && len(value) >= 3 && value[0] == '/' && value[2] == ':' {
			value = value[1:]
		}
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "data:") {
		return "", errors.New("remote images are not repository files")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(s.repo, filepath.FromSlash(value))
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	resolved := absolute
	if evaluated, err := filepath.EvalSymlinks(absolute); err == nil {
		resolved = evaluated
	}
	relative, err := filepath.Rel(s.repo, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", errors.New("image path is outside the repository")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("image path is not a regular file")
	}
	return resolved, nil
}

func extractImageCandidates(text string) []string {
	seen := make(map[string]struct{})
	var result []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		// Markdown destinations can include an optional quoted title.
		if index := strings.LastIndex(value, ` "`); index > 0 && strings.HasSuffix(value, `"`) {
			value = value[:index]
		}
		value = strings.Trim(value, "<>\"'` ")
		if decoded, err := url.PathUnescape(value); err == nil {
			value = decoded
		}
		lower := strings.ToLower(value)
		if !strings.HasSuffix(lower, ".png") && !strings.HasSuffix(lower, ".jpg") && !strings.HasSuffix(lower, ".jpeg") && !strings.HasSuffix(lower, ".gif") && !strings.HasSuffix(lower, ".webp") {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	for _, match := range markdownImageRE.FindAllStringSubmatch(text, -1) {
		if len(match) > 1 {
			add(match[1])
		}
	}
	for _, match := range backtickImageRE.FindAllStringSubmatch(text, -1) {
		if len(match) > 1 {
			add(match[1])
		}
	}
	for _, match := range bareImageRE.FindAllStringSubmatch(text, -1) {
		if len(match) > 1 {
			add(match[1])
		}
	}
	sort.Strings(result)
	return result
}

func writeManifest(path string, value manifest) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode attachment metadata: %w", err)
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".metadata-*.tmp")
	if err != nil {
		return fmt.Errorf("create attachment metadata: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("commit attachment metadata: %w", err)
	}
	return nil
}

func canonicalImageType(value string) string {
	value, _, _ = mime.ParseMediaType(value)
	switch strings.ToLower(value) {
	case "image/png":
		return "image/png"
	case "image/jpeg":
		return "image/jpeg"
	case "image/gif":
		return "image/gif"
	case "image/webp":
		return "image/webp"
	default:
		return ""
	}
}

func extensionForType(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".bin"
	}
}

func safeDisplayName(name, mediaType string) string {
	name = strings.TrimSpace(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	if name == "" || name == "." {
		name = "image" + extensionForType(mediaType)
	}
	var b bytes.Buffer
	for _, r := range name {
		if r < 32 || r == 127 {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= 180 {
			break
		}
	}
	result := strings.TrimSpace(b.String())
	if result == "" {
		return "image" + extensionForType(mediaType)
	}
	return result
}
