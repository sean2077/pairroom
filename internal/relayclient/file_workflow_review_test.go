package relayclient

import (
	"bytes"
	"context"
	"errors"
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
		"direct-join":              "CLI/hooks need no local Service or --service-file",
		"review-anchor":            "--review; review --id <published-id>",
		"review-checkout":          "--review-repo <task-checkout> for another checkout",
		"evidence-not-approval":    "Observations are not atomic or approval",
		"independent-review":       "Inspect independently; report unresolved disagreements",
	})
}

// A real wait with --inline-max prints a small envelope on stdout, creates no
// file and acknowledges it; the flag is rejected without --output-file.
func TestWaitInlinesSmallEnvelopeWithOutputFile(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	path := filepath.Join(t.TempDir(), "incoming.txt")
	var out, diagnostic bytes.Buffer
	if err := f.run(context.Background(), "wait", nil, &out, &diagnostic, "--timeout", "1", "--output-file", path, "--inline-max", "8192"); err != nil {
		t.Fatalf("wait: %v; %s", err, diagnostic.String())
	}
	if !strings.Contains(out.String(), "Review finding") || f.count("ack") != 1 {
		t.Fatalf("stdout=%q acks=%d", out.String(), f.count("ack"))
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an inlined envelope must not create the output file")
	}
	for _, args := range [][]string{{"--inline-max", "10"}, {"--output-file", filepath.Join(t.TempDir(), "x"), "--inline-max", "-1"}} {
		if err := f.run(context.Background(), "wait", nil, io.Discard, io.Discard, append([]string{"--timeout", "1"}, args...)...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
