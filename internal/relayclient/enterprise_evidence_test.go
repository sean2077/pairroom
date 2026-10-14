package relayclient

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sean2077/pairroom/internal/relay"
)

func TestEnterpriseCLIQuoteReviewRecoversOriginalObservationAndReadsHistory(t *testing.T) {
	f := newEnterpriseCLIFixture(t)
	commit := func() {
		t.Helper()
		for _, args := range [][]string{{"-C", f.root, "add", "evidence.txt"}, {"-C", f.root, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture evidence"}} {
			if output, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git fixture: %v %s", err, output)
			}
		}
	}
	path := filepath.Join(f.root, "evidence.txt")
	if err := os.WriteFile(path, []byte("original reviewed version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit()
	var output bytes.Buffer
	if err := f.run([]string{"send", "--id", "source", "--text", "original quote body"}, nil, &output); err != nil {
		t.Fatal(err)
	}
	var source publicationReceipt
	if json.Unmarshal(output.Bytes(), &source) != nil {
		t.Fatal("invalid source receipt")
	}
	args := []string{"send", "--id", "reviewed", "--text", "reviewed response", "--quote", source.Published, "--review"}
	f.mu.Lock()
	f.drop = relay.EnterpriseOperationSend
	f.mu.Unlock()
	output.Reset()
	if err := f.run(args, nil, &output); err == nil {
		t.Fatal("lost response reported acceptance")
	}
	f.mu.Lock()
	original := f.operations["send/reviewed"]
	f.mu.Unlock()
	if original.Message == nil || original.Message.Review == nil {
		t.Fatal("review was not captured before the effect")
	}
	if err := os.WriteFile(path, []byte("later workspace version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commit()
	output.Reset()
	if err := f.run(args, nil, &output); err != nil {
		t.Fatal(err)
	}
	var recovered publicationReceipt
	if json.Unmarshal(output.Bytes(), &recovered) != nil || recovered.Published != original.Message.ID || f.count("send") != 2 || f.count("send-resolve") != 1 {
		t.Fatal("same CLI command recaptured mutable review or repeated publication")
	}
	output.Reset()
	if err := f.run([]string{"history", "--id", recovered.Published}, nil, &output); err != nil {
		t.Fatal(err)
	}
	var page relay.HistoryPage
	if json.Unmarshal(output.Bytes(), &page) != nil || len(page.Messages) != 1 || !reflect.DeepEqual(page.Messages[0].Review, original.Message.Review) || page.Messages[0].Quote == nil || page.Messages[0].Quote.Text != "original quote body" || page.Messages[0].Receipt != "" {
		t.Fatal("CLI history lost review evidence")
	}
	output.Reset()
	if err := f.run([]string{"review", "--id", recovered.Published}, nil, &output); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("Evidence is not approval")) || f.count("send") != 2 || f.count("claim") != 0 || f.count("ack") != 0 || f.count("history") != 2 {
		t.Fatal("review/history changed delivery state or claimed approval")
	}
}
