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
	for _, guidance := range []string{
		"--text-file <path>", "--text-file -", "--ref <path>", "--output-file <new-path>",
		"not uploaded", "read that envelope before acting", "Prefer inline output for small replies",
		"current CLI", "pairroom relay exchange --help", "retry collection with a fresh path",
	} {
		if !strings.Contains(skillContent, guidance) {
			t.Errorf("distributed skill lost file workflow guidance: %s", guidance)
		}
	}
}
