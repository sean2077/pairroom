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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/claudewake"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/review"
	"github.com/sean2077/pairroom/internal/version"
)

type options struct {
	review                                         bool
	reviewRepo, reviewBase                         string
	repo, room, slot, kind, endpoint, text, id, to string
	name, peer                                     string
	textFile, outputFile                           string
	cursor, since                                  string
	limit                                          int
	pending                                        bool
	references                                     stringsFlag
	replace, purge, enabled, discard, resend       bool
	localOnly                                      bool
	create, brief                                  bool
	repoExplicit                                   bool
	timeout                                        int
	attachments                                    stringsFlag
	preparedAgents                                 map[model.ActorID]model.AgentSelection
}
type stringsFlag []string

func (s *stringsFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringsFlag) Set(v string) error { *s = append(*s, v); return nil }

// relayActions lists every relay operation Run accepts, in usage order.
var relayActions = []string{"install", "preflight", "bind", "hook", "send", "exchange", "wait", "status", "history", "doctor", "review", "peer", "park", "nudge", "reconcile", "unbind"}

func relayUsage() string {
	return "use pairroom relay " + strings.Join(relayActions, "|") + " (see docs/CLI_REFERENCE.md)"
}

// Run executes one relay subcommand. Afterwards it prints at most one stderr
// line when a Service response named a release other than this CLI's; stdout
// stays the machine-readable handoff channel. Preflight reports the release
// itself.
func Run(ctx context.Context, args []string, in io.Reader, out, diagnostic io.Writer) error {
	ctx, observed := withServiceObservation(ctx)
	err := run(ctx, args, in, out, diagnostic)
	if len(args) == 0 || args[0] == "preflight" {
		return err
	}
	// A skew-shaped failure may happen before any Service contact, such as a
	// binding the older CLI cannot find. Only then, and never in the Stop hook,
	// spend one bounded read to learn the release.
	if args[0] != "hook" && observed.needsProbe(err) {
		probeServiceRelease(ctx, observed.endpointPath())
	}
	if hint := observed.hint(err); hint != "" {
		_, _ = fmt.Fprintln(diagnostic, hint)
	}
	return err
}

