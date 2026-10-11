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
		"poll-only-no-idle-collector":      "Poll-only: no background wait or poll just to stay reachable",
		"poll-only-end-turn-path":          "bounded Stop park may receive queued input, Service wake needs a live local observer",
		"invisible-stdout-suppresses-wake": "unseen stdout can consume input and suppress Service wake",
		"active-task-foreground":           "An imminent reply during active work warrants one foreground wait/exchange inside the tool limit",
		"end-after-empty":                  "end the turn after an empty timeout",
		"cli-not-harness-budget":           "one-hour default is a transport budget, not a harness lifetime guarantee",
		"bounded-park":                     "bounded Stop park may receive queued input",
		"wake-rate-fail-closed":            "wake is rate-limited/fail-closed",
		"unattempted-recheck-only":         "with only unattempted cooldown work rechecked",
		"no-reserved-retry":                "reserved effects never auto-retried",
		"no-invisible-receiver":            "Without a collector, hook or observer, or after a failed wake, input stays queued for collection or a human nudge",
		"failed-wake-queued":               "Without a collector, hook or observer, or after a failed wake, input stays queued for collection or a human nudge",
		"no-agent-vendor-wake":             "Never execute printed vendor wake commands",
	})
	// Restoring the file-workflow review guidance that the rewrite dropped
	// (findings need location/evidence/impact; pin base/head SHAs and the dirty
	// hash; retest fixes at the new revision and report unrun tests) costs about
	// 220 bytes, so the compact operating rules were held to 5250 rather than
	// deleting another safety clause.
	//
	// Joining a Room the human names adds the rules a session needs for a Room ID
	// the human supplied: run a given command unchanged with its selectors, fall
	// back to pairroom relay bind --room, let the CLI pick the slot, never guess,
	// create or replace implicitly, a Room ID is not an invitation, and an
	// explicit join intent outranks generic preflight create guidance. Naming
	// every relay subcommand with its `pairroom relay` prefix, and the
	// description's join trigger, are part of the same work.
	//
	// Every non-clause sentence in the file was compressed and the duplicated
	// rules were merged (evidence ID reuse; queued input without a collector or
	// after a failed wake; bounded Stop park). That returned 234 of the 427 bytes
	// those additions cost. The remaining 165 are boundary sentences: this file
	// is now mostly asserted clause text, so closing the gap needs a boundary
	// merged or dropped (section labels, the Gemini no-Service-wake reminder that
	// Grok already carries, or the numeric-slot label the --slot rules imply)
	// rather than more wording. Do not raise this cap further without that
	// decision.
	if len(skillContent) > 5420 {
		t.Fatalf("skill grew to %d bytes; compact operating rules should fit in 5420", len(skillContent))
	}
}
