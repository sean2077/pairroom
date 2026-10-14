package relayclient

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestEnterpriseCLIAbandonPendingIsOfflineAndRetainsOriginalAttempt(t *testing.T) {
	f := newEnterpriseCLIFixture(t)
	session := "pending-native-session"
	t.Setenv("CLAUDE_CODE_SESSION_ID", session)
	selectors := []string{"--repo", f.root, "--enterprise-service", f.server.URL, "--deployment", "dep-test", "--enterprise-ca", f.ca, "--enterprise-state-dir", f.deviceDir, "--room", "server-room", "--slot", "2", "--id", "original-attempt"}
	var out bytes.Buffer
	if err := f.run(append([]string{"bind"}, selectors...), nil, &out); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, ".pairroom", "enterprise-joins", f.state.Room, relay.Digest(string(model.RuntimeClaude) + "\x00" + session)[:32], "bind-attempt-"+relay.Digest("original-attempt")[:24]+".json")
	var original enterpriseBindAttempt
	if err := privatefile.ReadJSON(path, maxPrivateFileBytes, &original); err != nil {
		t.Fatal(err)
	}
	f.server.Close()
	if err := os.Remove(f.ca); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	abandon := append([]string{"unbind", "--local-only", "--abandon-pending"}, selectors...)
	if err := f.run(abandon, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "locally_detached") || strings.Contains(out.String(), original.Credentials.Secret) || strings.Contains(out.String(), session) {
		t.Fatal("offline result was false success or exposed native identity")
	}
	var retired enterpriseBindAttempt
	if err := privatefile.ReadJSON(path, maxPrivateFileBytes, &retired); err != nil || retired.DetachedAt.IsZero() {
		t.Fatalf("retained attempt: %v", err)
	}
	retired.DetachedAt = original.DetachedAt
	if retired != original {
		t.Fatal("abandon rewrote original identity/candidate")
	}
	identities, err := nativeidentity.Open()
	if err != nil {
		t.Fatal(err)
	}
	newClaim := nativeidentity.Claim{Runtime: model.RuntimeClaude, SessionID: session, Association: nativeidentity.Enterprise("https://other.example", "other", "room"), BindID: "later-binding"}
	if err := identities.Reserve(context.Background(), newClaim); err != nil {
		t.Fatal(err)
	}
	if err := f.run(abandon, nil, &out); err != nil {
		t.Fatal(err)
	}
	if err := identities.Check(context.Background(), newClaim); err != nil {
		t.Fatal("old abandon removed another owner")
	}
	if _, err := os.Stat(filepath.Join(f.root, ".pairroom", "rooms", f.state.Room, "slots", "slot2", "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("abandon made a pending admission discoverable")
	}
	var preserved State
	if err := readPrivate(filepath.Join(f.slotDir, "state.json"), &preserved); err != nil || preserved.BindID != f.state.BindID {
		t.Fatal("pending abandon changed the other active slot")
	}
}

func TestEnterpriseCLIAbandonRequiresExplicitOfflineSelectors(t *testing.T) {
	for _, args := range [][]string{{"unbind", "--abandon-pending"}, {"bind", "--abandon-pending", "--local-only"}, {"unbind", "--local-only", "--abandon-pending", "--room", "room"}} {
		if err := Run(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Fatal("incomplete abandon selectors accepted")
		}
	}
}
