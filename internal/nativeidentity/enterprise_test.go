package nativeidentity

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestEnterpriseAssociationReservesAcrossLocalLANAndOtherDeployments(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	claim := Claim{Runtime: model.RuntimeClaude, SessionID: "official-session", Association: Enterprise("https://service.example.test", "deployment-one", "room"), BindID: "binding-one"}
	if !claim.Valid() || !strings.HasPrefix(claim.Association, "enterprise:") {
		t.Fatal("enterprise authority disguised as a prior mode")
	}
	if err := store.Reserve(ctx, claim); err != nil {
		t.Fatal(err)
	}
	for _, association := range []string{Hosted(t.TempDir(), "room", model.ActorSlot1), Remote("pin", "room"), Enterprise("https://other.example.test", "deployment-one", "room"), Enterprise("https://service.example.test", "deployment-two", "room")} {
		other := claim
		other.Association = association
		if err := store.Reserve(ctx, other); !errors.Is(err, ErrOwned) {
			t.Fatalf("same native session acquired other authority %q: %v", association, err)
		}
	}
	otherSession := claim
	otherSession.SessionID = "other-official-session"
	otherSession.BindID = "binding-two"
	if err := store.Reserve(ctx, otherSession); err != nil {
		t.Fatal("two sessions in one Room collided")
	}
	confirmed := claim
	confirmed.Generation = 1
	if err := store.Reserve(ctx, confirmed); err != nil {
		t.Fatal(err)
	}
	if err := store.Release(ctx, claim); !errors.Is(err, ErrOwned) {
		t.Fatal("pending claim released accepted generation")
	}
	if err := store.Check(ctx, confirmed); err != nil {
		t.Fatal(err)
	}
}
