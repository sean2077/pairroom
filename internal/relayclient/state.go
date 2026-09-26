// Package relayclient owns the bounded workspace publication state and CLI
// transport. It never parses vendor transcripts or exports long-lived secrets.
package relayclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

type Pending struct {
	Seq     uint64    `json:"seq"`
	Text    string    `json:"text"`
	At      time.Time `json:"at"`
	Unknown bool      `json:"unknown,omitempty"`
}

// maxPublicationBacklog bounds the Stop replies saved locally while the
// Service cannot settle them: the pending head plus the held replies behind it.
const maxPublicationBacklog = 8

// maxPrivateFileBytes is the read limit for private state files. A held reply
// is refused rather than written into a state file that could not be read
// back, leaving headroom for the small fields later writes add.
const (
	maxPrivateFileBytes  = 2 << 20
	maxBacklogStateBytes = maxPrivateFileBytes - 64<<10
)

var errReplyTooLarge = errors.New("final reply exceeds bounded publication size; nothing published")

// errPublicationBacklogFull reports that a reply could not be saved behind the
// unresolved backlog; no sequence was consumed for it.
var errPublicationBacklogFull = errors.New("local publication backlog is full; reply not retained")

type State struct {
	Schema           int               `json:"schema"`
	Room             string            `json:"room"`
	Slot             model.ActorID     `json:"slot"`
	Runtime          model.RuntimeKind `json:"runtime"`
	Workspace        string            `json:"workspace"`
	EndpointPath     string            `json:"endpoint_path"`
	BindID           string            `json:"bind_id"`
	Generation       uint64            `json:"generation"`
	SessionID        string            `json:"session_id,omitempty"`
	LastSeq          uint64            `json:"last_seq"`
	LastConfirmedSeq uint64            `json:"last_confirmed_seq"`
	Pending          *Pending          `json:"pending,omitempty"`
	// Held are Stop replies saved behind an unresolved Pending head, in
	// sequence order. None has been sent: each is reported under its original
	// sequence only after every earlier one settles.
	Held        []Pending `json:"held,omitempty"`
	Blocks      int       `json:"blocks"`
	HarnessPID  int       `json:"harness_pid,omitempty"`
	HarnessName string    `json:"harness_name,omitempty"`
	// LastHookAt is local-only observability: when the approved Stop hook last
	// ran for this binding, so `relay status` can distinguish "hook never
	// fires" from "hook fires but nothing routes". Never sent to the Service.
	LastHookAt string `json:"last_hook_at,omitempty"`
	// TranscriptPath caches the reference last confirmed with the Service, so
	// a Stop hook calls confirm only when the harness reports a new one.
	TranscriptPath string `json:"transcript_path,omitempty"`
}

type credentials struct {
	BindID string `json:"bind_id"`
	Secret string `json:"secret"`
}

type Client struct {
	Dir      string
	State    State
	Secret   string
	Endpoint relay.Endpoint
	HTTP     *http.Client
	Save     func(State) error // injected only by deterministic crash-window tests
	// endpointErr defers a missing Service endpoint to the first relay call, so
	// a Stop hook can still save its reply WAL while the Service is stopped.
	endpointErr error
	// unsent is the pending head's sequence while this process knows it saved
	// that reply and has not reported it yet. It is never persisted: a head
	// loaded from disk may have been sent, so it is queried first.
	unsent uint64
}

