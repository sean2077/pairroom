package access

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/daemon"
)

const (
	desktopURLVariable = "PAIRROOM_DESKTOP_URL"
	maximumLogBackups  = 32
	maximumProbeBody   = 32 << 20
)

// Access is the authenticated local Management Shell endpoint exposed to the
// desktop host. The bootstrap token is kept in memory and remains in the URL
// fragment, so it is never sent as part of a browser request.
type Access struct {
	BrowserURL string
	APIURL     string
	Token      string
}

// Parse accepts only an authenticated numeric-loopback PairRoom Management URL.
func Parse(raw string) (Access, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Access{}, errors.New("Management URL could not be parsed")
	}
	if parsed.Scheme != "http" || parsed.User != nil || parsed.ForceQuery || parsed.RawQuery != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return Access{}, errors.New("Management URL must be plain HTTP at the root path with no userinfo or query")
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		return Access{}, errors.New("Management URL must include an explicit port")
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	portNumber, portErr := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return Access{}, errors.New("Management URL listener must be a numeric loopback address")
	}
	fragment, err := url.ParseQuery(parsed.Fragment)
	tokens := fragment["token"]
	if err != nil || len(fragment) != 1 || len(tokens) != 1 || strings.TrimSpace(tokens[0]) == "" {
		return Access{}, errors.New("Management URL fragment must contain exactly one non-empty token")
	}
	token := strings.TrimSpace(tokens[0])
	browser := url.URL{Scheme: "http", Host: parsed.Host, Path: "/"}
	values := url.Values{}
	values.Set("token", token)
	browser.Fragment = values.Encode()
	api := url.URL{Scheme: "http", Host: parsed.Host, Path: "/api/v1/service"}
	return Access{BrowserURL: browser.String(), APIURL: api.String(), Token: token}, nil
}

// DesktopURL marks the Management Shell as hosted by the native desktop window.
// The bearer bootstrap remains a fragment and therefore never reaches the HTTP
// server in a request target.
func (a Access) DesktopURL() string {
	parsed, err := url.Parse(a.BrowserURL)
	if err != nil {
		return a.BrowserURL
	}
	query := parsed.Query()
	query.Set("desktop", "1")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// Probe confirms that Access authenticates the running PairRoom Service.
func Probe(ctx context.Context, access Access) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, access.APIURL, nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+access.Token)
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			Proxy:             nil,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	return response.StatusCode == http.StatusOK
}

// FromEnvironment validates an explicit desktop endpoint override.
func FromEnvironment(ctx context.Context) (Access, bool, error) {
	raw := strings.TrimSpace(os.Getenv(desktopURLVariable))
	if raw == "" {
		return Access{}, false, nil
	}
	access, err := Parse(raw)
	if err != nil {
		return Access{}, false, fmt.Errorf("%s: %w", desktopURLVariable, err)
	}
	if !Probe(ctx, access) {
		return Access{}, false, fmt.Errorf("%s did not authenticate a running numeric-loopback PairRoom Service", desktopURLVariable)
	}
	return access, true, nil
}

// DiscoverDaemon reuses the installed PairRoom daemon without duplicating its
// lifecycle or taking ownership of it. Every log candidate is parsed and
// authenticated before it is returned.
func DiscoverDaemon(ctx context.Context) (Access, bool, error) {
	meta, err := daemon.LoadMeta()
	if err != nil {
		return Access{}, false, err
	}
	logFile, err := resolveDaemonLogFile(meta)
	if err != nil {
		return Access{}, false, err
	}
	backups := meta.LogBackups
	if backups < 1 {
		backups = daemon.DefaultLogMaxBackups
	}
	candidates, err := managementCandidates(logFile, backups)
	if err != nil {
		return Access{}, false, err
	}
	for _, candidate := range candidates {
		access, err := Parse(candidate)
		if err == nil && Probe(ctx, access) {
			return access, true, nil
		}
	}
	return Access{}, false, nil
}

