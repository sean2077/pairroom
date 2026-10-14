package sqlstore

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestHumanEvidenceHashPreservesLegacyV1AndV2Vectors(t *testing.T) {
	cmd := humanSendCommand("u-owner", "hs-1", "slot2", "hello")
	key, class := humanSendKey(cmd)
	if class != "" {
		t.Fatal(class)
	}
	check := func(intent HumanSendIntent, want string) {
		t.Helper()
		sum := humanSendHash(key, intent)
		if hex.EncodeToString(sum[:]) != want {
			t.Fatal("legacy frozen hash bytes changed")
		}
	}
	// Independently calculated length-prefixed SHA-256 contract vectors. New
	// empty quote/review fields must not enter either historical codec.
	check(cmd.Intent, "1ccf0fe43487edd5b624aa533f826f29667afdcbc3f0fd10e6759fb9d4c578fd")
	cmd.Intent.AttachmentIDs, _ = model.NewEnterpriseAttachmentIDs([]string{"att-111111111111111111111111"})
	check(cmd.Intent, "7b639f3f864e4db9450c6dcde74c838f6f8a8b9d0a9107c2a5ee378cf1b89e8e")
	base := cmd.Intent
	base.QuoteID = "source"
	base.Review = evidenceAnchor()
	hash := humanSendHash(key, base)
	for _, change := range []func(*HumanSendIntent){func(i *HumanSendIntent) { i.QuoteID = "other" }, func(i *HumanSendIntent) { i.Review.Workspace = "other endpoint" }, func(i *HumanSendIntent) { i.Review.DirtySHA256 = strings.Repeat("d", 64) }} {
		next := base
		change(&next)
		if humanSendHash(key, next) == hash {
			t.Fatal("quote/review not covered by frozen hash")
		}
	}
}

func TestHumanEvidenceAuthorMustBeOriginalAuthenticatedPrincipal(t *testing.T) {
	cmd := humanSendCommand("u-owner", "human-author", "slot1", "body")
	for _, author := range []string{"", relay.EnterpriseHumanAuthor(cmd.Auth.UserID)} {
		cmd.Fact = humanSendCandidate(cmd.Auth.UserID, cmd.ClientID, cmd.Intent.To, cmd.Intent.Text, map[string]any{"author": author})
		if validateHumanSendFact(cmd) != "" {
			t.Fatal("exact new author or legacy empty author refused")
		}
	}
	for _, author := range []string{relay.EnterpriseHumanAuthor("different-user"), "Room Owner", "lan:owner"} {
		cmd.Fact = humanSendCandidate(cmd.Auth.UserID, cmd.ClientID, cmd.Intent.To, cmd.Intent.Text, map[string]any{"author": author})
		if validateHumanSendFact(cmd) != ClassInvalidFact {
			t.Fatal("caller narration forged authenticated public author")
		}
	}
}
