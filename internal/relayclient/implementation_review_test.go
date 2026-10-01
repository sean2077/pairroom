package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/review"
)

// Instruction contracts, not evidence that a model produces correct findings.
func TestImplementationReviewSkillContract(t *testing.T) {
	assertSkillRules(t, map[string]string{
		"opt-in":             "Implementation review (on request)",
		"version":            "Record base/head + dirty hash; recheck before closure",
		"actionable":         "Findings: ID, location, repro/evidence, impact; label hypotheses",
		"dispositions":       "Track open/resolved/not reproducible/disagreed with reasons",
		"resolved-evidence":  "resolved needs fix revision + retest",
		"bounded-conclusion": "End with reviewed revision, tests/not run, risks and human decisions",
		"no-consensus-proof": "Agreement is not validation",
	})
}

// Exercise the installer and freshness detection for every current Native
// Runtime. The recipe travels through the existing single-file projection.
func TestImplementationReviewSkillInstalledForEveryRuntime(t *testing.T) {
	for _, kind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok, model.RuntimeGemini} {
		t.Run(string(kind), func(t *testing.T) {
			IsolateNativeCaller(t)
			for _, env := range []string{"HOME", "USERPROFILE", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME", "GEMINI_CLI_HOME"} {
				t.Setenv(env, t.TempDir())
			}
			path, err := skillHome(kind)
			if err != nil {
				t.Fatal(err)
			}
			writeSkill(t, path, ownedMarker)
			if got := skillStatus(kind); got != "stale" {
				t.Fatalf("old skill status = %q", got)
			}
			if err := installSkill(kind); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(path, "SKILL.md"))
			if err != nil || string(data) != skillContent || !strings.Contains(string(data), "Implementation review (on request)") || skillStatus(kind) != "current" {
				t.Fatalf("recipe projection was not installed/current: %v", err)
			}
		})
	}
}

// Real CLI parsing, Git commits/diffs, local files and HTTP transport with
// scripted Service replies. This verifies recipe wiring, not model review quality.
func TestImplementationReviewRecipePinsAndRechecksTaskRevision(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	ctx := context.Background()
	task, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", task}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(task, "timeout.go"), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(message string) string {
		t.Helper()
		git("add", "timeout.go")
		git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", message)
		return git("rev-parse", "HEAD")
	}
	git("init", "-q")
	write("package fixture\nfunc expired(elapsed, limit int) bool { return elapsed > limit }\n")
	base := commit("base fixture")
	write("package fixture\nfunc expired(elapsed, limit int) bool { return elapsed >= limit }\n")
	head := commit("implementation fixture")
	request := "Review timeout.go:2 at the supplied base/head. Criterion: a zero limit disables expiry. Do not edit."
	requestPath := messageTestFile(t, "request.md", request)
	var out bytes.Buffer
	if err := f.run(ctx, "send", unreadMessageInput{t}, &out, io.Discard, "--id", "impl-review-v1", "--text-file", requestPath, "--review", "--review-repo", task, "--review-base", base); err != nil {
		t.Fatal(err)
	}
	var receipt publicationReceipt
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil || receipt.Published == "" || receipt.Published == "impl-review-v1" {
		t.Fatalf("missing distinct published ID: %v: %s", err, out.String())
	}
	check := func(id, checkout, want string) review.Anchor {
		t.Helper()
		out.Reset()
		args := []string{"--id", id}
		if checkout != "" {
			args = append(args, "--review-repo", checkout)
		}
		if err := f.run(ctx, "review", nil, &out, io.Discard, args...); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Status string        `json:"status"`
			Review review.Anchor `json:"review"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Status != want {
			t.Fatalf("review status: want %s: %v: %s", want, err, out.String())
		}
		return result.Review
	}
	anchor := check(receipt.Published, task, "unchanged_observation")
	if anchor.Workspace != task || anchor.Base != base || anchor.Head != head || anchor.Validate() != nil {
		t.Fatalf("review did not pin the task checkout/range: %+v", anchor)
	}
	// The bound checkout has no commit. Omitting the task path cannot silently
	// certify the different checkout just because the incoming message names it.
	check(receipt.Published, "", "unverified")
	write("package fixture\nfunc expired(elapsed, limit int) bool { return limit > 0 && elapsed >= limit }\n")
	check(receipt.Published, task, "stale")
	fixedHead := commit("fix zero-limit fixture")
	check(receipt.Published, task, "stale")

	// Dispositions remain ordinary message text, not a second workflow store.
	// These scripted findings/results are test data, not actual model findings.
	summary := "F1 resolved at " + fixedHead + ": timeout.go:2 guards limit > 0; zero-limit fixture retest passed.\nF2 not reproducible: original environment unavailable.\nF3 disagreed: inclusive boundary needs human decision.\nNot run: authenticated model E2E. Remaining risk: boundary requirement."
	summaryPath := messageTestFile(t, "summary.md", summary)
	out.Reset()
	if err := f.run(ctx, "send", unreadMessageInput{t}, &out, io.Discard, "--id", "impl-review-v2", "--to", "@user", "--text-file", summaryPath, "--review", "--review-repo", task, "--review-base", base); err != nil {
		t.Fatal(err)
	}
	var final publicationReceipt
	if err := json.Unmarshal(out.Bytes(), &final); err != nil {
		t.Fatal(err)
	}
	fixed := check(final.Published, task, "unchanged_observation")
	if fixed.Head != fixedHead || fixed.Base != base || fixed.Head == anchor.Head {
		t.Fatal("new evidence lost the original review base or fix revision")
	}
	check(receipt.Published, task, "stale")
	f.mu.Lock()
	first, last := f.messages["impl-review-v1"], f.messages["impl-review-v2"]
	f.mu.Unlock()
	if first.Text != request || first.Review.Head != head || last.Text != summary || last.To != model.ActorUser {
		t.Fatal("publication rewrote the old request or lost the human-directed disposition summary")
	}
	if f.count("send") != 2 || f.count("wait") != 0 || f.count("ack") != 0 {
		t.Fatal("read-only review requeued or collected work")
	}
}
