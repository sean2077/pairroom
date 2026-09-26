// Package updatecheck implements Desktop's opt-in, read-only release check.
// It only asks GitHub for the latest stable release and compares versions; it
// never downloads or installs anything. The choice and the last result live in
// the Desktop preference directory, not the Service data root, and nothing is
// requested until the user explicitly enables the check.
package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MessageKind = "pairroom.desktop.updates"
	// Endpoint is the only URL the checker requests.
	Endpoint = "https://api.github.com/repos/sean2077/pairroom/releases/latest"
	// ReleasePageBase is joined with the verified tag to form the release page,
	// so a response can never choose the URL Desktop opens.
	ReleasePageBase = "https://github.com/sean2077/pairroom/releases/tag/"
	// Interval bounds automatic checks; explicit Check now is not throttled.
	Interval = 24 * time.Hour
	// RequestTimeout stays below the Settings bridge timeout so a manual check
	// always reports its own outcome.
	RequestTimeout = 10 * time.Second

	prefsFile    = "update-check.json"
	maxBodyBytes = 1 << 20
	maxMessage   = 1024
)

// ErrDisabled reports a check requested while the user has not opted in.
var ErrDisabled = errors.New("update checks are turned off")

// Version is a stable X.Y.Z release version.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Compare returns -1, 0, or 1.
func (v Version) Compare(other Version) int {
	for _, pair := range [][2]int{{v.Major, other.Major}, {v.Minor, other.Minor}, {v.Patch, other.Patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ParseStable accepts only X.Y.Z or vX.Y.Z with decimal components and no
// leading zeros. Prerelease/build suffixes and anything else are rejected.
func ParseStable(value string) (Version, bool) {
	parts := strings.Split(strings.TrimPrefix(value, "v"), ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var numbers [3]int
	for i, part := range parts {
		if part == "" || len(part) > 9 || (len(part) > 1 && part[0] == '0') {
			return Version{}, false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return Version{}, false
			}
		}
		numbers[i], _ = strconv.Atoi(part)
	}
	return Version{numbers[0], numbers[1], numbers[2]}, true
}

// Due reports whether an automatic check may run. A last check in the future
// (clock moved back) does not suppress checks indefinitely.
func Due(last, now time.Time) bool {
	return last.IsZero() || now.Sub(last) >= Interval || last.After(now)
}

// ParseRelease reads a GitHub release object and returns its stable version.
// Drafts, prereleases, and malformed tags are errors.
func ParseRelease(body io.Reader) (Version, error) {
	var release struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxBodyBytes)).Decode(&release); err != nil {
		return Version{}, fmt.Errorf("unreadable release response: %w", err)
	}
	if release.Draft || release.Prerelease {
		return Version{}, errors.New("the latest release is not a stable release")
	}
	version, ok := ParseStable(release.TagName)
	if !ok {
		return Version{}, fmt.Errorf("unrecognized release tag %q", release.TagName)
	}
	return version, nil
}

// ReleasePage is the release notes page for a verified version.
func ReleasePage(v Version) string { return ReleasePageBase + "v" + v.String() }

// Prefs is the persisted Desktop preference. Latest is the newest stable
// version seen; availability is recomputed against the running version, so
// the notice disappears after an upgrade.
type Prefs struct {
	Enabled   bool      `json:"enabled"`
	LastCheck time.Time `json:"last_check,omitzero"`
	Latest    string    `json:"latest,omitempty"`
}

// LoadPrefs returns disabled defaults when the file is missing. An unreadable
// or malformed file is also treated as disabled: consent is never inferred.
func LoadPrefs(dir string) (Prefs, error) {
	data, err := os.ReadFile(filepath.Join(dir, prefsFile))
	if errors.Is(err, os.ErrNotExist) {
		return Prefs{}, nil
	}
	if err != nil {
		return Prefs{}, err
	}
	var prefs Prefs
	if err := json.Unmarshal(data, &prefs); err != nil {
		return Prefs{}, fmt.Errorf("invalid %s: %w", prefsFile, err)
	}
	return prefs, nil
}

// SavePrefs replaces the preference file atomically with owner-only access.
func SavePrefs(dir string, prefs Prefs) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".update-check-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), filepath.Join(dir, prefsFile))
}

// Status is what Settings and the tray display.
type Status struct {
	Enabled   bool
	Current   string
	Latest    string // set only when newer than Current
	URL       string // release page for Latest
	LastCheck time.Time
}

// Checker owns the preference file and the release request.
type Checker struct {
	Dir      string
	Current  string
	Client   *http.Client
	Endpoint string
	Now      func() time.Time
	// OnChange, when set, receives the status after every stored change.
	OnChange func(Status)

	mu       sync.Mutex // guards the preference file
	checkMu  sync.Mutex // serializes release requests
	wakeOnce sync.Once
	wakeCh   chan struct{}
}

func (c *Checker) wake() chan struct{} {
	c.wakeOnce.Do(func() { c.wakeCh = make(chan struct{}, 1) })
	return c.wakeCh
}

