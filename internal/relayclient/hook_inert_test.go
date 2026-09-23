package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func inertHookPayload(t *testing.T, kind model.RuntimeKind, event, session, cwd string) []byte {
	t.Helper()
	fields := map[string]any{
		"hook_event_name": event, "session_id": session, "cwd": cwd,
		"last_assistant_message": "private unbound reply @claude @codex @grok",
	}
	if kind == model.RuntimeGrok {
		name := "stop"
		if event == "StopFailure" {
			name = "stop_failure"
		}
		fields = map[string]any{
			"hookEventName": name, "sessionId": session, "cwd": cwd,
			"workspaceRoot": cwd, "reason": "end_turn", "error": "api_error",
			"lastAssistantMessage": "private unbound reply @claude @codex",
		}
	}
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

type inertHookFile struct {
	Mode     fs.FileMode
	Modified time.Time
	Data     string
}

// Include directories, mtimes, private files and symlinks, not just state.json:
// a successful no-op must not leave a locator, lock, WAL, credential or repair.
func inertHookSnapshot(t *testing.T, roots ...string) map[string]inertHookFile {
	t.Helper()
	files := map[string]inertHookFile{}
	for i, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if errors.Is(walkErr, os.ErrNotExist) && path == root {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			item := inertHookFile{Mode: info.Mode(), Modified: info.ModTime()}
			if entry.Type()&os.ModeSymlink != 0 {
				item.Data, err = os.Readlink(path)
			} else if entry.Type().IsRegular() {
				var data []byte
				data, err = os.ReadFile(path)
				item.Data = string(data)
			}
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files[fmt.Sprintf("%d/%s", i, rel)] = item
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func isolateInertHook(t *testing.T) {
	t.Helper()
	isolateCaller(t)
	// Snapshot only disposable test configuration on every supported OS.
	home, config := t.TempDir(), t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(key, home)
	}
	for _, key := range []string{"XDG_CONFIG_HOME", "APPDATA"} {
		t.Setenv(key, config)
	}
}

func TestUnboundNativeHooksAreInert(t *testing.T) {
	for _, host := range []struct {
		name, flag string
		kind       model.RuntimeKind
	}{
		{"claude", "claude", model.RuntimeClaude},
		{"codex", "codex", model.RuntimeCodex},
		{"grok", "grok", model.RuntimeGrok},
		{"grok-shared-claude-hook", "claude", model.RuntimeGrok},
	} {
		for _, event := range []string{"Stop", "StopFailure"} {
			for _, tc := range []struct {
				state, service string
				explicit       bool
			}{
				{"none", "missing", false},
				{"other", "stopped", false},
				{"shared-pid", "live", false},
				{"shared-pid-many", "live", true},
				{"malformed", "malformed", false},
				{"permissions", "stopped", true},
				{"symlink", "live", false},
				{"unconfirmed", "stopped", false},
				{"retired", "stopped", false},
				{"removed", "live", false},
				{"replaced", "live", false},
				{"non-git", "stopped", false},
			} {
				t.Run(host.name+"/"+event+"/"+tc.state+"/"+tc.service, func(t *testing.T) {
					isolateInertHook(t)
					root := sessionGitRoot(t)
					if tc.state == "non-git" {
						root = t.TempDir()
					}
					if err := editHooks(root, model.RuntimeKind(host.flag), false); err != nil {
						t.Fatal(err)
					}
					var processLookups int
					harnessAncestor = func() (int, string, bool) {
						processLookups++
						return 123, string(host.kind), true
					}
					if tc.state != "none" && tc.state != "non-git" {
						s := callerState(t, root, "room", "other-session", model.ActorSlot1, host.kind)
						path := filepath.Join(root, ".pairroom", "rooms", s.Room, "slots", string(s.Slot), "state.json")
						switch tc.state {
						case "shared-pid-many":
							callerState(t, root, "other-room", "another-session", model.ActorSlot2, host.kind)
						case "malformed":
							if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
								t.Fatal(err)
							}
						case "permissions":
							if err := os.Chmod(path, 0644); err != nil {
								t.Fatal(err)
							}
						case "symlink":
							if err := os.Rename(path, path+"-target"); err != nil {
								t.Fatal(err)
							}
							if err := os.Symlink(path+"-target", path); err != nil {
								t.Skipf("symlinks unavailable: %v", err)
							}
						case "unconfirmed", "retired", "removed", "replaced":
							s.SessionID = "unbound-session"
							if tc.state == "unconfirmed" {
								s.Generation = 0
							} else if tc.state == "retired" {
								s.Schema = 1
							}
							saveSessionTestState(t, s)
							if tc.state == "removed" || tc.state == "replaced" {
								if err := rememberSession(s); err != nil {
									t.Fatal(err)
								}
								if tc.state == "removed" {
									if err := os.Remove(path); err != nil {
										t.Fatal(err)
									}
								} else {
									s.SessionID = "replacement-session"
									s.Generation++
									saveSessionTestState(t, s)
								}
							}
						}
					}
					var requests atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						http.Error(w, "unbound hooks must never reach the Service", http.StatusServiceUnavailable)
					}))
					t.Cleanup(server.Close)
					endpoint, err := defaultEndpoint()
					if err != nil {
						t.Fatal(err)
					}
					if err := os.MkdirAll(filepath.Dir(endpoint), 0700); err != nil {
						t.Fatal(err)
					}
					if tc.service != "missing" {
						if err := relay.AtomicJSON(endpoint, relay.Endpoint{URL: server.URL, Token: "private-management-token"}); err != nil {
							t.Fatal(err)
						}
					}
					if tc.service == "malformed" {
						if err := os.WriteFile(endpoint, []byte("{"), 0600); err != nil {
							t.Fatal(err)
						}
					} else if tc.service == "stopped" {
						server.Close()
					}
					configRoot, err := os.UserConfigDir()
					if err != nil {
						t.Fatal(err)
					}
					before := inertHookSnapshot(t, root, configRoot)
					args := []string{"hook", "--runtime", host.flag}
					if tc.explicit {
						args = append(args, "--repo", root, "--room", "room", "--slot", "1", "--service-file", endpoint)
					}
					var out, diagnostic bytes.Buffer
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					err = Run(ctx, args, bytes.NewReader(inertHookPayload(t, host.kind, event, "unbound-session", root)), &out, &diagnostic)
					if err != nil || strings.TrimSpace(out.String()) != "{}" || diagnostic.Len() != 0 {
						t.Fatalf("unbound hook was visible: err=%v stdout=%q stderr=%q", err, out.String(), diagnostic.String())
					}
					if requests.Load() != 0 || processLookups != 0 {
						t.Fatalf("unbound hook performed discovery effects: HTTP=%d process lookups=%d", requests.Load(), processLookups)
					}
					if after := inertHookSnapshot(t, root, configRoot); !reflect.DeepEqual(before, after) {
						t.Fatal("unbound hook changed local files or directories")
					}
				})
			}
		}
	}
}

