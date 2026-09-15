package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestGrokInstallOwnsOnlyItsProjectFileAndUserSkill(t *testing.T) {
	isolateCaller(t)
	t.Setenv("GROK_HOME", "")
	root, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path, err := hookPath(root, model.RuntimeGrok)
	if err != nil || path != filepath.Join(root, ".grok", "hooks", "pairroom.json") {
		t.Fatalf("hook location: %s %v", path, err)
	}
	dir, err := secureDir(root, ".grok", "hooks")
	if err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "other.json")
	if err := os.WriteFile(unrelated, []byte(`{"untouched":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := editHooks(root, model.RuntimeGrok, false); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := editHooks(root, model.RuntimeGrok, false); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if !bytes.Equal(first, second) || installed(root, model.RuntimeGrok) != nil {
		t.Fatal("Grok hook install is not idempotent/discoverable")
	}
	var value map[string]any
	if json.Unmarshal(second, &value) != nil || value["hooks"] == nil {
		t.Fatal("invalid Grok hook config")
	}
	if err := installSkill(model.RuntimeGrok); err != nil {
		t.Fatal(err)
	}
	skill, err := os.ReadFile(filepath.Join(home, ".grok", "skills", "pairroom-relay", "SKILL.md"))
	if err != nil || string(skill) != skillContent {
		t.Fatal("Grok skill missing")
	}
	for _, p := range []string{filepath.Join(root, ".claude"), filepath.Join(root, ".codex"), filepath.Join(home, ".grok", "trusted_folders.toml")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("install changed native trust/another runtime: %s", p)
		}
	}
	if err := editHooks(root, model.RuntimeGrok, true); err != nil {
		t.Fatal(err)
	}
	if installed(root, model.RuntimeGrok) == nil {
		t.Fatal("removed hook still installed")
	}
	other, _ := os.ReadFile(unrelated)
	if string(other) != `{"untouched":true}` {
		t.Fatal("removed another hook")
	}
}

func TestGrokCreatorInfersOwnRuntimeWithPeerSelection(t *testing.T) {
	isolateCaller(t)
	t.Setenv("GROK_SESSION_ID", "own")
	o := options{peer: "codex"}
	slot, err := inferCreateSlot(o)
	if err != nil || slot != model.ActorClaude {
		t.Fatalf("slot=%s err=%v", slot, err)
	}
	selections, err := createAgents(o, slot)
	if err != nil || selections[slot].Runtime != model.RuntimeGrok || selections[peerSlot(slot)].Runtime != model.RuntimeCodex {
		t.Fatalf("explicit peer lost calling Grok: %+v %v", selections, err)
	}
	selections, err = createAgents(options{}, "")
	if err != nil || selections != nil {
		t.Fatal("zero overrides silently changed Service defaults")
	}
	root := t.TempDir()
	callerState(t, root, "grok-room", "own", model.ActorCodex, model.RuntimeGrok)
	resume := options{}
	if err := applyCallerDefaults(root, "bind", &resume); err != nil || resume.room != "grok-room" || resume.slot != "codex" || !resume.cont {
		t.Fatalf("Grok resume failed: %+v %v", resume, err)
	}
}

func TestGrokHookRejectsLegacyClaimWithoutLeakingOrAcknowledging(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	c, err := load(filepath.Dir(f.statePath))
	if err != nil {
		t.Fatal(err)
	}
	c.State.Runtime = model.RuntimeGrok
	var out bytes.Buffer
	_, err = deliverOnce(context.Background(), c, true, 1, &out)
	if err == nil || out.Len() != 0 || f.count("ack") != 0 {
		t.Fatalf("Grok hook accepted a full claim: %q %v", out.String(), err)
	}
}

func TestGrokContinuationSharesCapWithoutClaiming(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	c, err := load(filepath.Dir(f.statePath))
	if err != nil {
		t.Fatal(err)
	}
	c.State.Runtime = model.RuntimeGrok
	c.State.Blocks = 0
	if err := c.persist(c.State); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		var out bytes.Buffer
		if err := grokContinuation(context.Background(), c, strings.Repeat("x", 10001), &out); err != nil {
			t.Fatal(err)
		}
		if i < 8 && (!strings.Contains(out.String(), `"block"`) || out.Len() > 1000) {
			t.Fatal("unsafe/oversized continuation")
		}
		if i == 8 && strings.TrimSpace(out.String()) != "{}" {
			t.Fatal("continuation cap bypassed")
		}
	}
	if f.count("wait") != 0 || f.count("ack") != 0 || f.count("send") != 0 {
		t.Fatal("readiness hint consumed/published input")
	}
}

func TestGrokSkillHonorsConfiguredHome(t *testing.T) {
	home, grokHome := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GROK_HOME", grokHome)
	if err := installSkill(model.RuntimeGrok); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(grokHome, "skills", "pairroom-relay", "SKILL.md"))
	if err != nil || string(body) != skillContent {
		t.Fatalf("custom Grok skill missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".grok")); !os.IsNotExist(err) {
		t.Fatal("ignored GROK_HOME")
	}
}
