package relay

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

// isolateUserConfig points the user config directory at a temporary base so the
// default endpoint is test-owned on every platform.
func isolateUserConfig(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	t.Setenv("APPDATA", base)
	t.Setenv("HOME", base)
	t.Setenv("USERPROFILE", base)
	dir, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLocalBindCommandAddsTheSelectorOnlyForANonDefaultEndpoint(t *testing.T) {
	dir := isolateUserConfig(t)
	standard := filepath.Join(dir, EndpointFile)
	if got, want := LocalBindCommand(standard, "room-abc", model.ActorSlot2), "pairroom relay bind --room room-abc --slot 2"; got != want {
		t.Fatalf("default endpoint needs no selector: got %q, want %q", got, want)
	}
	custom := filepath.Join(t.TempDir(), "custom root", EndpointFile)
	want := "pairroom relay bind --room room-abc --slot 1 --service-file " + QuoteShellPath(custom)
	if got := LocalBindCommand(custom, "room-abc", model.ActorSlot1); got != want {
		t.Fatalf("custom endpoint lost its Service selector: got %q, want %q", got, want)
	}
}

// A command that cannot name this Service must not exist: binding against a
// different Service that happens to answer is worse than showing no command.
func TestLocalBindCommandRefusesAnUnresolvableTarget(t *testing.T) {
	isolateUserConfig(t)
	endpoint := filepath.Join(t.TempDir(), EndpointFile)
	for name, args := range map[string][3]any{
		"no endpoint":          {"", "room", model.ActorSlot1},
		"blank endpoint":       {"  ", "room", model.ActorSlot1},
		"no room":              {endpoint, "", model.ActorSlot1},
		"no slot":              {endpoint, "room", model.ActorID("")},
		"non-participant slot": {endpoint, "room", model.ActorID("user")},
	} {
		t.Run(name, func(t *testing.T) {
			if got := LocalBindCommand(args[0].(string), args[1].(string), args[2].(model.ActorID)); got != "" {
				t.Fatalf("unresolvable target rendered a command: %q", got)
			}
		})
	}
}

// The documented paste targets are the native PowerShell (Windows) and POSIX
// shells; each needs its own literal quoting for an embedded quote.
func TestQuoteShellPathIsAPasteSafeLiteral(t *testing.T) {
	value := "/tmp/a b/it's/endpoint.json"
	want := "'/tmp/a b/it'\\''s/endpoint.json'"
	if runtime.GOOS == "windows" {
		want = "'/tmp/a b/it''s/endpoint.json'"
	}
	if got := QuoteShellPath(value); got != want {
		t.Fatalf("quoted literal = %q, want %q", got, want)
	}
}
