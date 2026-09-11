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
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

type options struct {
	repo, room, slot, kind, endpoint, text, id, to, session string
	name, peer                                              string
	replace, cont, purge, enabled, discard, resend          bool
	create                                                  bool
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
	flags.StringVar(&o.slot, "slot", "", "Agent slot: 1 or 2; the durable IDs claude|codex are also accepted. Never a runtime name")
	flags.StringVar(&o.kind, "runtime", "", "native harness: claude or codex")
	flags.StringVar(&o.endpoint, "service-file", "", "owner-only relay-endpoint.json path for a custom Service data root")
	flags.StringVar(&o.text, "text", "", "message body; otherwise read stdin")
	flags.StringVar(&o.id, "id", "", "stable client message ID; reuse on uncertain send")
	flags.StringVar(&o.to, "to", "", "explicit send target: @user, or empty for peer")
	flags.StringVar(&o.session, "session-id", "", "associated session identity for explicit --continue")
	flags.BoolVar(&o.create, "create", false, "bind only: register the project when missing, create a native Room, then bind this session")
	flags.StringVar(&o.name, "name", "", "optional Room display name for bind --create")
	flags.StringVar(&o.peer, "peer-runtime", "", "peer slot runtime claude|codex for bind --create")
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
	o.slot = normalizeSlot(o.slot)
	if action == "hook" {
		return runHook(ctx, o, in, out, diagnostic)
	}
	root, err := workspace(ctx, o.repo)
	if err != nil {
		return err
	}
	if action == "install" {
		kind := model.RuntimeKind(o.kind)
		if kind == "" {
			if _, name, ok := harnessAncestor(); ok {
				kind = harnessRuntimes[name]
			}
		}
		if kind != model.RuntimeClaude && kind != model.RuntimeCodex {
			return errors.New("install requires --runtime claude|codex unless run inside a recognized native session")
		}
		if err := editHooks(root, kind, false); err != nil {
			return err
		}
		if err := installSkill(kind); err != nil {
			return err
		}
		return writeJSON(out, map[string]any{"installed": true, "runtime": kind, "notice": "Restart/review the exact project hook in your native harness (Codex: /hooks). This command does not grant native trust. Keep pairroom on PATH. Real authenticated bidirectional E2E remains release-gated.", "next_steps": []string{
			"Create a room and bind this session: pairroom relay bind --create --name \"<topic>\" (skill: /pairroom-relay <topic>)",
			"The peer session joins with the printed peer_join command, or zero-flag inside a recognized session: pairroom relay bind",
			"Echo the returned bind_nonce in your visible reply once so the approved Stop hook associates the session",
		}})
	}
	if action == "bind" {
		return bind(ctx, root, o, out)
	}
	if err := resolveSlotDefaults(root, &o); err != nil {
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
func bind(ctx context.Context, root string, o options, out io.Writer) (resultErr error) {
	slot := model.ActorID(o.slot) // normalized in Run; empty means "infer below"
	if o.slot != "" && !slot.ValidParticipant() {
		return errors.New("bind requires --slot 1|2 (Agent 1/2), or the durable IDs claude|codex")
	}
	if o.create {
		if o.room != "" {
			return errors.New("bind --create creates the Room itself; pass --create or --room, not both")
		}
	} else if o.room != "" && !safePart(o.room) {
		return errors.New("invalid --room value")
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
	var snapshot serviceSnapshot
	if err := management(ctx, endpoint, http.MethodGet, "/api/v1/service", nil, &snapshot); err != nil {
		return err
	}
	created := false
	defer func() {
		if created && resultErr != nil {
			resultErr = fmt.Errorf("Room %s was created but binding failed: %w; finish setup, then recover with %s --replace (revokes any pending binding; cannot stop native work). Do not repeat --create", o.room, resultErr, bindCommand(root, endpointPath, o.room, slot))
		}
	}()
	if o.create {
		switch {
		case slot != "":
		case o.kind != "" || o.peer != "":
			// An explicit runtime selection is placed at the slot whose default
			// runtime matches it, so that slot must be known before creation.
			inferred, err := inferCreateSlot(o)
			if err != nil {
				return err
			}
			slot = inferred
			o.slot = string(slot)
		case callerRuntime(o) == "":
			// No slot can ever be resolved for this caller; fail before the
			// Service creates durable state.
			return errCreateSlotUnresolved
		}
		// Otherwise the Service owns the default pair. That pair is user
		// configuration (agent-pair-profiles.json, then config defaults) and
		// need not match slot order, so the slot stays unresolved here and is
		// matched against the created Room's real selections below.
		agents, err := createAgents(o, slot)
		if err != nil {
			return err
		}
		if agents != nil {
			if err := installed(root, agents[slot].Runtime); err != nil {
				return err
			}
		} else if rt := callerRuntime(o); rt != "" {
			// The Service owns the default pair, so the slot is only resolved
			// after creation. The caller's own harness is already known: reject a
			// workspace that could never associate this session before the
			// Service creates durable state.
			if err := installed(root, rt); err != nil {
				return err
			}
		} else if installed(root, model.RuntimeClaude) != nil && installed(root, model.RuntimeCodex) != nil {
			// Do not guess the pair, but reject a workspace with no usable relay
			// hook before creating durable state.
			return errors.New("bind --create requires an approved relay Stop hook; run pairroom relay install --runtime claude|codex for the intended harness first")
		}
		room, err := createNativeRoom(ctx, endpoint, root, o, slot)
		if err != nil {
			return err
		}
		o.room = room
		created = true
		// The refreshed snapshot must include the just-created Room.
		if err := management(ctx, endpoint, http.MethodGet, "/api/v1/service", nil, &snapshot); err != nil {
			return err
		}
	} else if o.room == "" {
		roomID, err := resolveNativeRoom(snapshot, root)
		if err != nil {
			return err
		}
		o.room = roomID
	}
	if slot == "" {
		target, ok := snapshot.findRoom(o.room)
		if !ok || target.HostMode != model.HostNative {
			return errors.New("bind requires a native-hosted Room")
		}
		inferred, err := resolveSlotForRoom(target, callerRuntime(o))
		if err != nil {
			return err
		}
		slot = inferred
		o.slot = string(slot)
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
		if s.SessionID == "" {
			return errors.New("slot has a pending binding; finish association with the nonce already returned to its original session, or use bind --replace explicitly to revoke it and start again")
		}
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
	// Best-effort lineage for foreground default selection in multi-binding
	// workspaces; never authentication material.
	if pid, name, ok := harnessAncestor(); ok {
		s.HarnessPID, s.HarnessName = pid, name
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
	payload := map[string]any{"binding": result.Binding, "bind_nonce": s.Nonce, "bootstrap": result.Bootstrap, "collaboration": result.Collaboration, "notice": result.Notice + " Added .pairroom/ to .gitignore. Echo bind_nonce in your visible final reply once; the approved Stop hook will associate this session. No long-lived secret is included."}
	if created {
		payload["peer_join"] = bindCommand(root, endpointPath, o.room, peerSlot(slot))
	}
	return writeJSON(out, payload)
}

// Commands are pasted into the native PowerShell (Windows) or POSIX shell.
// Quote paths as literals so spaces, quotes and shell expansion are preserved.
func quoteShellPath(value string) string {
	if runtime.GOOS == "windows" {
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// bindCommand renders a literal, paste-safe bind command. An unresolved slot is
// omitted rather than rendered as a guessed Agent number; the accompanying
// error lists the real candidates.
func bindCommand(root, endpoint, room string, slot model.ActorID) string {
	command := "pairroom relay bind --room " + room
	if slot.ValidParticipant() {
		command += " --slot " + strconv.Itoa(slotNumber(slot))
	}
	return command + " --service-file " + quoteShellPath(endpoint) + " --repo " + quoteShellPath(root)
}

// serviceSnapshot is the read-only Management projection used for Room and
// slot resolution. Resolution only selects convenience; binding still presents
// credentials and the hook path still requires the official session identity.
type serviceSnapshot struct {
	Projects []serviceProject `json:"projects"`
	Rooms    []serviceRoom    `json:"rooms"`
}

type serviceProject struct {
	ID   string `json:"id"`
	Root string `json:"root"`
}

type serviceRoom struct {
	ID        string                                 `json:"id"`
	ProjectID string                                 `json:"project_id"`
	HostMode  model.HostMode                         `json:"host_mode"`
	Lifecycle string                                 `json:"lifecycle"`
	Agents    map[model.ActorID]model.AgentSelection `json:"agents"`
}

// roomLifecycleActive mirrors the Service's active Room lifecycle. The relay
// client reads the projected JSON only and stays decoupled from the Service
// package; the Service remains the authority and rejects bindings itself.
const roomLifecycleActive = "active"

func (s serviceSnapshot) findRoom(id string) (serviceRoom, bool) {
	for _, room := range s.Rooms {
		if room.ID == id {
			return room, true
		}
	}
	return serviceRoom{}, false
}

// normalizeSlot maps the Agent-number UX onto the durable participant IDs.
// Slot 1/2 are the primary names; claude/codex remain accepted durable IDs and
// never denote the runtime.
func normalizeSlot(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "agent1", "claude":
		return string(model.ActorClaude)
	case "2", "agent2", "codex":
		return string(model.ActorCodex)
	}
	return value
}

func slotNumber(slot model.ActorID) int {
	if slot == model.ActorClaude {
		return 1
	}
	return 2
}

// callerRuntime resolves the runtime the caller actually is: an explicit
// --runtime wins, otherwise the recognized native harness lineage.
func callerRuntime(o options) model.RuntimeKind {
	if o.kind != "" {
		return model.RuntimeKind(o.kind)
	}
	if _, name, ok := harnessAncestor(); ok {
		return harnessRuntimes[name]
	}
	return ""
}

// resolveNativeRoom selects the unique active native Room of the canonical
// workspace. Archived Rooms are never candidates: the Service rejects their
// bindings, so counting them would either block a workspace that has exactly
// one usable Room or offer an unusable one.
func resolveNativeRoom(snapshot serviceSnapshot, root string) (string, error) {
	roots := make(map[string]string, len(snapshot.Projects))
	for _, p := range snapshot.Projects {
		roots[p.ID] = p.Root
	}
	var ids []string
	for _, room := range snapshot.Rooms {
		if room.HostMode == model.HostNative && room.Lifecycle == roomLifecycleActive && roots[room.ProjectID] == root {
			ids = append(ids, room.ID)
		}
	}
	switch len(ids) {
	case 1:
		if !safePart(ids[0]) {
			return "", errors.New("Service returned an unusable Room ID")
		}
		return ids[0], nil
	case 0:
		return "", errors.New("no active native Room exists for this workspace; create one with pairroom relay bind --create, or pass --room")
	}
	sort.Strings(ids)
	return "", fmt.Errorf("multiple active native Rooms match this workspace; pass --room explicitly: %s", strings.Join(ids, ", "))
}

// resolveSlotForRoom infers the caller's slot only when exactly one slot of
// the Room runs the caller's runtime; every other case fails with candidates.
func resolveSlotForRoom(room serviceRoom, rt model.RuntimeKind) (model.ActorID, error) {
	if rt == "" {
		return "", errors.New("bind requires --slot 1|2 (Agent 1/2) outside a recognized native session")
	}
	order := []model.ActorID{model.ActorClaude, model.ActorCodex}
	var matches []model.ActorID
	for _, slot := range order {
		if sel, ok := room.Agents[slot]; ok && sel.Runtime == rt {
			matches = append(matches, slot)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	desc := make([]string, 0, len(order))
	for _, slot := range order {
		if sel, ok := room.Agents[slot]; ok {
			desc = append(desc, fmt.Sprintf("--slot %d = %s runtime", slotNumber(slot), sel.Runtime))
		}
	}
	return "", fmt.Errorf("the %q harness does not match exactly one slot of Room %s (%s); pass --slot 1|2", rt, room.ID, strings.Join(desc, ", "))
}

// errCreateSlotUnresolved is returned when a creator slot cannot be known
// before the Room exists. It never authorizes a guess.
var errCreateSlotUnresolved = errors.New("bind --create requires --slot 1|2 unless run inside a recognized claude/codex session (or with --runtime claude|codex)")

// inferCreateSlot picks the slot that an explicit runtime selection occupies:
// the caller's runtime takes the slot whose default runtime matches. It is only
// used when `createAgents` will send explicit selections, because those are
// placed by slot. When the Service owns the default pair the slot is resolved
// after creation against the Room's real selections instead.
func inferCreateSlot(o options) (model.ActorID, error) {
	rt := callerRuntime(o)
	switch rt {
	case model.RuntimeClaude:
		return model.ActorClaude, nil
	case model.RuntimeCodex:
		return model.ActorCodex, nil
	}
	return "", errCreateSlotUnresolved
}

// resolveSlotDefaults fills omitted --room/--slot for per-slot foreground
// commands. Explicit flags always win. When both are omitted, a recognized
// caller must match a unique recorded native-harness lineage, even with one
// binding. An unrecognized caller may use the sole workspace binding; ambiguity fails
// with the candidate list. Lineage only selects convenience: every call still
// presents the slot's owner-only credential, and the hook path still requires
// the associated official session identity.
func resolveSlotDefaults(root string, o *options) error {
	if model.ActorID(o.slot).ValidParticipant() && safePart(o.room) {
		return nil
	}
	if o.slot != "" || o.room != "" {
		return errors.New("--room and --slot claude|codex are required")
	}
	paths, err := statePaths(root)
	if err != nil {
		return err
	}
	type candidate struct {
		room       string
		slot       model.ActorID
		harnessPID int
		harness    string
	}
	var all []candidate
	for _, path := range paths {
		var s State
		if err := readPrivate(path, &s); err != nil {
			return err
		}
		if !s.Slot.ValidParticipant() || !safePart(s.Room) {
			continue
		}
		all = append(all, candidate{room: s.Room, slot: s.Slot, harnessPID: s.HarnessPID, harness: s.HarnessName})
	}
	if len(all) == 0 {
		return errors.New("no relay binding in this workspace; run pairroom relay bind first")
	}
	matched := all
	if pid, name, ok := harnessAncestor(); ok {
		matched = nil
		for _, c := range all {
			if c.harnessPID == pid && strings.EqualFold(c.harness, name) {
				matched = append(matched, c)
			}
		}
	}
	if len(matched) != 1 {
		list := make([]string, 0, len(all))
		for _, c := range all {
			list = append(list, "--room "+c.room+" --slot "+string(c.slot))
		}
		return fmt.Errorf("no unique relay binding matches this caller; pass one explicitly: %s", strings.Join(list, " | "))
	}
	o.room = matched[0].room
	o.slot = string(matched[0].slot)
	return nil
}

// createNativeRoom registers the workspace Project when missing and creates a
// native-hosted Room through the same validated Management path the browser
// uses. It never bypasses creation-time validation or writes Room state
// directly. Omitted runtimes keep the Service default pair; explicit runtime
// overrides stay empty-field selections that inherit the native configuration.
func createNativeRoom(ctx context.Context, endpoint relay.Endpoint, root string, o options, slot model.ActorID) (string, error) {
	agents, err := createAgents(o, slot)
	if err != nil {
		return "", err
	}
	var snapshot serviceSnapshot
	if err := management(ctx, endpoint, http.MethodGet, "/api/v1/service", nil, &snapshot); err != nil {
		return "", err
	}
	project := ""
	for _, p := range snapshot.Projects {
		if p.Root == root {
			project = p.ID
			break
		}
	}
	if project == "" {
		var registered struct{ ID string }
		if err := management(ctx, endpoint, http.MethodPost, "/api/v1/projects", map[string]string{"path": root}, &registered); err != nil {
			return "", err
		}
		project = registered.ID
	}
	request := map[string]any{"host_mode": string(model.HostNative)}
	if name := strings.TrimSpace(o.name); name != "" {
		request["name"] = name
	}
	if agents != nil {
		request["agents"] = agents
	}
	var room struct{ ID string }
	if err := management(ctx, endpoint, http.MethodPost, "/api/v1/projects/"+project+"/rooms", request, &room); err != nil {
		return "", fmt.Errorf("Room creation was not confirmed: %w; inspect Management for an already-created Room before repeating --create", err)
	}
	if !safePart(room.ID) {
		return "", errors.New("Service returned an unusable Room ID")
	}
	return room.ID, nil
}

func createAgents(o options, slot model.ActorID) (map[model.ActorID]model.AgentSelection, error) {
	own, peer := model.RuntimeKind(o.kind), model.RuntimeKind(o.peer)
	if own == "" && peer == "" {
		return nil, nil
	}
	if own == "" {
		own = defaultRuntimeFor(slot)
	}
	if peer == "" {
		peer = defaultRuntimeFor(peerSlot(slot))
	}
	for _, kind := range []model.RuntimeKind{own, peer} {
		if kind != model.RuntimeClaude && kind != model.RuntimeCodex {
			return nil, errors.New("native Rooms support --runtime/--peer-runtime claude or codex only")
		}
	}
	return map[model.ActorID]model.AgentSelection{
		slot:           {Runtime: own},
		peerSlot(slot): {Runtime: peer},
	}, nil
}

func peerSlot(slot model.ActorID) model.ActorID {
	if slot == model.ActorClaude {
		return model.ActorCodex
	}
	return model.ActorClaude
}

func defaultRuntimeFor(slot model.ActorID) model.RuntimeKind {
	if slot == model.ActorCodex {
		return model.RuntimeCodex
	}
	return model.RuntimeClaude
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
