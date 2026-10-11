package relay

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

// DefaultDir is the user-scoped Service data root used when the operator passes
// no --data-root. The CLI resolves its default endpoint here and the Service
// publishes its endpoint file here; one owner keeps the two from drifting.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(base, "pairroom"), nil
}

// DefaultEndpointPath is the owner-only endpoint file the CLI reads when the
// caller passes no --service-file.
func DefaultEndpointPath() (string, error) {
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, EndpointFile), nil
}

// QuoteShellPath quotes a path as a literal for the native PowerShell (Windows)
// or POSIX shell, so spaces, quotes and expansion survive a paste.
func QuoteShellPath(value string) string {
	if runtime.GOOS == "windows" {
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// LocalBindCommand renders the paste-safe command a human copies into an Agent
// session running on the same machine as the hosting Service.
//
// It owns this text for every surface that shows it: CLI stdout
// (peer_join_local) and the Room join guide. --service-file appears only when
// the endpoint is not the one the CLI resolves by itself, so the command names
// the intended Service without a redundant selector.
//
// An unknown endpoint, Room or slot returns "" : none of them can locate this
// Service's Room, and a command that silently bound against a different
// Service would be worse than showing no command at all.
func LocalBindCommand(endpoint, room string, slot model.ActorID) string {
	if strings.TrimSpace(endpoint) == "" || strings.TrimSpace(room) == "" || !slot.ValidParticipant() {
		return ""
	}
	number := 2
	if slot == model.ActorSlot1 {
		number = 1
	}
	command := fmt.Sprintf("pairroom relay bind --room %s --slot %d", room, number)
	standard, err := DefaultEndpointPath()
	if err != nil || filepath.Clean(endpoint) != filepath.Clean(standard) {
		command += " --service-file " + QuoteShellPath(endpoint)
	}
	return command
}
