package relayclient

import (
	"strings"
	"testing"
)

// Prose assertions preserve specific safety decisions; they are not evidence
// that a vendor model will comply. Transport behavior has separate tests.
func assertSkillRules(t *testing.T, rules map[string]string) {
	t.Helper()
	for name, clause := range rules {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(skillContent, clause) {
				t.Errorf("skill lost %s: %q", name, clause)
			}
		})
	}
}
func TestNativeSkillCostAndCompleteOutput(t *testing.T) {
	assertSkillRules(t, map[string]string{
		"no-routine-status":                "only for uncertainty/failure, not after each success",
		"full-output-before-action":        "Read a harness's full-output file if only a preview is shown",
		"stdout-not-model":                 "stdout/handed_off is not model acceptance",
		"same-publication-id":              "Reuse an ID only for identical publication content",
		"confirmed-timeout-receive-only":   "Confirmed send plus timeout: receive-only wait, never resend",
		"no-duplicate-final":               "omit a final peer handle unless intentionally publishing twice",
		"one-collector":                    "one collector only",
		"tracked-completion-condition":     "If tracked background completion wakes this harness without idle model turns",
		"bound-session-long-wait":          "keep one long relay wait while bound (--timeout 0 until cancelled)",
		"resolve-before-collect":           "resolve its result before another collector",
		"rehang-until-unbind-or-human":     "rehang after expiry unless unbound or the human stops it",
		"quiet-not-stop":                   "a quiet Room is not a stop request",
		"poll-only-no-idle-collector":      "In a poll-only harness, do not keep a background wait or poll just to stay reachable",
		"poll-only-end-turn-path":          "Bounded Stop park may deliver queued input; Service wake needs a live local observer",
		"invisible-stdout-suppresses-wake": "unseen stdout can consume input and suppress Service wake",
		"active-task-foreground":           "During active work, an imminent reply warrants one foreground wait/exchange inside the tool limit",
		"end-after-empty":                  "after an empty timeout, end the turn",
		"cli-not-harness-budget":           "one-hour default is a transport budget, not a harness lifetime guarantee",
		"bounded-park":                     "Bounded Stop park may deliver queued input",
		"wake-rate-fail-closed":            "Wake is rate-limited/fail-closed",
		"unattempted-recheck-only":         "only unattempted cooldown work is rechecked",
		"no-reserved-retry":                "reserved effects are never auto-retried",
		"no-invisible-receiver":            "Without a collector, hook or observer, input stays at the host",
		"failed-wake-queued":               "Failed wake leaves input queued for collection or a human nudge",
		"no-agent-vendor-wake":             "never execute printed vendor wake commands",
	})
	// Restoring the file-workflow review guidance that the rewrite dropped
	// (findings need location/evidence/impact; pin base/head SHAs and the dirty
	// hash; retest fixes at the new revision and report unrun tests) costs about
	// 220 bytes, so the compact operating rules were held to 5250 rather than
	// deleting another safety clause. Joining a Room the human names (a complete
	// command run unchanged with its selectors, exact Room ID, slot inference, no
	// guess or implicit create/replace, a Room ID is not an invitation, and an
	// explicit join intent outranking generic preflight create guidance) adds
	// about 360 bytes. Compressing every non-clause sentence across the file
	// returned about 160 of them; the cap carries the remaining 171 so no
	// reachability, LAN, unknown or review clause had to go. Reaching 5250 again
	// needs a rule merged or dropped, not more wording.
	if len(skillContent) > 5430 {
		t.Fatalf("skill grew to %d bytes; compact operating rules should fit in 5430", len(skillContent))
	}
}
