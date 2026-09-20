package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCLITextFilePublishesOnceWithoutEcho(t *testing.T) {
	body := strings.Repeat("long-review-do-not-echo\n", 8192)
	path := messageTestFile(t, "review.md", body)
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	var out, diagnostic bytes.Buffer
	if err := f.run(context.Background(), "send", unreadMessageInput{t}, &out, &diagnostic, "--id", "review-file", "--text-file", path); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	msg := f.messages["review-file"]
	f.mu.Unlock()
	if msg.Text != body || f.count("send") != 1 || f.count("wait") != 0 {
		t.Fatal("file body was not published exactly once")
	}
	if out.Len()+diagnostic.Len() > 2048 || strings.Contains(out.String()+diagnostic.String(), "long-review-do-not-echo") {
		t.Fatal("publication output echoed the file body")
	}
}

func TestCLIReferenceOnlyAndChangedReferenceRetry(t *testing.T) {
	path := messageTestFile(t, "evidence.md", strings.Repeat("evidence-not-inline", 32768))
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	if err := f.run(context.Background(), "send", unreadMessageInput{t}, io.Discard, io.Discard, "--id", "reference", "--ref", path); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	msg := f.messages["reference"]
	f.mu.Unlock()
	refs := parseMessageReferences(t, msg.Text)
	if len(refs) != 1 || strings.Contains(msg.Text, "evidence-not-inline") {
		t.Fatal("reference was expanded into the publication")
	}
	if err := os.WriteFile(path, []byte("changed evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := f.run(context.Background(), "send", unreadMessageInput{t}, io.Discard, io.Discard, "--id", "reference", "--ref", path)
	if err == nil || !strings.Contains(err.Error(), "different body") {
		t.Fatalf("same-ID changed reference was not detected: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.messages) != 1 || f.messages["reference"].Text != msg.Text {
		t.Fatal("retry replaced the original publication")
	}
}

func TestCLIFileFlagsFailBeforeWorkspaceOrPublication(t *testing.T) {
	for _, args := range [][]string{
		{"wait", "--text-file", "missing"},
		{"status", "--ref", "missing"},
		{"send", "--output-file", "incoming.txt"},
		{"send", "--text", "", "--text-file", "-"},
		{"send", "--text-file", ""},
		{"wait", "--output-file", ""},
	} {
		err := Run(context.Background(), args, unreadMessageInput{t}, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "--") || strings.Contains(err.Error(), "workspace") {
			t.Fatalf("file-option validation did not run first: %v: %v", args, err)
		}
	}
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	for _, path := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing"), messageTestFile(t, "bad.bin", "\xff")} {
		if err := f.run(context.Background(), "send", unreadMessageInput{t}, io.Discard, io.Discard, "--text-file", path); err == nil {
			t.Fatal("accepted invalid file body")
		}
	}
	existing := messageTestFile(t, "existing.txt", "keep")
	if err := f.run(context.Background(), "exchange", unreadMessageInput{t}, io.Discard, io.Discard, "--id", "not-sent", "--text", "proposal", "--output-file", existing); err == nil {
		t.Fatal("accepted an existing receive destination")
	}
	if f.count("send") != 0 || f.count("wait") != 0 || f.count("ack") != 0 {
		t.Fatal("file validation performed a publication or claim")
	}
}

// Verify from the stdout sink that persistence preceded the receipt; the
// fixture independently verifies that acknowledgement follows that receipt.
type persistedReceiptObserver struct {
	t       *testing.T
	want    string
	written *atomic.Bool
	output  bytes.Buffer
}

func (w *persistedReceiptObserver) Write(data []byte) (int, error) {
	var receipt envelopeFileReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		w.t.Errorf("invalid file receipt: %v", err)
		return 0, err
	}
	body, err := os.ReadFile(receipt.EnvelopeFile)
	if err != nil || string(body) != w.want {
		w.t.Error("receipt preceded the complete persisted envelope")
		return 0, errors.New("envelope not persisted")
	}
	w.written.Store(true)
	return w.output.Write(data)
}

func TestCLIFileDeliveryAcknowledgesOnlyAfterPersistenceAndReceipt(t *testing.T) {
	envelope := strings.Repeat("peer-evidence-not-in-stdout\n", 8192)
	var written atomic.Bool
	f := newForegroundFixture(t, foregroundFixtureOptions{envelope: envelope, ackAfterWrite: &written})
	observer := &persistedReceiptObserver{t: t, want: envelope + "\n", written: &written}
	path := filepath.Join(t.TempDir(), "incoming.txt")
	if err := f.run(context.Background(), "wait", unreadMessageInput{t}, observer, io.Discard, "--timeout", "1", "--output-file", path); err != nil {
		t.Fatal(err)
	}
	if f.count("ack") != 1 || observer.output.Len() > 1024 || strings.Contains(observer.output.String(), "peer-evidence-not-in-stdout") {
		t.Fatal("file delivery lost its one-ack, body-free receipt boundary")
	}
}

func TestCLIFileDeliveryWithholdsAckOnReceiptFailure(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{envelope: "complete incoming"})
	path := filepath.Join(t.TempDir(), "incoming.txt")
	err := f.run(context.Background(), "wait", unreadMessageInput{t}, shortFileReceiptWriter{}, io.Discard, "--timeout", "1", "--output-file", path)
	if !errors.Is(err, io.ErrShortWrite) || f.count("ack") != 0 || f.count("wait") != 1 {
		t.Fatalf("failed receipt was acknowledged or retried: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "complete incoming\n" {
		t.Fatal("lost the local recovery copy")
	}
}

func TestCLIFileExchangeAndTimeoutRecovery(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "delivery", true: "timeout"}[empty], func(t *testing.T) {
			f := newForegroundFixture(t, foregroundFixtureOptions{emptyInbox: empty, envelope: "peer response"})
			input := messageTestFile(t, "outgoing.md", "proposal")
			destination, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(destination, "incoming with spaces.txt")
			var out, diagnostic bytes.Buffer
			err = f.run(context.Background(), "exchange", unreadMessageInput{t}, &out, &diagnostic, "--id", "file-exchange", "--text-file", input, "--output-file", output, "--timeout", "1")
			if f.count("send") != 1 || f.count("wait") != 1 || strings.Contains(diagnostic.String(), "proposal") {
				t.Fatal("exchange duplicated or echoed its publication")
			}
			if empty {
				if !errors.Is(err, errExchangeWaiting) || !strings.Contains(err.Error(), " --output-file "+quoteShellPath(output)) {
					t.Fatalf("timeout lost file-mode recovery: %v", err)
				}
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) || out.Len() != 0 || f.count("ack") != 0 {
					t.Fatal("empty inbox created or acknowledged an output file")
				}
			} else {
				if err != nil || f.count("ack") != 1 {
					t.Fatalf("exchange delivery failed: %v", err)
				}
				if data, err := os.ReadFile(output); err != nil || string(data) != "peer response\n" {
					t.Fatal("exchange lost incoming file content")
				}
			}
		})
	}
}
