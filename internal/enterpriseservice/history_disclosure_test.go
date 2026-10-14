package enterpriseservice

import (
	"net/http"
	"strings"
	"testing"
)

func TestHistoryDisclosureAtOwnerInviteRoomEntryAndBindingApproval(t *testing.T) {
	h, _, backend := newHandlerFixture(t)
	for _, path := range []string{"/enterprise/", "/enterprise/binding-requests/room-1/request-1"} {
		w := request(t, h, http.MethodGet, path, "", nil, "")
		if w.Code != 200 {
			t.Fatal("entry page unavailable")
		}
		ids := []string{"member-access-disclosure", "room-access-disclosure"}
		if path != "/enterprise/" {
			ids = []string{"binding-history-disclosure"}
		}
		for _, id := range ids {
			_, rest, found := strings.Cut(w.Body.String(), `id="`+id+`">`)
			if !found {
				t.Fatal("disclosure entry missing", id)
			}
			paragraph, _, found := strings.Cut(rest, "</p>")
			if !found {
				t.Fatal("disclosure paragraph malformed")
			}
			for _, clause := range []string{"full retained history", "before", "referenced attachments", "same-Room quoted evidence"} {
				if !strings.Contains(paragraph, clause) {
					t.Fatalf("%s omitted admission scope %q", id, clause)
				}
			}
		}
	}
	if backend.roomCalls != 0 {
		t.Fatal("disclosure loaded Room contents before admission")
	}
}
