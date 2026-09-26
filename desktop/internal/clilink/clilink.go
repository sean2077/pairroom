// Package clilink exposes the CLI bundled inside the macOS PairRoom.app on
// PATH as /usr/local/bin/pairroom, the way editors install a shell command.
// The link targets the bundle, so replacing PairRoom.app keeps it current. It
// never overwrites a regular file or a link it does not own, and it changes
// nothing without an explicit, OS-authorized user request.
package clilink

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LinkPath is on the default macOS PATH (/etc/paths) for every shell and for
// GUI-launched harnesses, unlike ~/.local/bin.
const LinkPath = "/usr/local/bin/pairroom"

type State string

const (
	// StateUnsupported: not running from an app bundle that carries the CLI.
	StateUnsupported State = "unsupported"
	// StateMissing: nothing exists at LinkPath.
	StateMissing State = "missing"
	// StateInstalled: LinkPath already resolves to this bundle's CLI.
	StateInstalled State = "installed"
	// StateStale: LinkPath is a link into another PairRoom.app's Helpers.
	StateStale State = "stale"
	// StateForeign: LinkPath is something PairRoom does not own; leave it alone.
	StateForeign State = "foreign"
)

// BundledCLI returns Contents/Helpers/pairroom for an executable at
// Contents/MacOS/<host> inside a .app bundle, or "" when not bundled.
func BundledCLI(executable string) string {
	macos := filepath.Dir(executable)
	contents := filepath.Dir(macos)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" ||
		!strings.HasSuffix(filepath.Dir(contents), ".app") {
		return ""
	}
	cli := filepath.Join(contents, "Helpers", "pairroom")
	if info, err := os.Stat(cli); err != nil || !info.Mode().IsRegular() {
		return ""
	}
	return cli
}

// Inspect classifies linkPath relative to the bundled CLI without changing it.
func Inspect(cli, linkPath string) State {
	if cli == "" {
		return StateUnsupported
	}
	info, err := os.Lstat(linkPath)
	if errors.Is(err, os.ErrNotExist) {
		return StateMissing
	}
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return StateForeign
	}
	target, err := os.Readlink(linkPath)
	if err != nil {
		return StateForeign
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(linkPath), target)
	}
	if sameFile(target, cli) {
		return StateInstalled
	}
	if ownedTarget(target) {
		return StateStale
	}
	return StateForeign
}

// ownedTarget recognizes a link PairRoom created for some PairRoom.app,
// including one whose bundle was moved or deleted.
func ownedTarget(target string) bool {
	helpers := filepath.Dir(target)
	contents := filepath.Dir(helpers)
	return filepath.Base(target) == "pairroom" && filepath.Base(helpers) == "Helpers" &&
		filepath.Base(contents) == "Contents" && strings.HasSuffix(filepath.Dir(contents), ".app")
}

func sameFile(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// Script is the shell command run with administrator privileges. It creates
// the directory and replaces only a missing or PairRoom-owned link; a foreign
// entry must be refused before this is called.
func Script(cli, linkPath string) string {
	return "/bin/mkdir -p " + shellQuote(filepath.Dir(linkPath)) +
		" && /bin/ln -sfh " + shellQuote(cli) + " " + shellQuote(linkPath)
}

// AppleScript wraps Script in `do shell script ... with administrator
// privileges`, which shows the standard macOS authorization prompt.
func AppleScript(cli, linkPath string) string {
	return "do shell script " + appleQuote(Script(cli, linkPath)) + " with administrator privileges"
}

// ErrCancelled reports that the user dismissed the authorization prompt.
var ErrCancelled = errors.New("installation was cancelled")

// runOSAScript is replaced by tests; production uses the system osascript.
var runOSAScript = func(script string) ([]byte, error) {
	return exec.Command("/usr/bin/osascript", "-e", script).CombinedOutput()
}

// Install links linkPath to cli after an OS authorization prompt. It refuses
// to replace a foreign entry and verifies the result afterwards.
func Install(cli, linkPath string) error {
	switch Inspect(cli, linkPath) {
	case StateUnsupported:
		return errors.New("this PairRoom build does not bundle the command-line tool")
	case StateInstalled:
		return nil
	case StateForeign:
		return fmt.Errorf("%s already exists and is not a PairRoom link; remove it yourself if it is no longer needed", linkPath)
	}
	output, err := runOSAScript(AppleScript(cli, linkPath))
	if err != nil {
		// osascript reports a dismissed prompt as error -128.
		if strings.Contains(string(output), "-128") {
			return ErrCancelled
		}
		return fmt.Errorf("could not create %s: %s", linkPath, strings.TrimSpace(string(output)))
	}
	if Inspect(cli, linkPath) != StateInstalled {
		return fmt.Errorf("%s was not linked to the bundled command-line tool", linkPath)
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func appleQuote(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

// declinedMarker records only that the user declined the first-launch offer,
// so it is asked once. It lives beside, not inside, the Service data root.
const declinedMarker = "cli-link-declined"

// Declined reports whether the first-launch offer was declined before.
func Declined(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, declinedMarker))
	return err == nil
}

// RecordDeclined stops future first-launch offers. The tray item still works.
func RecordDeclined(stateDir string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stateDir, declinedMarker), nil, 0o600)
}

// StateDir is the Desktop's own preference directory, separate from the
// Service data root so backups and data-root moves never carry it.
func StateDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "PairRoom Desktop"), nil
}