// Run performs automatic checks until ctx ends: once at start, every poll,
// and right after the user enables checking. Check itself enforces Interval,
// so polling reads only the local preference file until a check is due.
// Automatic failures are quiet; they surface only through an explicit check.
func (c *Checker) Run(ctx context.Context, poll time.Duration) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-c.wake():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		_, _ = c.Check(ctx, false)
		timer.Reset(poll)
	}
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Checker) status(prefs Prefs) Status {
	status := Status{Enabled: prefs.Enabled, Current: c.Current, LastCheck: prefs.LastCheck}
	latest, okLatest := ParseStable(prefs.Latest)
	current, okCurrent := ParseStable(c.Current)
	// Turning the check off also hides a previously found notice.
	if prefs.Enabled && okLatest && okCurrent && latest.Compare(current) > 0 {
		status.Latest = latest.String()
		status.URL = ReleasePage(latest)
	}
	return status
}

// Status reads the stored preference without any network access.
func (c *Checker) Status() (Status, error) {
	if c.Dir == "" {
		return Status{Current: c.Current}, errors.New("the Desktop preference directory is unavailable")
	}
	c.mu.Lock()
	prefs, err := LoadPrefs(c.Dir)
	c.mu.Unlock()
	return c.status(prefs), err
}

func (c *Checker) update(mutate func(*Prefs)) (Status, error) {
	if c.Dir == "" {
		return Status{Current: c.Current}, errors.New("the Desktop preference directory is unavailable")
	}
	c.mu.Lock()
	prefs, _ := LoadPrefs(c.Dir) // A malformed file is replaced by the explicit change.
	mutate(&prefs)
	err := SavePrefs(c.Dir, prefs)
	if err != nil {
		prefs, _ = LoadPrefs(c.Dir)
	}
	c.mu.Unlock()
	status := c.status(prefs)
	if err == nil && c.OnChange != nil {
		c.OnChange(status)
	}
	return status, err
}

// SetEnabled records the user's explicit choice. It performs no request
// itself; enabling wakes Run so a due check follows promptly.
func (c *Checker) SetEnabled(enabled bool) (Status, error) {
	status, err := c.update(func(p *Prefs) { p.Enabled = enabled })
	if err == nil && enabled {
		select {
		case c.wake() <- struct{}{}:
		default:
		}
	}
	return status, err
}

// Check requests the latest release when enabled. Automatic checks run only
// when Due; explicit checks always run. Every attempt, successful or not, is
// recorded so automatic checks stay within Interval. A failed request keeps
// the previously known latest version.
func (c *Checker) Check(ctx context.Context, explicit bool) (Status, error) {
	c.checkMu.Lock()
	defer c.checkMu.Unlock()
	status, err := c.Status()
	if err != nil {
		return status, err
	}
	if !status.Enabled {
		return status, ErrDisabled
	}
	if !explicit && !Due(status.LastCheck, c.now()) {
		return status, nil
	}
	latest, fetchErr := c.fetch(ctx)
	status, saveErr := c.update(func(p *Prefs) {
		p.LastCheck = c.now().UTC()
		if fetchErr == nil {
			p.Latest = latest.String()
		}
	})
	return status, errors.Join(fetchErr, saveErr)
}

func (c *Checker) fetch(ctx context.Context) (Version, error) {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = Endpoint
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: RequestTimeout}
	}
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Version{}, err
	}
	// Only a product User-Agent: no token, cookie, or installation identifier.
	request.Header.Set("User-Agent", "PairRoom-Desktop/"+c.Current)
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := client.Do(request)
	if err != nil {
		return Version{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Version{}, fmt.Errorf("GitHub responded with HTTP %d", response.StatusCode)
	}
	return ParseRelease(response.Body)
}

// Request is the bounded Settings bridge message.
type Request struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Action  string `json:"action"`
	Enabled *bool  `json:"enabled,omitempty"`
}

// Response mirrors Status for the Settings bridge.
type Response struct {
	ID        string `json:"id"`
	Enabled   *bool  `json:"enabled,omitempty"`
	Current   string `json:"current,omitempty"`
	Latest    string `json:"latest,omitempty"`
	URL       string `json:"url,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Handle accepts typed get/set/check requests; anything else is ignored. A
// check may block for up to RequestTimeout, so callers run it off the UI
// thread.
func (c *Checker) Handle(ctx context.Context, message string) (Response, bool) {
	if len(message) > maxMessage {
		return Response{}, false
	}
	var request Request
	decoder := json.NewDecoder(strings.NewReader(message))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return Response{}, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Response{}, false
	}
	if request.Kind != MessageKind || request.ID == "" || len(request.ID) > 100 {
		return Response{}, false
	}
	switch request.Action {
	case "get", "check":
		if request.Enabled != nil {
			return Response{}, false
		}
	case "set":
		if request.Enabled == nil {
			return Response{}, false
		}
	default:
		return Response{}, false
	}
	var status Status
	var err error
	switch request.Action {
	case "get":
		status, err = c.Status()
	case "set":
		status, err = c.SetEnabled(*request.Enabled)
	case "check":
		status, err = c.Check(ctx, true)
	}
	response := Response{ID: request.ID, Enabled: &status.Enabled, Current: status.Current, Latest: status.Latest, URL: status.URL}
	if !status.LastCheck.IsZero() {
		response.CheckedAt = status.LastCheck.UTC().Format(time.RFC3339)
	}
	if err != nil {
		response.Error = err.Error()
	}
	return response, true
}
