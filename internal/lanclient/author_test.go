package lanclient

import (
	"context"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestSharedRoomHumanEnvelopeKeepsAttributionAndApprovalBoundary(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, _ := f.join(t, s)
	r, err := c.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key, err := r.Identity.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		author string
		handle string
		remote bool
	}{
		{name: "remote human", author: "host_owner", handle: protocol.RemoteRoomOwnerHandle, remote: true},
		{name: "local human", author: "lan:" + key, handle: protocol.LocalRoomOwnerHandle},
		{name: "absent author"},
		{name: "unknown author", author: "teammate"},
		{name: "other certificate", author: "lan:" + relay.Digest("unrecognized member")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope, err := c.prepareEnvelope(ctx, r, relay.Message{From: model.ActorUser, To: auth.Slot, Author: tc.author, Text: "Please inspect this repro."})
			if tc.handle == "" {
				if err == nil || envelope != "" {
					t.Fatalf("unattributed human was rendered: %q %v", envelope, err)
				}
				return
			}
			if err != nil || !strings.Contains(envelope, "from: "+tc.handle+"\n") {
				t.Fatalf("human attribution changed: %q %v", envelope, err)
			}
			wantNotices := 0
			if tc.remote {
				wantNotices = 1
			}
			if got := strings.Count(envelope, protocol.SharedRoomEnvelopeNotice); got != wantNotices {
				t.Fatalf("remote human approval notice count = %d: %q", got, envelope)
			}
		})
	}
}
