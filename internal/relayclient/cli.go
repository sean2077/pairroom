package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

type options struct {
	repo, room, slot, kind, endpoint, text, id, to, session string
	replace, cont, purge, enabled, discard, resend          bool
	timeout                                                 int
	attachments                                             stringsFlag
}
type stringsFlag []string

func (s *stringsFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }
func Run(ctx context.Context, args []string, in io.Reader, out, diagnostic io.Writer) error {
	if len(args) == 0 {
		return errors.New("use pairroom relay install|bind|hook|send|wait|status|peer|park|nudge|reconcile|unbind (see docs/CLI_REFERENCE.md)")
	}
	action := args[0]
	o := options{}
	flags := flag.NewFlagSet("pairroom relay "+action, flag.ContinueOnError)
	flags.SetOutput(diagnostic)
	flags.StringVar(&o.repo, "repo", ".", "Room project path")
	flags.StringVar(&o.room, "room", "", "Room ID")
	flags.StringVar(&o.slot, "slot", "", "stable slot ID: claude or codex, independent of runtime")
	flags.StringVar(&o.kind, "runtime", "", "native harness: claude or codex")
	flags.StringVar(&o.endpoint, "service-file", "", "owner-only relay-endpoint.json path for a custom Service data root")
	flags.StringVar(&o.text, "text", "", "message body; otherwise read stdin")
	flags.StringVar(&o.id, "id", "", "stable client message ID; reuse on uncertain send")
	flags.StringVar(&o.to, "to", "", "explicit send target: @user, or empty for peer")
	flags.StringVar(&o.session, "session-id", "", "associated session identity for explicit --continue")
	flags.BoolVar(&o.replace, "replace", false, "explicitly revoke occupied binding; does not stop native work")
	flags.BoolVar(&o.cont, "continue", false, "restore the same associated session")
	flags.BoolVar(&o.purge, "purge-hooks", false, "remove this runtime's relay hooks when no other local binding uses them")
	flags.BoolVar(&o.enabled, "enabled", true, "park enabled")
	flags.BoolVar(&o.discard, "discard", false, "explicitly discard uncertain pending publication, retaining its consumed sequence")
	flags.BoolVar(&o.resend, "resend", false, "explicitly supplement uncertain pending with its ORIGINAL sequence")
	flags.IntVar(&o.timeout, "timeout", 30, "wait seconds (1–30)")
	flags.Var(&o.attachments, "attach", "image attachment path (repeatable)")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected relay arguments")
	}
	if action == "hook" {
		return runHook(ctx, o, in, out, diagnostic)
	}
	root, err := workspace(ctx, o.repo)
	if err != nil {
		return err
	}
	if action == "install" {
		kind := model.RuntimeKind(o.kind)
		if err := editHooks(root, kind, false); err != nil {
			return err
		}
		if err := installSkill(kind); err != nil {
			return err
		}
		return writeJSON(out, map[string]any{"installed": true, "runtime": kind, "notice": "Restart/review the exact project hook in your native harness (Codex: /hooks). This command does not grant native trust. Keep pairroom on PATH. Real authenticated bidirectional E2E remains release-gated."})
	}
	if action == "bind" {
		return bind(ctx, root, o, out)
	}
	if !model.ActorID(o.slot).ValidParticipant() || !safePart(o.room) {
		return errors.New("--room and --slot claude|codex are required")
	}
	dir, err := secureDir(root, ".pairroom", "rooms", o.room, "slots", o.slot)
	if err != nil {
		return err
	}
	release, err := lockSlot(ctx, dir)
	if err != nil {
		return err
	}
	c, err := load(dir)
	if err != nil {
		release()
		return err
	}
	cleanupAtomicTemps(dir)
	if action == "reconcile" || action == "status" {
		if o.discard && o.resend {
			release()
			return errors.New("choose --discard or --resend, not both")
		}
		if action == "reconcile" && o.discard {
			err = c.DiscardPending()
		} else {
			err = c.Reconcile(ctx, action == "reconcile" && o.resend)
		}
		if err != nil && !errors.Is(err, relay.ErrUnknown) {
			release()
			return err
		}
	}
	release()
	switch action {
	case "send":
		text := o.text
		if text == "" {
			data, err := io.ReadAll(io.LimitReader(in, relay.MaxBodyBytes+1))
			if err != nil {
				return err
			}
			text = string(data)
		}
		if len(text) > relay.MaxBodyBytes {
			return errors.New("message exceeds 256 KiB")
		}
		if o.id == "" {
			o.id, err = relay.RandomID()
			if err != nil {
				return err
			}
		}
		to := model.ActorID("")
		if o.to != "" {
			if o.to != "@user" {
				return errors.New("--to supports only @user; default is the peer")
			}
			to = model.ActorUser
		}
		attachments := []string{}
		for _, path := range o.attachments {
			id, err := c.upload(ctx, path)
			if err != nil {
				return err
			}
			attachments = append(attachments, id)
		}
		var msg relay.Message
		err = c.call(ctx, "send", map[string]any{"id": o.id, "text": text, "to": to, "attachment_ids": attachments}, &msg)
		if err != nil {
			return fmt.Errorf("%w; publication uncertain: retry with the SAME --id %s, not a new ID", err, o.id)
		}
		return writeJSON(out, msg)
	case "wait":
		return deliver(ctx, c, false, o.timeout, out)
	case "peer":
		var peer relay.Binding
		if err := c.call(ctx, "peer", nil, &peer); err != nil {
			return err
		}
		return writeJSON(out, peer)
	case "park":
		var result map[string]any
		if err := c.call(ctx, "park", map[string]bool{"enabled": o.enabled}, &result); err != nil {
			return err
		}
		return writeJSON(out, result)
	case "nudge":
		return writeJSON(out, map[string]string{"notice": "PairRoom cannot inject outside a park window. Run the following in the associated native session.", "command": fmt.Sprintf("pairroom relay wait --room %s --slot %s", o.room, o.slot)})
	case "status", "reconcile":
		var status relay.Snapshot
		if e := c.call(ctx, "status", nil, &status); e != nil {
			return e
		}
		local := map[string]any{"last_confirmed_seq": c.State.LastConfirmedSeq, "last_seq": c.State.LastSeq, "publication_unknown": errors.Is(err, relay.ErrUnknown)}
		if c.State.Pending != nil {
			local["pending_seq"] = c.State.Pending.Seq
			local["publication_unknown"] = c.State.Pending.Unknown
		}
		return writeJSON(out, map[string]any{"local": local, "relay": status})
	case "unbind":
		release, err := lockSlot(ctx, dir)
		if err != nil {
			return err
		}
		defer release()
		current, err := load(dir)
		if err != nil {
			return err
		}
		if current.State.BindID != c.State.BindID {
			return errors.New("binding changed; inspect before unbinding")
		}
		if err := c.call(ctx, "unbind", nil, nil); err != nil {
			return err
		}
		for _, name := range []string{"state.json", "credentials", "bootstrap"} {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if o.purge {
			states, err := statePaths(root)
			if err != nil {
				return err
			}
			for _, path := range states {
				var s State
				if readPrivate(path, &s) == nil && s.Runtime == c.State.Runtime {
					return writeJSON(out, map[string]any{"unbound": true, "hooks_preserved": "another local binding uses this runtime"})
				}
			}
			if err := editHooks(root, c.State.Runtime, true); err != nil {
				return err
			}
		}
		return writeJSON(out, map[string]bool{"unbound": true})
	default:
		return fmt.Errorf("unknown relay operation %q", action)
	}
}
func safePart(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func workspace(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel")
	data, err := cmd.Output()
	if err != nil {
		return "", errors.New("relay requires the Room's Git workspace")
	}
	root, err := filepath.EvalSymlinks(strings.TrimSpace(string(data)))
	if err != nil {
		return "", err
	}
	return filepath.Abs(root)
}
func writeJSON(out io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := out.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}
func defaultEndpoint() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "pairroom", relay.EndpointFile), nil
}
func management(ctx context.Context, endpoint relay.Endpoint, method, path string, payload, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(endpoint.URL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid Service request")
	}
	req.Header.Set("Authorization", "Bearer "+endpoint.Token)
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return errors.New("Service unavailable; verify current endpoint file")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 && res.StatusCode != 201 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&e)
		if e.Error == "" {
			e.Error = http.StatusText(res.StatusCode)
		}
		return fmt.Errorf("Service rejected request: %s", e.Error)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(result)
}
func bind(ctx context.Context, root string, o options, out io.Writer) error {
	slot := model.ActorID(o.slot)
	if !slot.ValidParticipant() || !safePart(o.room) {
		return errors.New("bind requires --room and --slot claude|codex")
	}
	if o.endpoint == "" {
		var err error
		o.endpoint, err = defaultEndpoint()
		if err != nil {
			return err
		}
	}
	endpointPath, err := filepath.Abs(o.endpoint)
	if err != nil {
		return err
	}
	endpoint, err := relay.ReadEndpoint(endpointPath)
	if err != nil {
		return err
	}
	var snapshot struct {
		Projects []struct{ ID, Root string }
		Rooms    []struct {
			ID        string
			ProjectID string                                 `json:"project_id"`
			HostMode  model.HostMode                         `json:"host_mode"`
			Agents    map[model.ActorID]model.AgentSelection `json:"agents"`
		}
	}
	if err := management(ctx, endpoint, http.MethodGet, "/api/v1/service", nil, &snapshot); err != nil {
		return err
	}
	var kind model.RuntimeKind
	var project string
	for _, room := range snapshot.Rooms {
		if room.ID == o.room {
			if room.HostMode != model.HostNative {
				return errors.New("bind requires a native-hosted Room")
			}
			kind = room.Agents[slot].Runtime
			for _, p := range snapshot.Projects {
				if p.ID == room.ProjectID {
					project = p.Root
				}
			}
			break
		}
	}
	if kind == "" {
		return errors.New("Room or slot not found")
	}
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil || canonical != root {
		return errors.New("bind must run in the Room's canonical project workspace")
	}
	if o.kind != "" && model.RuntimeKind(o.kind) != kind {
		return errors.New("--runtime does not match the Room's selected runtime")
	}
	if err := installed(root, kind); err != nil {
		return err
	}
	dir, err := secureDir(root, ".pairroom", "rooms", o.room, "slots", o.slot)
	if err != nil {
		return err
	}
	release, err := lockSlot(ctx, dir)
	if err != nil {
		return err
	}
	defer release()
	cleanupAtomicTemps(dir)
	var s State
	var cred credentials
	prior := readPrivate(filepath.Join(dir, "state.json"), &s)
	if prior == nil && !o.replace {
		if err := readPrivate(filepath.Join(dir, "credentials"), &cred); err != nil {
			return err
		}
		if cred.BindID != s.BindID {
			return errors.New("state/credential mismatch: inspect and --replace explicitly")
		}
		if s.SessionID != "" && (!o.cont || o.session != s.SessionID) {
			return relay.ErrOccupied
		}
		if s.Room != o.room || s.Slot != slot || s.Runtime != kind {
			return errors.New("local binding identity mismatch")
		}
	} else {
		if prior != nil && !errors.Is(prior, os.ErrNotExist) && !o.replace {
			return prior
		}
		id, err := relay.RandomID()
		if err != nil {
			return err
		}
		secret, err := relay.RandomID()
		if err != nil {
			return err
		}
		nonce, err := relay.RandomID()
		if err != nil {
			return err
		}
		s = State{Schema: 1, Room: o.room, Slot: slot, Runtime: kind, Workspace: root, EndpointPath: endpointPath, BindID: id, Nonce: "[pairroom-bind:" + nonce + "]"}
		cred = credentials{BindID: id, Secret: secret}
	}
	s.EndpointPath = endpointPath
	if err := ignoreWorkspace(root); err != nil {
		return err
	}
	if err := relay.AtomicJSON(filepath.Join(dir, "credentials"), cred); err != nil {
		return err
	}
	if err := relay.AtomicJSON(filepath.Join(dir, "state.json"), s); err != nil {
		return err
	}
	var result struct {
		Binding                                              relay.Binding `json:"binding"`
		Bootstrap, Collaboration, Workspace, Runtime, Notice string
	}
	// A consumed nonce's hash is still syntactically valid on an idempotent resume;
	// the Service returns the existing binding without creating a new nonce.
	request := relay.BindRequest{BindID: s.BindID, CredentialHash: relay.Digest(cred.Secret), NonceHash: relay.Digest(s.Nonce), SessionID: s.SessionID, Replace: o.replace}
	if err := management(ctx, endpoint, http.MethodPost, "/api/v1/rooms/"+o.room+"/native-bindings/"+o.slot, request, &result); err != nil {
		return err
	}
	if result.Binding.BindID != s.BindID || result.Binding.Generation == 0 {
		return errors.New("binding response identity mismatch")
	}
	s.Generation = result.Binding.Generation
	if err := relay.AtomicJSON(filepath.Join(dir, "state.json"), s); err != nil {
		return err
	}
	return writeJSON(out, map[string]any{"binding": result.Binding, "bind_nonce": s.Nonce, "bootstrap": result.Bootstrap, "collaboration": result.Collaboration, "notice": result.Notice + " Added .pairroom/ to .gitignore. Echo bind_nonce in your visible final reply once; the approved Stop hook will associate this session. No long-lived secret is included."})
}
func (c *Client) upload(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 10<<20 {
		return "", errors.New("attachment must be a regular image, at most 10 MiB")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err = io.Copy(part, io.LimitReader(f, (10<<20)+1)); err != nil {
		return "", err
	}
	if err = writer.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint.URL+"/api/v1/relay/"+c.State.Room+"/"+string(c.State.Slot)+"/upload", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	c.authHeaders(req)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return "", errors.New("relay attachment upload unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 && res.StatusCode != 201 {
		return "", errors.New("attachment rejected by Room validation")
	}
	var value model.Attachment
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(&value); err != nil {
		return "", err
	}
	if value.ID == "" {
		return "", errors.New("attachment receipt missing")
	}
	return value.ID, nil
}
