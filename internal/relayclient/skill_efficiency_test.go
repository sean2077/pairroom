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
		"one-collector":                    "keep exactly one collector",
		"tracked-completion-condition":     "If tracked background completion wakes this harness without idle model turns",
		"bound-session-long-wait":          "keep one long relay wait while bound (--timeout 0 until cancelled)",
		"resolve-before-collect":           "resolve its result before another collector",
		"rehang-until-unbind-or-human":     "rehang after expiry unless unbound or the human stops it",
		"quiet-not-stop":                   "A quiet Room alone is not a stop request",
		"poll-only-no-idle-collector":      "In a poll-only harness, do not keep a background wait or poll merely to stay reachable",
		"invisible-stdout-suppresses-wake": "unseen stdout can consume input and suppress Service wake",
		"active-task-foreground":           "During active work, an imminent reply warrants one foreground wait/exchange within the harness tool limit",
		"end-after-empty":                  "after an empty timeout, end the turn",
		"cli-not-harness-budget":           "one-hour default is a transport budget, not a harness lifetime guarantee",
		"bounded-park":                     "Stop park is bounded",
		"wake-rate-fail-closed":            "Wake is rate-limited/fail-closed",
		"unattempted-recheck-only":         "only unattempted cooldown work is rechecked",
		"no-reserved-retry":                "reserved effects are never auto-retried",
		"failed-wake-queued":               "Failed wake leaves input queued for collection or a human nudge",
		"no-agent-vendor-wake":             "Never execute printed vendor wake commands",
	})
	if len(skillContent) > 5000 {
		t.Fatalf("skill grew to %d bytes; compact operating rules should fit in 5000", len(skillContent))
	}
}
