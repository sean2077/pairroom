// Package updatecheck reads public release metadata. It never installs software
// or receives Service credentials, workspace paths, or conversation content.
package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	Endpoint        = "/api/v1/updates"
	releasesAPI     = "https://api.github.com/repos/sean2077/pairroom/releases/latest"
	releasesURL     = "https://github.com/sean2077/pairroom/releases/tag/"
	checkInterval   = 6 * time.Hour
	failureInterval = 15 * time.Minute
	manualInterval  = time.Minute
	requestTimeout  = 5 * time.Second
	maxResponse     = 1 << 20
)

var stableVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

type Result struct {
	CurrentVersion string `json:"current_version"`
	Status         string `json:"status"`
	LatestVersion  string `json:"latest_version,omitempty"`
	ReleaseURL     string `json:"release_url,omitempty"`
	CheckedAt      string `json:"checked_at,omitempty"`
	NextCheckAt    string `json:"next_check_at,omitempty"`
}

type release struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Checker coalesces callers and caches both success and failure. A manual check
// may bypass the ordinary cache, but not its one-minute floor or rate limits.
type Checker struct {
	mu                    sync.Mutex
	client                *http.Client
	now                   func() time.Time
	current               string
	result                Result
	nextCheck, nextManual time.Time
	pending               chan struct{}
	etag                  string
	lastRelease           release
}

func New(current string) *Checker {
	c := &Checker{
		current: current, now: time.Now,
		client: &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		result: Result{CurrentVersion: current, Status: "unchecked"},
	}
	if _, ok := versionParts(current); !ok {
		c.result.Status = "unsupported"
	}
	return c
}

// Check starts work only on demand. Cancelling a tab detaches that waiter, not
// another tab's shared request; that request has its own bounded deadline.
func (c *Checker) Check(ctx context.Context, manual bool) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	c.mu.Lock()
	if c.result.Status == "unsupported" || (c.pending == nil && ((!manual && c.now().Before(c.nextCheck)) || (manual && c.now().Before(c.nextManual)))) {
		result := c.result
		c.mu.Unlock()
		return result, nil
	}
	if c.pending == nil {
		c.pending = make(chan struct{})
		go c.refresh(c.pending, c.etag, c.lastRelease)
	}
	done := c.pending
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.result, nil
	}
}

func (c *Checker) refresh(done chan struct{}, etag string, previous release) {
	latest, validator, retryAt, err := c.fetch(etag, previous)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	result := Result{CurrentVersion: c.current, Status: "unavailable", CheckedAt: now.UTC().Format(time.RFC3339)}
	c.nextCheck, c.nextManual = now.Add(failureInterval), now.Add(manualInterval)
	if err == nil {
		result.Status = "current"
		result.LatestVersion = strings.TrimPrefix(latest.Tag, "v")
		if newer(latest.Tag, c.current) {
			result.Status = "available"
		}
		result.ReleaseURL = releasesURL + url.PathEscape(latest.Tag)
		c.nextCheck = now.Add(checkInterval)
		c.etag, c.lastRelease = validator, latest
	}
	if retryAt.After(c.nextCheck) {
		c.nextCheck = retryAt
	}
	if retryAt.After(c.nextManual) {
		c.nextManual = retryAt
	}
	result.NextCheckAt = c.nextCheck.UTC().Format(time.RFC3339)
	c.result, c.pending = result, nil
	close(done)
}

func (c *Checker) fetch(etag string, previous release) (release, string, time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesAPI, nil)
	if err != nil {
		return release{}, "", time.Time{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "PairRoom-update-check")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	response, err := c.client.Do(req)
	if err != nil {
		return release{}, "", time.Time{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified && etag != "" && previous.Tag != "" {
		return previous, etag, time.Time{}, nil
	}
	if response.StatusCode != http.StatusOK {
		var retryAt time.Time
		if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusTooManyRequests {
			retryAt = rateLimitUntil(response.Header, c.now())
		}
		return release{}, "", retryAt, errors.New("release check unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		return release{}, "", time.Time{}, err
	}
	if len(body) > maxResponse {
		return release{}, "", time.Time{}, errors.New("release response too large")
	}
	var latest release
	if err := json.Unmarshal(body, &latest); err != nil {
		return release{}, "", time.Time{}, err
	}
	if _, ok := versionParts(latest.Tag); !ok || latest.Draft || latest.Prerelease {
		return release{}, "", time.Time{}, errors.New("not a stable release")
	}
	validator := response.Header.Get("ETag")
	if len(validator) > 256 || strings.ContainsAny(validator, "\r\n") {
		validator = ""
	}
	return latest, validator, time.Time{}, nil
}

func versionParts(value string) ([]string, bool) {
	if len(value) > 128 || !stableVersion.MatchString(value) {
		return nil, false
	}
	core, _, _ := strings.Cut(strings.TrimPrefix(value, "v"), "+")
	return strings.Split(core, "."), true
}

// Compare numeric components without integer overflow or lexical 5.10 < 5.9
// mistakes. Build metadata does not change precedence; prereleases are excluded.
func newer(latest, current string) bool {
	a, okA := versionParts(latest)
	b, okB := versionParts(current)
	if !okA || !okB {
		return false
	}
	for i := range a {
		if len(a[i]) != len(b[i]) {
			return len(a[i]) > len(b[i])
		}
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func rateLimitUntil(header http.Header, now time.Time) time.Time {
	until := now.Add(failureInterval)
	if seconds, err := strconv.ParseInt(header.Get("Retry-After"), 10, 64); err == nil && seconds > 0 {
		if seconds > 86400 {
			seconds = 86400
		}
		if candidate := now.Add(time.Duration(seconds) * time.Second); candidate.After(until) {
			until = candidate
		}
	} else if candidate, err := http.ParseTime(header.Get("Retry-After")); err == nil && candidate.After(until) {
		until = candidate
	}
	if seconds, err := strconv.ParseInt(header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		if candidate := time.Unix(seconds, 0); candidate.After(until) {
			until = candidate
		}
	}
	if ceiling := now.Add(24 * time.Hour); until.After(ceiling) {
		until = ceiling
	}
	return until
}

// Handler must be mounted inside the host's existing API authentication layer,
// not under the public static-asset prefix. It forwards no inbound headers.
func (c *Checker) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		result, err := c.Check(r.Context(), r.URL.Query().Get("refresh") == "1")
		if err != nil {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
}
