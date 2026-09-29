package folderdialog

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type chooserFunc func(title string) (string, error)

func (f chooserFunc) ChooseFolder(title string) (string, error) { return f(title) }

func TestParseAcceptsOnlyBoundedTypedRequests(t *testing.T) {
	oversized := `{"kind":"pairroom.desktop.folder","id":"abc:1","path":"` + strings.Repeat("x", maxMessageBytes) + `"}`
	for name, tc := range map[string]struct {
		message string
		ok      bool
		id      string
	}{
		"valid":             {message: `{"kind":"pairroom.desktop.folder","id":"abc:1"}`, ok: true, id: "abc:1"},
		"trailing space":    {message: `{"kind":"pairroom.desktop.folder","id":"abc:1"} `, ok: true, id: "abc:1"},
		"other lane":        {message: `{"kind":"pairroom.desktop.updates","id":"abc:1","action":"get"}`, ok: false},
		"missing id":        {message: `{"kind":"pairroom.desktop.folder"}`, ok: false},
		"oversized id":      {message: `{"kind":"pairroom.desktop.folder","id":"` + strings.Repeat("a", maxIDBytes+1) + `"}`, ok: false},
		"unknown field":     {message: `{"kind":"pairroom.desktop.folder","id":"abc:1","path":"/etc"}`, ok: false},
		"second document":   {message: `{"kind":"pairroom.desktop.folder","id":"abc:1"}{}`, ok: false},
		"not an object":     {message: `[]`, ok: false},
		"truncated":         {message: `{`, ok: false},
		"oversized message": {message: oversized, ok: false},
		"empty":             {message: ``, ok: false},
	} {
		request, ok := New(nil).Parse(tc.message)
		if ok != tc.ok {
			t.Fatalf("%s: Parse ok = %v, want %v", name, ok, tc.ok)
		}
		if ok && request.ID != tc.id {
			t.Fatalf("%s: Parse id = %q, want %q", name, request.ID, tc.id)
		}
	}
}

func TestPickReportsTheChosenPath(t *testing.T) {
	chosen := filepath.Join(t.TempDir(), "worktree")
	var titled string
	picker := New(chooserFunc(func(title string) (string, error) {
		titled = title
		return chosen, nil
	}))
	response := picker.Pick(Request{ID: "abc:1"})
	if response.ID != "abc:1" || response.Path != chosen || response.Cancelled || response.Error != "" {
		t.Fatalf("unexpected response %+v", response)
	}
	if titled != DefaultTitle {
		t.Fatalf("dialog title = %q, want %q", titled, DefaultTitle)
	}
}

func TestPickTreatsCancellationAsAnOutcome(t *testing.T) {
	// Cancelling is not an error: the page keeps whatever the user typed.
	// macOS and Linux report it as an empty path.
	response := New(chooserFunc(func(string) (string, error) { return "", nil })).Pick(Request{ID: "abc:1"})
	if !response.Cancelled || response.Error != "" || response.Path != "" {
		t.Fatalf("unexpected response %+v", response)
	}
}

func TestPickFailsClosed(t *testing.T) {
	oversized := filepath.Join(t.TempDir(), strings.Repeat("x", maxPathBytes))
	for name, picker := range map[string]*Picker{
		"unavailable chooser": New(nil),
		"dialog failed":       New(chooserFunc(func(string) (string, error) { return "", errors.New("dialog subsystem unavailable") })),
		"relative path":       New(chooserFunc(func(string) (string, error) { return "worktree", nil })),
		"oversized path":      New(chooserFunc(func(string) (string, error) { return oversized, nil })),
		"error wins over path": New(chooserFunc(func(string) (string, error) {
			return filepath.Join(t.TempDir(), "worktree"), errors.New("the dialog failed after selection")
		})),
	} {
		response := picker.Pick(Request{ID: "abc:1"})
		if response.Error == "" || response.Path != "" || response.Cancelled {
			t.Fatalf("%s: unexpected response %+v", name, response)
		}
		if response.ID != "abc:1" {
			t.Fatalf("%s: response lost the correlation ID: %+v", name, response)
		}
	}
	response := New(chooserFunc(func(string) (string, error) { return "", errors.New("dialog subsystem unavailable") })).Pick(Request{ID: "abc:1"})
	if !strings.Contains(response.Error, "dialog subsystem unavailable") {
		t.Fatalf("dialog failure must reach the page verbatim: %+v", response)
	}
}

func TestRespondNeverBlocksTheBridgeCaller(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var unblocked bool
	defer func() {
		// A failing assertion must still let the blocked chooser finish.
		if !unblocked {
			close(release)
		}
	}()
	picker := New(chooserFunc(func(string) (string, error) {
		close(entered)
		<-release
		return filepath.Join(t.TempDir(), "worktree"), nil
	}))
	responses := make(chan Response, 2)
	returned := make(chan struct{})
	// The WebView message thread must stay free for other bridge lanes while
	// the window-modal dialog is open.
	go func() {
		picker.Respond(Request{ID: "abc:1"}, func(response Response) { responses <- response })
		close(returned)
	}()
	<-entered
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("Respond blocked its caller on the open dialog")
	}
	unblocked = true
	close(release)
	response := <-responses
	if response.ID != "abc:1" || response.Cancelled || response.Error != "" || !filepath.IsAbs(response.Path) {
		t.Fatalf("unexpected response %+v", response)
	}
	if len(responses) != 0 {
		t.Fatal("the dialog delivered more than one response")
	}
}

// The page and the host own opposite halves of one wire contract; a rename on
// either side would otherwise only surface in a manual desktop run.
func TestMessageKindMatchesTheManagementBridge(t *testing.T) {
	asset, err := os.ReadFile(filepath.Join("..", "..", "..", "internal", "webui", "assets", "desktop.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`'` + MessageKind + `'`, "pickFolder("} {
		if !strings.Contains(string(asset), expected) {
			t.Fatalf("Management desktop bridge must keep %s", expected)
		}
	}
}
