package relayclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/version"
)

// A relay command names a CLI/Service release mismatch from responses it
// already receives: the Service stamps its release on every API response, and
// older Services still report it in the /api/v1/service snapshot that binding
// and discovery read. Successful commands and every Stop hook add no request;
// only a failed foreground command that looks like version skew, with the
// release still unknown, spends one bounded read to learn it.
type serviceObservationKey struct{}

type serviceObservation struct {
	mu       sync.Mutex
	release  string
	endpoint string
	// rejected records a Service rejection that a release mismatch plausibly
	// explains: an unknown route/operation, a request shape the strict decoder
	// refuses, or an authentication format it does not accept.
	rejected bool
}

// Binding lookups that an older or newer CLI resolves differently. They fail
// before any Service contact, so the release is learned by a bounded probe.
var (
	errNoAssociatedBinding = errors.New("this native session has no matching associated binding in this workspace; run pairroom relay bind here first (do not consume another session's inbox)")
	errNoWorkspaceBinding  = errors.New("no relay binding in this workspace; run pairroom relay bind first")
)

func withServiceObservation(ctx context.Context) (context.Context, *serviceObservation) {
	observed := &serviceObservation{}
	return context.WithValue(ctx, serviceObservationKey{}, observed), observed
}

func observationFrom(ctx context.Context) *serviceObservation {
	observed, _ := ctx.Value(serviceObservationKey{}).(*serviceObservation)
	return observed
}

// noteServiceEndpoint remembers the endpoint file this invocation targets; a
// later, more specific value (the binding's saved endpoint) replaces it.
func noteServiceEndpoint(ctx context.Context, path string) {
	if observed := observationFrom(ctx); observed != nil && path != "" {
		observed.mu.Lock()
		observed.endpoint = path
		observed.mu.Unlock()
	}
}

func observeServiceResponse(ctx context.Context, res *http.Response) {
	observed := observationFrom(ctx)
	if observed == nil {
		return
	}
	observed.mu.Lock()
	defer observed.mu.Unlock()
	observed.recordLocked(res.Header.Get(relay.VersionHeader))
	switch res.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound:
		observed.rejected = true
	}
}

// observeServiceSnapshotVersion is the fallback for a Service that predates
// the response header; the header, when present, was recorded first.
func observeServiceSnapshotVersion(ctx context.Context, display string) {
	observed := observationFrom(ctx)
	if observed == nil {
		return
	}
	observed.mu.Lock()
	defer observed.mu.Unlock()
	observed.recordLocked(display)
}

func (o *serviceObservation) recordLocked(display string) {
	if o.release == "" {
		o.release = serviceRelease(display)
	}
}

func (o *serviceObservation) skewShapedLocked(err error) bool {
	return err != nil && (o.rejected || errors.Is(err, errNoAssociatedBinding) || errors.Is(err, errNoWorkspaceBinding))
}

// needsProbe reports a skew-shaped failure whose Service release is still
// unknown.
func (o *serviceObservation) needsProbe(err error) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.release == "" && o.skewShapedLocked(err)
}

func (o *serviceObservation) endpointPath() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.endpoint
}

// probeServiceRelease makes one short, read-only snapshot request on a failed
// foreground command. Any failure leaves the release unknown and prints
// nothing; the endpoint token is only sent, never printed.
func probeServiceRelease(ctx context.Context, endpointPath string) {
	if endpointPath == "" {
		var err error
		if endpointPath, err = defaultEndpoint(); err != nil {
			return
		}
	}
	endpoint, err := relay.ReadEndpoint(endpointPath)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var discard struct{}
	_ = management(ctx, endpoint, http.MethodGet, "/api/v1/service", nil, &discard)
}

// serviceRelease compares releases, not build metadata: a display version
// such as v5.5.1+8.a5cb253 still belongs to release 5.5.1. The value comes from
// the Service and may reach a model's context through stderr, so anything but
// a short version token is treated as unknown.
func serviceRelease(display string) string {
	release, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(display), "v"), "+")
	if release == "" || len(release) > 32 {
		return ""
	}
	for _, c := range release {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '.' || c == '-') {
			return ""
		}
	}
	return release
}

// hint returns at most one stderr line for the invocation, or "" when the
// Service release is unknown or matches this CLI.
func (o *serviceObservation) hint(err error) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.release == "" || o.release == version.Current {
		return ""
	}
	if o.skewShapedLocked(err) {
		return fmt.Sprintf("PairRoom: this failure may come from a release mismatch: the Service runs %s but this CLI is %s. Run pairroom relay preflight and use the CLI from the Service's release.", o.release, version.Current)
	}
	return fmt.Sprintf("PairRoom: the Service runs release %s but this CLI is %s; run pairroom relay preflight and use the CLI from the Service's release.", o.release, version.Current)
}
