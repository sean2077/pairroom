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
		"no-routine-status":                "status/reconcile only on uncertainty/failure",
		"full-output-before-action":        "Read full-output file when shown only a preview",
		"stdout-not-model":                 "stdout/handed_off is not model acceptance",
		"same-publication-id":              "Reuse IDs only for identical publication content",
		"confirmed-timeout-receive-only":   "Confirmed send plus timeout: receive-only wait, never resend",
		"no-duplicate-final":               "omit a final peer handle unless intentionally publishing twice",
		"one-collector":                    "one collector only",
		"tracked-completion-condition":     "If tracked completion wakes harness without idle model turns",
		"bound-session-long-wait":          "keep one bound wait (--timeout 0 until cancelled)",
		"resolve-before-collect":           "resolve before another collector",
		"rehang-until-unbind-or-human":     "rehang on expiry until unbound/human stop",
		"quiet-not-stop":                   "quiet is not stop",
		"poll-only-no-idle-collector":      "Poll-only: no idle background wait/poll",
		"poll-only-end-turn-path":          "Bounded Stop park may receive queued input; wake needs a live local observer",
		"invisible-stdout-suppresses-wake": "unseen stdout may take input and suppress Service wake",
		"active-task-foreground":           "Imminent reply during work: one foreground wait/exchange within tool limit",
		"end-after-empty":                  "empty timeout: end turn",
		"cli-not-harness-budget":           "default hour: transport, not harness lifetime",
		"bounded-park":                     "Bounded Stop park may receive queued input",
		"wake-rate-fail-closed":            "wake: rate-limited/fail-closed",
		"unattempted-recheck-only":         "recheck only unattempted cooldown work",
		"no-reserved-retry":                "no auto-retry reserved effects",
		"no-invisible-receiver":            "No collector/hook/observer or failed wake: queued for collection/human nudge",
		"failed-wake-queued":               "No collector/hook/observer or failed wake: queued for collection/human nudge",
		"no-agent-vendor-wake":             "Never execute printed vendor wake commands",
	})
	// Restoring the file-workflow review guidance that the rewrite dropped
	// (findings need location/evidence/impact; pin base/head SHAs and the dirty
	// hash; retest fixes at the new revision and report unrun tests) costs about
	// 220 bytes, so the compact operating rules are held to 5250 rather than
	// deleting another safety clause.
	//
	// Joining a Room the human names adds the rules a session needs for a Room ID
	// the human supplied: run a given command unchanged with its selectors, fall
	// back to pairroom relay bind --room, let the CLI pick the slot, never guess,
	// create or replace implicitly, a Room ID is not an invitation, and an
	// explicit join intent outranks generic preflight create guidance. Naming
	// every relay subcommand with its `pairroom relay` prefix, and the
	// description's join trigger, belong to the same work.
	//
	// Every non-clause sentence was compressed, duplicated rules stay merged
	// (evidence ID reuse; queued input without a collector or after a failed
	// wake; bounded Stop park), the section labels and the redundant title line
	// are gone, and the Gemini line leaves the no-Service-wake boundary to
	// Grok's line, which the reviewer chose. With those, the file fits 5250 with
	// no boundary dropped; keep it there.
	if len(skillContent) > 5250 {
		t.Fatalf("skill grew to %d bytes; compact operating rules should fit in 5250", len(skillContent))
	}
}