func secureDir(root string, parts ...string) (string, error) {
	path := root
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "/\\\r\n\x00") {
			return "", errors.New("invalid relay state path component")
		}
		path = filepath.Join(path, part)
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errors.New("relay state path must not contain symlinks or non-directories")
		}
	}
	return path, nil
}
func readPrivate(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxPrivateFileBytes {
		return errors.New("relay state must be a bounded regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return errors.New("relay state and credential permissions must be 0600")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, value) != nil {
		return errors.New("invalid relay state; inspect before rebind")
	}
	return nil
}
func load(dir string) (*Client, error) {
	c, err := loadLocal(dir)
	if err != nil {
		return nil, err
	}
	if c.endpointErr != nil {
		return nil, c.endpointErr
	}
	return c, nil
}

// loadLocal validates the private binding without requiring a running Service.
// Relay calls on the result fail with the endpoint error until it is readable.
func loadLocal(dir string) (*Client, error) {
	var state State
	if err := readPrivate(filepath.Join(dir, "state.json"), &state); err != nil {
		return nil, err
	}
	if state.Schema != 2 {
		return nil, errors.New("retired relay state format; re-bind this slot without reading legacy credentials")
	}
	if !state.Slot.ValidParticipant() || state.Room == "" || state.BindID == "" {
		return nil, errors.New("invalid relay state identity")
	}
	if !validBacklog(state) {
		return nil, errors.New("invalid relay publication backlog; inspect before rebind")
	}
	var cred credentials
	if err := readPrivate(filepath.Join(dir, "credentials"), &cred); err != nil {
		return nil, err
	}
	if cred.BindID != state.BindID || cred.Secret == "" {
		return nil, errors.New("relay credential/state mismatch; recover bind explicitly")
	}
	c := &Client{Dir: dir, State: state, Secret: cred.Secret, HTTP: &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 40 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	c.Save = func(s State) error { return relay.AtomicJSON(filepath.Join(dir, "state.json"), s) }
	endpoint, err := relay.ReadEndpoint(state.EndpointPath)
	if err != nil {
		c.endpointErr = fmt.Errorf("read current Service endpoint (is PairRoom running?): %w", err)
	}
	c.Endpoint = endpoint
	return c, nil
}
func (c *Client) authHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Relay "+c.Secret)
	req.Header.Set("X-PairRoom-Bind", c.State.BindID)
	req.Header.Set("X-PairRoom-Generation", fmt.Sprint(c.State.Generation))
	req.Header.Set("X-PairRoom-Session", c.State.SessionID)
}
func (c *Client) call(ctx context.Context, action string, payload any, result any) error {
	if c.endpointErr != nil {
		return c.endpointErr
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.Endpoint.URL, "/")+"/api/v1/relay/"+c.State.Room+"/"+string(c.State.Slot)+"/"+action, strings.NewReader(string(data)))
	if err != nil {
		return errors.New("invalid relay request")
	}
	req.Header.Set("Content-Type", "application/json")
	c.authHeaders(req)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("relay %s transport unavailable", action)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		// Never echo raw HTML or vendor/provider errors into the model context.
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&failure)
		if failure.Error == "" {
			failure.Error = http.StatusText(res.StatusCode)
		}
		return fmt.Errorf("relay %s: %s", action, failure.Error)
	}
	if result == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 2<<20))
		return err
	}
	return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(result)
}
func (c *Client) persist(next State) error {
	if err := c.Save(next); err != nil {
		return err
	}
	c.State = next
	return nil
}

// Reconcile settles the pending head and then each held reply, strictly in
// sequence order. A head this process saved and never reported is reported
// directly. Any other head may already have been sent, so its original
// identity is queried before any automatic retransmission; transport
// uncertainty keeps it, marks it unknown and sends nothing behind it. A
// same-sequence supplement is permitted only after a negative authoritative
// receipt query, or an explicit operator resend decision, which applies to
// the current head only. A held reply was never sent, so reporting it after
// its predecessor settles is its first publication, not a replay.
func (c *Client) Reconcile(ctx context.Context, force bool) error {
	if c.State.Pending != nil && c.endpointErr != nil {
		// Nothing can be sent or queried; the backlog stays exactly as saved.
		return c.endpointErr
	}
	for c.State.Pending != nil {
		p := *c.State.Pending
		accepted := false
		if p.Seq != c.unsent {
			var receipt struct {
				Accepted *bool `json:"accepted"`
			}
			err := c.call(ctx, "publication", map[string]any{"report_seq": p.Seq}, &receipt)
			if (err != nil || receipt.Accepted == nil) && !force {
				return c.markUnknown(p)
			}
			accepted = receipt.Accepted != nil && *receipt.Accepted
		}
		if !accepted {
			// From here an attempt may exist; only a receipt query settles it.
			c.unsent = 0
			var result relay.Publication
			if err := c.call(ctx, "report", map[string]any{"report_seq": p.Seq, "text": p.Text}, &result); err != nil {
				return c.markUnknown(p)
			}
			if result.ReportSeq != p.Seq || result.BindID != c.State.BindID || result.Generation != c.State.Generation {
				return relay.ErrUnknown
			}
		}
		next := c.State.advance()
		next.LastConfirmedSeq = p.Seq
		if err := c.persist(next); err != nil {
			return err
		}
		if c.State.Pending != nil {
			c.unsent = c.State.Pending.Seq
		}
		force = false
	}
	return nil
}