func TestPassiveHookDiscoveryPreservesBoundAndForegroundErrors(t *testing.T) {
	isolateInertHook(t)
	root := sessionGitRoot(t)
	s := callerState(t, root, "room", "bound-session", model.ActorSlot1, model.RuntimeClaude)
	bad := callerState(t, root, "aaa-broken", "other-session", model.ActorSlot2, model.RuntimeClaude)
	badPath := filepath.Join(root, ".pairroom", "rooms", bad.Room, "slots", string(bad.Slot), "state.json")
	if err := os.WriteFile(badPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	caller := nativeCaller{runtime: s.Runtime, session: s.SessionID}
	if _, err := matchingSessions(root, caller); err == nil {
		t.Fatal("foreground discovery silently ignored corrupt state")
	}
	o := options{repo: root}
	if got, err := resolveSessionWorkspace(context.Background(), "hook", &o, caller); err != nil || got != root || o.room != s.Room {
		t.Fatalf("unrelated broken state hid a readable old binding: root=%s options=%+v err=%v", got, o, err)
	}
	// Exact environment evidence still diagnoses identity drift without a locator.
	t.Setenv("CLAUDE_CODE_SESSION_ID", s.SessionID)
	if _, err := resolveSessionWorkspace(context.Background(), "hook", &options{repo: root}, nativeCaller{runtime: s.Runtime, session: "wrong-session"}); !errors.Is(err, errHookSessionMismatch) {
		t.Fatalf("bound environment mismatch became inert: %v", err)
	}
	// A confirmed caller's own corrupt record remains an error, not opt-out.
	if err := rememberSession(s); err != nil {
		t.Fatal(err)
	}
	ownPath := filepath.Join(root, ".pairroom", "rooms", s.Room, "slots", string(s.Slot), "state.json")
	if err := os.WriteFile(ownPath, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSessionWorkspace(context.Background(), "hook", &options{repo: root}, caller); err == nil {
		t.Fatal("confirmed caller's corrupt state was hidden")
	}
}

func TestBoundHookServiceFailuresRemainVisible(t *testing.T) {
	for _, service := range []string{"missing", "malformed", "stopped"} {
		t.Run(service, func(t *testing.T) {
			isolateInertHook(t)
			root := sessionGitRoot(t)
			s := callerState(t, root, "room", "bound-session", model.ActorSlot1, model.RuntimeClaude)
			if err := rememberSession(s); err != nil {
				t.Fatal(err)
			}
			if err := relay.AtomicJSON(filepath.Join(root, ".pairroom", "rooms", s.Room, "slots", string(s.Slot), "credentials"), credentials{BindID: s.BindID, Secret: "private-relay-secret"}); err != nil {
				t.Fatal(err)
			}
			switch service {
			case "malformed":
				if err := os.WriteFile(s.EndpointPath, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "stopped":
				server := httptest.NewServer(http.NotFoundHandler())
				server.Close()
				if err := relay.AtomicJSON(s.EndpointPath, relay.Endpoint{URL: server.URL, Token: "private-management-token"}); err != nil {
					t.Fatal(err)
				}
			}
			var out, diagnostic bytes.Buffer
			err := Run(context.Background(), []string{"hook", "--runtime", "claude"}, bytes.NewReader(inertHookPayload(t, s.Runtime, "Stop", s.SessionID, root)), &out, &diagnostic)
			if err == nil || out.Len() != 0 {
				t.Fatalf("bound Service failure was swallowed: err=%v stdout=%q", err, out.String())
			}
		})
	}
}
