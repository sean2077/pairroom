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
	Nonce            string            `json:"bind_nonce,omitempty"`
	LastSeq          uint64            `json:"last_seq"`
	LastConfirmedSeq uint64            `json:"last_confirmed_seq"`
	Pending          *Pending          `json:"pending,omitempty"`
	Blocks           int               `json:"blocks"`
	HarnessPID       int               `json:"harness_pid,omitempty"`
	HarnessName      string            `json:"harness_name,omitempty"`
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
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 2<<20 {
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
	var state State
	if err := readPrivate(filepath.Join(dir, "state.json"), &state); err != nil {
		return nil, err
	}
	if state.Schema != 1 || !state.Slot.ValidParticipant() || state.Room == "" || state.BindID == "" {
		return nil, errors.New("invalid relay state identity")
	}
	var cred credentials
	if err := readPrivate(filepath.Join(dir, "credentials"), &cred); err != nil {
		return nil, err
	}
	if cred.BindID != state.BindID || cred.Secret == "" {
		return nil, errors.New("relay credential/state mismatch; recover bind explicitly")
	}
	endpoint, err := relay.ReadEndpoint(state.EndpointPath)
	if err != nil {
		return nil, fmt.Errorf("read current Service endpoint (is PairRoom running?): %w", err)
	}
	c := &Client{Dir: dir, State: state, Secret: cred.Secret, Endpoint: endpoint, HTTP: &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 40 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	c.Save = func(s State) error { return relay.AtomicJSON(filepath.Join(dir, "state.json"), s) }
	return c, nil
}
func (c *Client) authHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Relay "+c.Secret)
	req.Header.Set("X-PairRoom-Bind", c.State.BindID)
	req.Header.Set("X-PairRoom-Generation", fmt.Sprint(c.State.Generation))
	req.Header.Set("X-PairRoom-Session", c.State.SessionID)
}
func (c *Client) call(ctx context.Context, action string, payload any, result any) error {
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

// Reconcile queries the original identity before any automatic retransmission.
// Transport uncertainty preserves the only pending slot and never allocates a
// new sequence. A same-sequence supplement is permitted only after a negative
// authoritative receipt query, or an explicit operator resend decision.
func (c *Client) Reconcile(ctx context.Context, force bool) error {
	if c.State.Pending == nil {
		return nil
	}
	p := *c.State.Pending
	var receipt struct {
		Accepted *bool `json:"accepted"`
	}
	err := c.call(ctx, "publication", map[string]any{"report_seq": p.Seq}, &receipt)
	if (err != nil || receipt.Accepted == nil) && !force {
		p.Unknown = true
		next := c.State
		next.Pending = &p
		_ = c.persist(next)
		return relay.ErrUnknown
	}
	if receipt.Accepted == nil || !*receipt.Accepted {
		var accepted relay.Publication
		if err := c.call(ctx, "report", map[string]any{"report_seq": p.Seq, "text": p.Text}, &accepted); err != nil {
			p.Unknown = true
			next := c.State
			next.Pending = &p
			_ = c.persist(next)
			return relay.ErrUnknown
		}
		if accepted.ReportSeq != p.Seq || accepted.BindID != c.State.BindID || accepted.Generation != c.State.Generation {
			return relay.ErrUnknown
		}
	}
	next := c.State
	next.Pending = nil
	next.LastConfirmedSeq = p.Seq
	return c.persist(next)
}
func (c *Client) Publish(ctx context.Context, text string) error {
	if err := c.Reconcile(ctx, false); err != nil {
		return err
	}
	if len(text) > relay.MaxBodyBytes {
		return errors.New("final reply exceeds bounded publication size; nothing published")
	}
	next := c.State
	next.LastSeq++
	next.Pending = &Pending{Seq: next.LastSeq, Text: text, At: time.Now().UTC()}
	// This one replacement is the seq claim and body WAL. No HTTP before it.
	if err := c.persist(next); err != nil {
		return err
	}
	var result relay.Publication
	if err := c.call(ctx, "report", map[string]any{"report_seq": next.LastSeq, "text": text}, &result); err != nil {
		return relay.ErrUnknown
	}
	if result.ReportSeq != next.LastSeq || result.BindID != next.BindID || result.Generation != next.Generation {
		return relay.ErrUnknown
	}
	next = c.State
	next.Pending = nil
	next.LastConfirmedSeq = next.LastSeq
	return c.persist(next)
}
func (c *Client) DiscardPending() error { next := c.State; next.Pending = nil; return c.persist(next) }
func cleanupAtomicTemps(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".state.json-") || strings.HasPrefix(entry.Name(), ".credentials-") {
			if entry.Type().IsRegular() {
				_ = os.Remove(filepath.Join(dir, entry.Name()))
			}
		}
	}
}