// Publish settles the existing backlog first and publishes text only after
// it is empty, so a reply that could not be saved never overtakes it.
func (c *Client) Publish(ctx context.Context, text string) error {
	if err := c.Reconcile(ctx, false); err != nil {
		return err
	}
	if err := c.ReservePublication(text); err != nil {
		return err
	}
	return c.Reconcile(ctx, false)
}

func (c *Client) markUnknown(p Pending) error {
	p.Unknown = true
	next := c.State
	next.Pending = &p
	_ = c.persist(next)
	return relay.ErrUnknown
}

// ReservePublication claims the next report sequence and persists the reply
// body as the WAL before any HTTP for this invocation, so a stopped Service or
// a transient metadata failure cannot lose the reply without a trace. With no
// pending publication the reply becomes the head; behind an unresolved head
// it is held, never sent until everything before it settles. A full backlog
// refuses the reply without consuming a sequence.
func (c *Client) ReservePublication(text string) error {
	if len(text) > relay.MaxBodyBytes {
		return errReplyTooLarge
	}
	next := c.State
	next.LastSeq++
	p := Pending{Seq: next.LastSeq, Text: text, At: time.Now().UTC()}
	if next.Pending == nil {
		next.Pending = &p
	} else {
		if 1+len(next.Held) >= maxPublicationBacklog {
			return errPublicationBacklogFull
		}
		next.Held = append(slices.Clone(next.Held), p)
		// Measure the exact bytes relay.AtomicJSON writes: never write a state
		// file that the bounded reader would then refuse.
		data, err := relay.EncodeJSONFile(next)
		if err != nil {
			return err
		}
		if len(data) > maxBacklogStateBytes {
			return errPublicationBacklogFull
		}
	}
	// This one replacement is the seq claim and body WAL. No HTTP before it.
	if err := c.persist(next); err != nil {
		return err
	}
	if next.Pending.Seq == p.Seq {
		c.unsent = p.Seq
	}
	return nil
}

// DiscardPending is the explicit decision to abandon the uncertain head. Its
// consumed sequence is retained so a later observed jump records a gap. The
// next held reply becomes the head without being sent.
func (c *Client) DiscardPending() error { return c.persist(c.State.advance()) }

// advance drops the pending head and promotes the next held reply, so the
// backlog never has held replies without a head.
func (s State) advance() State {
	s.Pending = nil
	if len(s.Held) > 0 {
		head := s.Held[0]
		s.Pending = &head
		s.Held = slices.Clone(s.Held[1:])
		if len(s.Held) == 0 {
			s.Held = nil
		}
	}
	return s
}

// validBacklog accepts no pending publication, a head alone as older CLIs
// wrote it, or a head followed by a bounded run of never-sent replies with
// the next consecutive sequences.
func validBacklog(s State) bool {
	if len(s.Held) == 0 {
		return true
	}
	if s.Pending == nil || 1+len(s.Held) > maxPublicationBacklog || s.Held[len(s.Held)-1].Seq != s.LastSeq {
		return false
	}
	for i, h := range s.Held {
		if h.Seq != s.Pending.Seq+uint64(i)+1 || h.Unknown || len(h.Text) > relay.MaxBodyBytes {
			return false
		}
	}
	return true
}

func cleanupAtomicTemps(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".state.json-") || strings.HasPrefix(entry.Name(), ".credentials-") || strings.HasPrefix(entry.Name(), ".bind-attempt.json-") || strings.HasPrefix(entry.Name(), ".claude-inbox-") {
			if entry.Type().IsRegular() {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
}
