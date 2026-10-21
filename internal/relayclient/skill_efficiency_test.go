package relayclient

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/relayclient/skill"
)

// Prose assertions preserve specific safety decisions in the file that owns
// them. They do not show that a vendor model will select or obey the guidance;
// transport and installer behavior have separate tests.
func assertSkillRules(t *testing.T, path string, rules map[string]string) {
	t.Helper()
	var content string
	for _, file := range skill.Files() {
		if file.Path == path {
			content = file.Content
			break
		}
	}
	if content == "" {
		t.Fatalf("missing skill file %s", path)
	}
	for name, clause := range rules {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(content, clause) {
				t.Errorf("%s lost %s: %q", path, name, clause)
			}
		})
	}
}

func TestNativeSkillCostAndCompleteOutput(t *testing.T) {
	assertSkillRules(t, "SKILL.md", map[string]string{
		"no-routine-status":              "status/reconcile only on uncertainty/failure",
		"full-output-before-action":      "Read full-output files when shown only a preview",
		"stdout-not-model":               "stdout/handed_off is not model acceptance",
		"same-publication-id":            "Reuse IDs only for identical publication content",
		"confirmed-timeout-receive-only": "Confirmed send plus timeout: receive-only wait, never resend",
		"no-duplicate-final":             "omit a final peer handle unless intentionally publishing twice",
		"one-collector":                  "one collector only; resolve it before another",
		"active-task-foreground":         "one foreground wait/exchange within the tool limit",
		"end-after-empty":                "an empty timeout ends the turn",
		"poll-only-no-idle-collector":    "No idle background wait/poll unless",
	})
	assertSkillRules(t, "references/waiting.md", map[string]string{
		"tracked-completion-condition":     "If tracked completion wakes the harness without idle model turns",
		"bound-session-long-wait":          "pairroom relay wait --timeout 0",
		"resolve-before-collect":           "resolve it before another collector",
		"rehang-until-unbind-or-human":     "rehang on expiry until unbound or a human stops it",
		"quiet-not-stop":                   "quiet is not stop",
		"poll-only-no-idle-collector":      "Poll-only tools: no idle background wait/poll",
		"poll-only-end-turn-path":          "Bounded Stop park may receive queued input; wake needs a live local observer",
		"invisible-stdout-suppresses-wake": "Unseen stdout may take input and suppress Service wake",
		"cli-not-harness-budget":           "transport limit, not the harness lifetime",
		"wake-rate-fail-closed":            "Wake is rate-limited and fail-closed",
		"unattempted-recheck-only":         "Recheck only unattempted cooldown work",
		"no-reserved-retry":                "never automatically retry reserved effects",
		"no-invisible-receiver":            "No collector/hook/observer or failed wake",
		"no-agent-vendor-wake":             "Never execute printed vendor wake commands",
	})
	// The default body must stay small without deleting optional operating
	// rules. Only this entrypoint is loaded on every skill invocation.
	if len(skillContent) > 3000 {
		t.Fatalf("default skill grew to %d bytes; route optional guidance to references (budget 3000)", len(skillContent))
	}
}

func TestNativeSkillReferenceRoutes(t *testing.T) {
	files := map[string]string{}
	for _, file := range skill.Files() {
		files[file.Path] = file.Content
	}
	if files["SKILL.md"] != skillContent {
		t.Fatal("CLI and shared bundle use different entrypoints")
	}
	linked := map[string]bool{}
	for _, match := range regexp.MustCompile("\\]\\((references/[^)]+)\\)").FindAllStringSubmatch(skillContent, -1) {
		path := match[1]
		if strings.TrimSpace(files[path]) == "" {
			t.Errorf("default skill links to missing or empty reference %s", path)
		}
		linked[path] = true
	}
	for path, content := range files {
		if path == "SKILL.md" {
			continue
		}
		if !linked[path] {
			t.Errorf("bundled reference %s is not reachable directly from SKILL.md", path)
		}
		if !strings.HasPrefix(content, skill.ReferenceMarker+"\n") {
			t.Errorf("reference %s lacks the installer ownership marker", path)
		}
	}
	assertSkillRules(t, "SKILL.md", map[string]string{
		"selective-load":     "Read only the matching reference before its operation",
		"relative-location":  "Paths are relative to this skill directory, not the tool cwd",
		"no-preload":         "do not preload references",
		"enterprise-route":   "or an enterprise Binding",
		"route-before-local": "before local preflight",
		"lan-route":          "LAN invitation, sharing or admission",
		"runtime-route":      "Current Runtime is Grok:",
		"file-route":         "Before file transfer, referenced-file reads or evidence review",
		"wake-route":         "also read it for idle-wake diagnosis",
		"recovery-route":     "stop and read [recovery]",
	})
	enterprise := strings.Index(skillContent, "(references/enterprise.md)")
	local := strings.Index(skillContent, "## Local create or join")
	if enterprise < 0 || local < 0 || enterprise >= local {
		t.Fatal("enterprise routing must precede local setup/preflight")
	}
	for _, detail := range []string{"Device login", "staged files", "ACK after stdout", "GROK_SESSION_ID", "seven Stop continuations", "--peer-generation", "base/head SHAs"} {
		if strings.Contains(skillContent, detail) {
			t.Errorf("optional detail %q leaked into the default body", detail)
		}
	}
}