func run(ctx context.Context, args []string, in io.Reader, out, diagnostic io.Writer) error {
	if len(args) == 0 {
		return errors.New(relayUsage())
	}
	action := args[0]
	switch action {
	case "help", "--help", "-h":
		_, err := fmt.Fprintf(out, "usage: pairroom relay <command> [flags]\ncommands: %s\nrun pairroom relay <command> --help for flags; see docs/CLI_REFERENCE.md\n", strings.Join(relayActions, ", "))
		return err
	}
	if !slices.Contains(relayActions, action) {
		return fmt.Errorf("unknown relay operation %q; %s", action, relayUsage())
	}
	o := options{}
	flags := flag.NewFlagSet("pairroom relay "+action, flag.ContinueOnError)
	flags.SetOutput(diagnostic)
	flags.StringVar(&o.repo, "repo", ".", "Room project path; defaults to this native session's binding, then workspace discovery")
	flags.StringVar(&o.room, "room", "", "Room ID")
	flags.StringVar(&o.slot, "slot", "", "Agent slot: 1 or 2 (bind --create defaults to 1); claude/codex are CLI input aliases only. Never a runtime name")
	flags.StringVar(&o.kind, "runtime", "", "native harness: claude (cc), codex or grok; install accepts a comma-separated list")
	flags.StringVar(&o.endpoint, "service-file", "", "owner-only relay-endpoint.json path for a custom Service data root")
	flags.StringVar(&o.text, "text", "", "message body; otherwise read stdin unless --text-file or --ref is used")
	flags.StringVar(&o.textFile, "text-file", "", "send/exchange: read UTF-8 body from a file, or - for stdin")
	flags.Var(&o.references, "ref", "send/exchange: local file reference with path, size and SHA-256, not an upload (repeatable)")
	flags.StringVar(&o.outputFile, "output-file", "", "wait/exchange: save the incoming envelope to a new private file and print only its receipt; parent must exist and be writable")
	flags.StringVar(&o.id, "id", "", "stable client message ID (required for exchange); reuse on uncertain send")
	flags.StringVar(&o.to, "to", "", "explicit send target: @user, or empty for peer")
	flags.BoolVar(&o.create, "create", false, "bind only: register the project when missing, create a native Room, then bind this session")
	flags.StringVar(&o.name, "name", "", "optional Room display name for bind --create")
	flags.StringVar(&o.peer, "peer-runtime", "", "peer slot runtime claude|codex|grok for bind --create")
	flags.BoolVar(&o.replace, "replace", false, "explicitly revoke occupied binding; does not stop native work")
	flags.BoolVar(&o.purge, "purge-hooks", false, "remove this runtime's relay hooks when no other local binding uses them")
	flags.BoolVar(&o.enabled, "enabled", true, "park enabled")
	flags.BoolVar(&o.brief, "brief", action == "status" || action == "reconcile", "status/reconcile: bounded transport summary; --brief=false includes full history")
	flags.BoolVar(&o.discard, "discard", false, "explicitly discard uncertain pending publication, retaining its consumed sequence")
	flags.BoolVar(&o.resend, "resend", false, "explicitly supplement uncertain pending with its ORIGINAL sequence")
	flags.BoolVar(&o.localOnly, "local-only", false, "unbind: remove local binding files without contacting the Service; the server-side binding stays active until an explicit unbind or replace")
	defaultTimeout := 30
	if action == "wait" || action == "exchange" {
		defaultTimeout = 3600
	}
	flags.IntVar(&o.timeout, "timeout", defaultTimeout, "foreground wait seconds (0 or 1–21600); 0 waits until cancellation; hook park remains at most 30 seconds")
	flags.Var(&o.attachments, "attach", "image attachment path (repeatable)")
	flags.BoolVar(&o.review, "review", false, "send/exchange: attach a bounded Git review observation")
	flags.StringVar(&o.reviewRepo, "review-repo", "", "review evidence checkout; defaults to the bound Room workspace (does not rebind)")
	flags.StringVar(&o.reviewBase, "review-base", "", "send/exchange --review: base commit/ref, default HEAD")
	flags.StringVar(&o.cursor, "cursor", "", "history: opaque next_cursor from a previous page")
	flags.StringVar(&o.since, "since", "", "history: minimum publication time (RFC3339)")
	flags.IntVar(&o.limit, "limit", 50, "history: page size (1–100; also text-budgeted)")
	flags.BoolVar(&o.pending, "pending", false, "history: oldest unresolved messages, independently of recent chat")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected relay arguments")
	}
	provided := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	if (provided["review"] || provided["review-base"]) && (action != "send" && action != "exchange") {
		return errors.New("--review/--review-base apply only to send/exchange")
	}
	if provided["review-base"] && !o.review {
		return errors.New("--review-base requires --review")
	}
	if provided["review-repo"] && action != "review" && !o.review {
		return errors.New("--review-repo requires review or send/exchange --review")
	}
	if action == "review" && o.id == "" {
		return errors.New("review requires the published message --id")
	}
	if provided["cursor"] || provided["since"] || provided["limit"] || provided["pending"] {
		if action != "history" {
			return errors.New("history filters apply only to history")
		}
	}
	if action == "history" {
		if o.limit < 1 || o.limit > relay.HistoryPageLimit {
			return errors.New("history limit must be 1–100")
		}
		if o.id != "" && (o.cursor != "" || o.pending || o.since != "" || provided["limit"]) {
			return errors.New("choose --id or a history page")
		}
		if o.since != "" {
			if _, err := time.Parse(time.RFC3339, o.since); err != nil {
				return errors.New("--since must be RFC3339")
			}
		}
	}
	o.repoExplicit = provided["repo"]
	if provided["text-file"] || provided["ref"] {
		if action != "send" && action != "exchange" {
			return errors.New("--text-file and --ref apply only to send or exchange")
		}
	}
	if provided["text-file"] {
		if provided["text"] {
			return errors.New("choose --text or --text-file, not both")
		}
		if o.textFile == "" {
			return errors.New("--text-file requires a nonempty path, or - for stdin")
		}
	}
	if provided["output-file"] {
		if action != "wait" && action != "exchange" {
			return errors.New("--output-file applies only to wait or exchange")
		}
		// Preflight before a send or claim; actual creation is still exclusive.
		writer, err := newEnvelopeFileWriter(o.outputFile, out)
		if err != nil {
			return err
		}
		o.outputFile = writer.path
		out = writer
	}
	if o.brief && action != "status" && action != "reconcile" {
		return errors.New("--brief applies only to status or reconcile")
	}
	if o.localOnly && action != "unbind" {
		return errors.New("--local-only applies only to unbind")
	}
	// Reject collection options before workspace I/O or any publication.
	if action == "wait" || action == "exchange" {
		if err := validateForegroundTimeout(o.timeout); err != nil {
			return err
		}
	}
	if action == "exchange" {
		if !safePart(o.id) || len(o.id) > 128 {
			return errors.New("exchange requires a stable --id (1–128 letters, digits, '-' or '_'); reuse it only for the same publication")
		}
		if o.to != "" {
			return errors.New("exchange sends to the peer only; use send --to @user for escalation")
		}
	}
	// Use the same documented runtime vocabulary for setup and session commands.
	// Validate before workspace discovery or creation can have side effects.
	for _, runtimeFlag := range []struct {
		name  string
		value *string
	}{{"--runtime", &o.kind}, {"--peer-runtime", &o.peer}} {
		if *runtimeFlag.value == "" || action == "install" && runtimeFlag.name == "--runtime" {
			continue
		}
		kind, ok := parseRuntimeToken(*runtimeFlag.value)
		if !ok {
			return fmt.Errorf("invalid %s; choose claude (cc), codex, or grok", runtimeFlag.name)
		}
		*runtimeFlag.value = string(kind)
	}
	o.slot = normalizeSlot(o.slot)
	noteServiceEndpoint(ctx, o.endpoint)
	if action == "hook" {
		return runHook(ctx, o, in, out, diagnostic)
	}
	// Preflight runs before binding exists, so it must not use session routing.
	if action == "preflight" {
		return runPreflight(ctx, o, out)
	}
	root, err := resolveCommandWorkspace(ctx, action, &o)
	if err != nil {
		return err
	}
	if err := applyCallerDefaults(root, action, &o); err != nil {
		return err
	}
	if action == "install" {
		kinds, err := selectInstallRuntimes(o.kind, in, diagnostic)
		if err != nil {
			return err
		}
		return runInstall(root, kinds, in, out, diagnostic)
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
	if action == "unbind" && o.localOnly {
		// Offline path: never reads the endpoint file, so a stopped or
		// unreachable Service cannot block local cleanup.
		return unbindLocalOnly(ctx, root, dir, o, out)
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
	noteServiceEndpoint(ctx, c.State.EndpointPath)
	if err := validateCommandCaller(c); err != nil {
		release()
		return err
	}
	if err := rememberSession(c.State); err != nil {
		_, _ = fmt.Fprintln(diagnostic, "PairRoom: session locator unavailable; use --repo for this binding until repaired.")
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
	if action == "wait" || action == "exchange" {
		releaseCollector, err := acquireCollector(ctx, dir)
		if err != nil {
			return err
		}
		defer releaseCollector()
	}
	switch action {
	case "send", "exchange":
		text, err := readPublicationInput(publicationInput{
			text:       o.text,
			textSet:    provided["text"],
			textFile:   o.textFile,
			references: o.references,
		}, in, relay.MaxBodyBytes)
		if err != nil {
			return err
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
		var anchor *review.Anchor
		if o.review {
			workspace := o.reviewRepo
			if workspace == "" {
				workspace = root
			}
			value, e := review.Capture(ctx, workspace, o.reviewBase)
			if e != nil {
				return e
			}
			anchor = &value
		}
		var msg relay.Message
		err = c.call(ctx, "send", map[string]any{"id": o.id, "text": text, "to": to, "attachment_ids": attachments, "review": anchor}, &msg)
		if relayErrorCode(err) == relay.SendPayloadConflictCode {
			// Settled, not uncertain: this ID already names an accepted message.
			return fmt.Errorf("--id %s was already used for a different message (body, target, attachments, quote or review); nothing new was published. Check relay history for the original; send new content with a new --id", o.id)
		}
		if err != nil {
			return fmt.Errorf("%w; publication uncertain: retry with the SAME --id %s, not a new ID", err, o.id)
		}
		if err := validatePublicationReceipt(c, o, msg, text); err != nil {
			return err
		}
		if action == "exchange" {
			return finishExchange(ctx, c, o, msg, out, diagnostic)
		}
		receipt := publicationReceipt{Published: msg.ID, ClientID: o.id, State: msg.State, To: msg.To}
		if err := writeJSON(out, receipt); err != nil {
			return fmt.Errorf("publication %s confirmed but receipt output failed: %w; recover only with the SAME --id %s", msg.ID, err, o.id)
		}
		writeQueuedDeliveryHint(diagnostic, c, msg)
		return nil
	case "wait":
		_, err := deliverForeground(ctx, c, o.timeout, out)
		return err
	case "history":
		var since time.Time
		if o.since != "" {
			since, _ = time.Parse(time.RFC3339, o.since)
		}
		var page relay.HistoryPage
		if err := c.call(ctx, "history", relay.HistoryQuery{ID: o.id, Cursor: o.cursor, Limit: o.limit, Pending: o.pending, Since: since}, &page); err != nil {
			return err
		}
		return writeJSON(out, page)
	case "review":
		var page relay.HistoryPage
		if err := c.call(ctx, "history", relay.HistoryQuery{ID: o.id}, &page); err != nil {
			return err
		}
		if len(page.Messages) != 1 || page.Messages[0].Review == nil {
			return errors.New("message has no review anchor")
		}
		workspace := o.reviewRepo
		if workspace == "" {
			workspace = root
		}
		return writeJSON(out, map[string]any{"id": o.id, "review": page.Messages[0].Review, "status": review.Check(ctx, workspace, *page.Messages[0].Review), "notice": "Observation only; unchanged evidence is not approval. Ignored files require explicit --ref evidence."})
	case "doctor":
		var report map[string]any
		if err := c.call(ctx, "doctor", nil, &report); err != nil {
			if relayErrorCode(err) == "runtime_not_ready" {
				// Doctor deliberately never activates a Room: activation resumes
				// Service-managed wake, which a read-only diagnosis must not start.
				return fmt.Errorf("%w: the Native Room is suspended (for example after a Service restart or idle timeout) and doctor never activates it. Run pairroom relay status --brief --room %s --slot %s, which activates it, or open the Room in Management, then rerun doctor", err, o.room, o.slot)
			}
			return err
		}
		hook := "installed"
		if installed(root, c.State.Runtime) != nil {
			hook = "missing_or_disabled"
		}
		local := map[string]any{"cli_version": version.Current, "protocol": protocol.NativeVersion, "protocol_match": report["protocol"] == protocol.NativeVersion, "service_version_match": report["service_version"] == version.Current, "workspace_match": root == c.State.Workspace, "hook_installation": hook, "hook_approval": "unknown", "last_hook_at": c.State.LastHookAt}
		if hint := hookNotRunHint(c.State); hint != "" {
			local["hook_hint"] = hint
		}
		report["local"] = local
		return writeJSON(out, report)
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
		return writeJSON(out, map[string]string{"notice": "Automatic wake is Service-managed for eligible Claude/Codex sessions. This receive-only fallback runs inside the associated session; relay doctor explains the current boundary.", "command": fmt.Sprintf("pairroom relay wait --room %s --slot %s", o.room, o.slot)})
	case "status", "reconcile":
		var status any = &relay.Snapshot{}
		operation := "status"
		if o.brief {
			status = &relay.Summary{}
			operation = "summary"
		}
		if e := c.call(ctx, operation, nil, status); e != nil {
			return e
		}
		local := map[string]any{"last_confirmed_seq": c.State.LastConfirmedSeq, "last_seq": c.State.LastSeq, "publication_unknown": errors.Is(err, relay.ErrUnknown), "binding_workspace": c.State.Workspace}
		if c.State.LastHookAt != "" {
			local["last_hook_at"] = c.State.LastHookAt
		} else {
			local["hook_hint"] = hookNotRunHint(c.State)
		}
		if c.State.Pending != nil {
			local["pending_seq"] = c.State.Pending.Seq
			local["publication_unknown"] = c.State.Pending.Unknown
		}
		if len(c.State.Held) > 0 {
			local["held_publications"] = len(c.State.Held)
		}
		result := map[string]any{"local": local, "relay": status}
		if action == "status" && o.brief {
			if summary, ok := status.(*relay.Summary); ok {
				if hints := queuedInboxHints(ctx, c, summary); len(hints) > 0 {
					result["queued_inbox_hints"] = hints
				}
			}
		}
		return writeJSON(out, result)
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
		for _, name := range []string{"state.json", "credentials", "bootstrap", bindAttemptFile, claudewake.FileName} {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := forgetSession(current.State); err != nil {
			return fmt.Errorf("unbound, but session locator cleanup failed: %w", err)
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

// unbindLocalOnly removes this slot's local binding files without contacting
// the Service. The server-side binding and its generation remain ACTIVE — the
// slot stays occupied and only loses its local credentials — until an explicit
// `pairroom relay unbind` or a `bind --replace` from the intended session.
func unbindLocalOnly(ctx context.Context, root, dir string, o options, out io.Writer) error {
	release, err := lockSlot(ctx, dir)
	if err != nil {
		return err
	}
	defer release()
	cleanupAtomicTemps(dir)
	var state State
	if err := readPrivate(filepath.Join(dir, "state.json"), &state); err != nil {
		return fmt.Errorf("read local binding state: %w", err)
	}
	if err := validateCommandCaller(&Client{State: state}); err != nil {
		return err
	}
	if state.Schema != 2 || !state.Slot.ValidParticipant() {
		return errors.New("invalid local relay state identity")
	}
	for _, name := range []string{"state.json", "credentials", "bootstrap", bindAttemptFile, claudewake.FileName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := forgetSession(state); err != nil {
		return fmt.Errorf("locally unbound, but session locator cleanup failed: %w", err)
	}
	result := map[string]any{
		"unbound": "local-only",
		"notice":  "local binding files removed without contacting the Service; the server-side binding stays active and the slot remains occupied until an explicit `pairroom relay unbind` or `pairroom relay bind --replace`",
	}
	if o.purge {
		states, err := statePaths(root)
		if err != nil {
			return err
		}
		for _, path := range states {
			var s State
			if readPrivate(path, &s) == nil && s.Runtime == state.Runtime {
				result["hooks_preserved"] = "another local binding uses this runtime"
				return writeJSON(out, result)
			}
		}
		if err := editHooks(root, state.Runtime, true); err != nil {
			return err
		}
	}
	return writeJSON(out, result)
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
	observeServiceResponse(ctx, res)
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
	if method != http.MethodGet || path != "/api/v1/service" {
		return json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(result)
	}
	// The snapshot's display version also names a Service that predates the
	// release response header.
	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return err
	}
	var snapshot struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &snapshot) == nil {
		observeServiceSnapshotVersion(ctx, snapshot.Version)
	}
	return json.Unmarshal(body, result)
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

func localBindCommand(endpoint, room string, slot model.ActorID) string {
	command := fmt.Sprintf("pairroom relay bind --room %s --slot %d", room, slotNumber(slot))
	// A fresh peer cannot infer a custom Service root from the creator's binding.
	// Omit only the default endpoint, never a selector needed to find this Room.
	standard, err := defaultEndpoint()
	if err != nil || filepath.Clean(endpoint) != filepath.Clean(standard) {
		command += " --service-file " + quoteShellPath(endpoint)
	}
	return command
}

func createRetryCommand(root, endpoint string, o options) string {
	command := "pairroom relay bind --create"
	if name := strings.TrimSpace(o.name); name != "" {
		command += " --name " + quoteShellPath(name)
	}
	command += " --peer-runtime <claude|codex|grok>"
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

// normalizeSlot maps CLI-only aliases to durable participant IDs before any
// state or request is persisted. claude/codex never denote a durable slot.
func normalizeSlot(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "slot1", "1", "agent1", "claude":
		return string(model.ActorSlot1)
	case "slot2", "2", "agent2", "codex":
		return string(model.ActorSlot2)
	}
	return normalized
}

func slotNumber(slot model.ActorID) int {
	if slot == model.ActorSlot1 {
		return 1
	}
	return 2
}

// callerRuntime resolves the runtime the caller actually is: an explicit
// --runtime wins after caller consistency checks; otherwise use native session
// metadata or the recognized harness lineage. Neither grants association.
func callerRuntime(o options) model.RuntimeKind {
	if o.kind != "" {
		return model.RuntimeKind(o.kind)
	}
	caller, err := currentNativeCaller()
	if err != nil {
		return ""
	}
	return caller.runtime
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
	order := []model.ActorID{model.ActorSlot1, model.ActorSlot2}
	matches := runtimeSlots(room.Agents, rt)
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
var errCreateSlotUnresolved = errors.New("bind --create requires --slot 1|2 unless run inside a recognized claude/codex/grok session (or with --runtime claude|codex|grok)")

// inferCreateSlot defaults every recognized creator to Agent 1. Service pair
// selections are oriented to that slot before creation; an explicit --slot is
// handled by the caller and never inferred from a runtime name.
func inferCreateSlot(o options) (model.ActorID, error) {
	switch callerRuntime(o) {
	case model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok:
		return model.ActorSlot1, nil
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
	o.slot = normalizeSlot(o.slot)
	if model.ActorID(o.slot).ValidParticipant() && safePart(o.room) {
		return nil
	}
	if o.slot != "" || o.room != "" {
		return errors.New("--room and --slot 1|2 are required")
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
		if s.Schema != 2 || !s.Slot.ValidParticipant() || !safePart(s.Room) || s.Generation == 0 || s.SessionID == "" {
			continue
		}
		all = append(all, candidate{room: s.Room, slot: s.Slot, harnessPID: s.HarnessPID, harness: s.HarnessName})
	}
	if len(all) == 0 {
		return errNoWorkspaceBinding
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
// directly. Preflight pins Service defaults in creator-first order; explicit
// runtime overrides inherit the native configuration through empty fields.
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
	if o.preparedAgents != nil {
		return o.preparedAgents, nil
	}
	own, peer := model.RuntimeKind(o.kind), model.RuntimeKind(o.peer)
	if own == "" && peer == "" {
		return nil, nil
	}
	if own == "" {
		own = callerRuntime(o)
		if own == "" {
			own = defaultRuntimeFor(slot)
		}
	}
	if peer == "" {
		peer = defaultRuntimeFor(peerSlot(slot))
		if peer == own {
			// Moving a Codex creator to Agent 1 must not accidentally create a
			// Codex/Codex pair. Duplicate runtimes remain an explicit choice.
			peer = defaultRuntimeFor(slot)
		}
	}
	for _, kind := range []model.RuntimeKind{own, peer} {
		if kind != model.RuntimeClaude && kind != model.RuntimeCodex && kind != model.RuntimeGrok {
			return nil, errors.New("native Rooms support --runtime/--peer-runtime claude, codex or grok")
		}
	}
	return map[model.ActorID]model.AgentSelection{
		slot:           {Runtime: own},
		peerSlot(slot): {Runtime: peer},
	}, nil
}

func peerSlot(slot model.ActorID) model.ActorID {
	if slot == model.ActorSlot1 {
		return model.ActorSlot2
	}
	return model.ActorSlot1
}

func defaultRuntimeFor(slot model.ActorID) model.RuntimeKind {
	if slot == model.ActorSlot2 {
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
	observeServiceResponse(ctx, res)
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
