package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func TestLANDirectCLIStatusPreservesBoundedTailAndExactTotals(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	host := newLANHostFixture(t)
	guestConfig := directLANUser(t)
	guest := directLANJoin(t, host, testGitRepo(t), "direct-status-private-session")
	endpoint := filepath.Join(guestConfig, "pairroom", relay.EndpointFile)
	assertNoGuestService := func() {
		t.Helper()
		if _, err := os.Stat(endpoint); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("direct status required or created a guest Service endpoint")
		}
	}
	assertNoGuestService()

	// The Engine's separate encoded-size regression covers worst-case large
	// bodies. This real transport fixture crosses both count limits cheaply,
	// and follows the omitted evidence through the direct CLI's history pages.
	before := host.native.engine.Snapshot()
	if len(before.Messages) != 0 {
		t.Fatal("status fixture did not start with an empty canonical history")
	}
	const omitted = 7
	count := relay.SnapshotMessageLimit + omitted
	messages := make([]relay.Message, 0, count)
	for i := 0; i < count; i++ {
		request := relay.SendRequest{ID: fmt.Sprintf("status-evidence-%03d", i), Text: fmt.Sprintf("Complete evidence %03d: <tag> \\\"quoted\\\"\nnext line", i)}
		if i == count-1 {
			request.QuoteID = messages[0].ID
		}
		message, err := host.native.engine.Send(host.owner, request)
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	canonical := host.native.engine.Snapshot()
	wantAudit := len(before.Audit) + count
	if len(canonical.Messages) != count || len(canonical.Audit) != wantAudit {
		t.Fatal("canonical retained totals do not match the actual publications")
	}
	output, err := guest.run(t, true, nil, "status", "--brief=false")
	var result struct {
		Relay relay.TailSnapshot `json:"relay"`
		Local struct {
			Workspace string `json:"binding_workspace"`
		} `json:"local"`
	}
	if err != nil || json.Unmarshal(output, &result) != nil {
		t.Fatalf("direct full-status JSON failed: %s, %v", output, err)
	}
	tail := result.Relay
	if tail.RoomID != guest.room {
		t.Fatalf("status Room route = %q; want %q", tail.RoomID, guest.room)
	}
	// Native bindings store the canonical workspace. TempDir may pass through
	// a directory alias, including macOS /var or an explicit symlink root.
	wantWorkspace, err := filepath.EvalSymlinks(guest.root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Local.Workspace != wantWorkspace {
		t.Fatalf("status workspace = %q; want canonical %q for input %q", result.Local.Workspace, wantWorkspace, guest.root)
	}
	if tail.TotalMessages != count || tail.TotalAudit != wantAudit || tail.Sequence != canonical.Sequence {
		t.Fatalf("CLI dropped or changed the exact retained totals: messages=%d audit=%d sequence=%d; want %d/%d/%d", tail.TotalMessages, tail.TotalAudit, tail.Sequence, count, wantAudit, canonical.Sequence)
	}
	if len(tail.Messages) != relay.SnapshotMessageLimit || len(tail.Audit) != relay.SnapshotAuditLimit || tail.TailUnchanged {
		t.Fatalf("status did not return a real bounded tail: %d messages, %d audit entries", len(tail.Messages), len(tail.Audit))
	}
	assertMessage := func(got, want relay.Message) {
		t.Helper()
		if got.ID != want.ID || got.Text != want.Text || got.State != "queued" || !got.CreatedAt.Equal(want.CreatedAt) {
			t.Fatalf("status/history changed or consumed canonical evidence: %+v", got)
		}
		if want.Quote == nil && got.Quote != nil || want.Quote != nil && (got.Quote == nil || *got.Quote != *want.Quote) {
			t.Fatalf("status/history omitted or clipped a complete quote: %+v", got.Quote)
		}
	}
	for i, message := range tail.Messages {
		assertMessage(message, messages[omitted+i])
	}
	for i, audit := range tail.Audit {
		want := canonical.Audit[len(canonical.Audit)-relay.SnapshotAuditLimit+i]
		if audit.Seq != want.Seq || audit.Kind != want.Kind || audit.Actor != want.Actor || audit.Detail != want.Detail || !audit.At.Equal(want.At) {
			t.Fatal("CLI status changed the retained audit tail")
		}
	}
	for _, private := range []string{host.owner.SessionID, "/private/native/transcript", host.room.DataDir, guest.session} {
		encoded, err := json.Marshal(private)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(output, encoded) {
			t.Fatalf("status exposed private native identity field %q", private)
		}
	}
	for _, field := range []string{"\"session_id\"", "\"transcript_path\"", "\"credential_hash\""} {
		if bytes.Contains(output, []byte(field)) {
			t.Fatalf("status retained private binding field %s", field)
		}
	}
	for _, binding := range tail.Bindings {
		if binding.SessionID != "" || binding.TranscriptPath != "" {
			t.Fatal("a status binding contains a private native identity")
		}
	}

	seen, cursor := 0, ""
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber > count/relay.HistoryPageLimit {
			t.Fatal("history did not terminate after its retained pages")
		}
		args := []string{"history", "--limit", strconv.Itoa(relay.HistoryPageLimit)}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		output, err = guest.run(t, true, nil, args...)
		var page relay.HistoryPage
		if err != nil || json.Unmarshal(output, &page) != nil {
			t.Fatalf("direct history page failed: %s, %v", output, err)
		}
		if page.Total != count || page.Sequence != canonical.Sequence || len(page.Messages) == 0 || len(page.Messages) > relay.HistoryPageLimit {
			t.Fatal("history lost retained evidence or changed its publication boundary")
		}
		for _, message := range page.Messages {
			if seen >= count {
				t.Fatal("history repeated retained evidence")
			}
			assertMessage(message, messages[count-1-seen])
			seen++
		}
		if !page.HasMore {
			break
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			t.Fatal("history did not advance beyond its bounded tail")
		}
		cursor = page.NextCursor
	}
	if seen != count {
		t.Fatal("messages omitted from status disappeared from paginated history")
	}
	if after := host.native.engine.Snapshot(); after.Sequence != canonical.Sequence || len(after.Messages) != count {
		t.Fatal("status/history appended a receipt or replayed work")
	}
	assertNoGuestService()
}
