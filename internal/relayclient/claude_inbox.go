package relayclient

import (
	"github.com/sean2077/pairroom/internal/claudewake"
	"github.com/sean2077/pairroom/internal/model"
	"os"
)

// Capture only after the Service confirmed this native binding. Grok may reuse
// Claude hooks and inherit outer environment; that never grants a Claude wake.
func captureClaudeInbox(dir string, state State) error {
	address, token := "", ""
	if state.Runtime.Canonical() == model.RuntimeClaude {
		address, token = os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"), os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN")
	}
	return claudewake.Capture(dir, claudewake.Identity{BindID: state.BindID, Generation: state.Generation, SessionID: state.SessionID}, address, token)
}