// DiscoverDaemonForRoot is the ownership-aware variant used by Desktop. A
// live authenticated endpoint is accepted only when its Service snapshot
// reports the same data root as the installed daemon metadata.
func DiscoverDaemonForRoot(ctx context.Context, expectedRoot string) (Access, bool, error) {
	meta, err := daemon.LoadMeta()
	if err != nil {
		return Access{}, false, err
	}
	logFile, err := resolveDaemonLogFile(meta)
	if err != nil {
		return Access{}, false, err
	}
	expectedRoot, err = resolveExpectedRoot(expectedRoot)
	if err != nil {
		return Access{}, false, err
	}
	backups := meta.LogBackups
	if backups < 1 {
		backups = daemon.DefaultLogMaxBackups
	}
	candidates, err := managementCandidates(logFile, backups)
	if err != nil {
		return Access{}, false, err
	}
	for _, candidate := range candidates {
		value, err := Parse(candidate)
		if err != nil || !ProbeDataRoot(ctx, value, expectedRoot) {
			continue
		}
		return value, true, nil
	}
	return Access{}, false, nil
}

func resolveDaemonLogFile(meta *daemon.Meta) (string, error) {
	if meta == nil || strings.TrimSpace(meta.LogFile) == "" {
		return "", errors.New("PairRoom daemon metadata has no log file")
	}
	path := strings.TrimSpace(meta.LogFile)
	if !filepath.IsAbs(path) {
		base := strings.TrimSpace(meta.WorkDir)
		if base == "" {
			return "", errors.New("PairRoom daemon metadata has a relative log file but no work directory")
		}
		if !filepath.IsAbs(base) {
			var err error
			base, err = filepath.Abs(base)
			if err != nil {
				return "", fmt.Errorf("resolve daemon work directory: %w", err)
			}
		}
		path = filepath.Join(base, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve daemon log file: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func resolveExpectedRoot(input string) (string, error) {
	value := strings.TrimSpace(input)
	if value == "" || !filepath.IsAbs(value) {
		return "", errors.New("expected PairRoom service data root must be absolute")
	}
	return filepath.Clean(value), nil
}

// ProbeDataRoot authenticates a Management endpoint and verifies that it
// serves the expected Service data root. This prevents a stale log entry for
// another local Service from being mistaken for the installed daemon.
func ProbeDataRoot(ctx context.Context, access Access, expectedRoot string) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, access.APIURL, nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+access.Token)
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	dataRoot, ok := decodeDataRoot(response.Body)
	if !ok {
		return false
	}
	return sameDataRoot(dataRoot, expectedRoot)
}

func decodeDataRoot(body io.Reader) (string, bool) {
	decoder := json.NewDecoder(io.LimitReader(body, maximumProbeBody))
	first, err := decoder.Token()
	if err != nil {
		return "", false
	}
	delimiter, ok := first.(json.Delim)
	if !ok || delimiter != '{' {
		return "", false
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return "", false
		}
		key, ok := keyToken.(string)
		if !ok {
			return "", false
		}
		if key == "data_root" {
			var value string
			if err := decoder.Decode(&value); err != nil {
				return "", false
			}
			return value, true
		}
		if err := skipJSONValue(decoder); err != nil {
			return "", false
		}
	}
	return "", false
}

func skipJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return nil
	}
	for decoder.More() {
		if delimiter == '{' {
			if _, err := decoder.Token(); err != nil {
				return err
			}
		}
		if err := skipJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func sameDataRoot(left, right string) bool {
	left = filepath.Clean(strings.TrimSpace(left))
	right = filepath.Clean(strings.TrimSpace(right))
	if left == "." || right == "." || left == "" || right == "" || !filepath.IsAbs(left) || !filepath.IsAbs(right) {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func managementCandidates(logFile string, backups int) ([]string, error) {
	if backups < 0 {
		backups = 0
	}
	if backups > maximumLogBackups {
		backups = maximumLogBackups
	}
	seen := make(map[string]struct{})
	var candidates []string
	for index := 0; index <= backups; index++ {
		path := filepath.Clean(logFile)
		if index > 0 {
			path = fmt.Sprintf("%s.%d", path, index)
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read daemon log %s: %w", path, err)
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		for lineIndex := len(lines) - 1; lineIndex >= 0; lineIndex-- {
			line := strings.TrimSpace(lines[lineIndex])
			if !strings.HasPrefix(line, "management:") {
				continue
			}
			candidate := strings.TrimSpace(strings.TrimPrefix(line, "management:"))
			if candidate == "" {
				continue
			}
			if _, exists := seen[candidate]; exists {
				continue
			}
			seen[candidate] = struct{}{}
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

// EqualToken avoids ordinary string comparison when a caller needs to compare
// two in-memory bootstrap tokens.
func EqualToken(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
