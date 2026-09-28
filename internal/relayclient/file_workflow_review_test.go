package relayclient

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileWorkflowRetainsExplicitStdinWithReferences(t *testing.T) {
	path := messageTestFile(t, "evidence.txt", "external evidence")
	text, err := readPublicationInput(publicationInput{textFile: "-", references: []string{path}}, strings.NewReader("piped decision"), 256<<10)
	if err != nil || !strings.HasPrefix(text, "piped decision\n\n") || len(parseMessageReferences(t, text)) != 1 {
		t.Fatalf("explicit stdin was lost beside references: %v", err)
	}
}

func TestFileDeliveryWithholdsAckWhenDestinationAppears(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	c, err := load(filepath.Dir(f.statePath))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "incoming.txt")
	writer, err := newEnvelopeFileWriter(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// A file arriving after preflight must not be clobbered or acknowledged.
	if err := os.WriteFile(path, []byte("keep existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	delivered, err := deliverForeground(context.Background(), c, 1, writer)
	if err == nil || delivered || f.count("ack") != 0 || f.count("wait") != 1 {
		t.Fatalf("storage failure was acknowledged or retried: delivered=%v, err=%v", delivered, err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep existing" {
		t.Fatal("storage failure altered the destination")
	}
}

func TestNativeFileWorkflowSkillGuidance(t *testing.T) {
	assertSkillRules(t, map[string]string{
		"existing-text-file":       "--text-file <path>",
		"stdin":                    "--text-file -",
		"local-reference":          "--ref <path>",
		"new-output-file":          "--output-file <new-path>",
		"no-read-retype":           "do not read and retype it",
		"inline-small":             "Prefer inline small replies and send deltas",
		"not-uploaded":             "shares path/size/SHA-256, not uploaded content",
		"retain-and-verify":        "retain the file, verify its hash",
		"authorized-file-access":   "read needed sections with authorized access",
		"changed-evidence-new-id":  "changed evidence needs a new ID",
		"read-full-envelope":       "saves the full envelope; read it before acting",
		"existing-writable-parent": "Parent must exist and be writable",
		"no-overwrite":             "no overwrite, retry with a fresh path",
		"tool-cwd":                 "Paths use the tool cwd",
		"current-cli":              "these flags need the current CLI",
		"review-anchor":            "use --review and verify with review --id <published-id>",
		"review-checkout":          "pass --review-repo <task-checkout> on each for a different checkout",
		"evidence-not-approval":    "An observation is neither an atomic snapshot nor approval",
		"independent-review":       "Inspect independently; report unresolved disagreements",
	})
}
