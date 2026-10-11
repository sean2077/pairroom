package relayclient

import (
	"path/filepath"
	"strings"
)

// lanPrivateStatePath reports whether a workspace state directory belongs to a
// LAN transport identity: the lan-joins directory itself, a lan_<id> directory
// below .pairroom/rooms or .pairroom/lan-joins, and everything below those.
// These directories carry credentials, so they must be created owner-private —
// the same boundary secureLANJoinDir applies and privatefile.WriteJSON
// requires — whichever CLI path reaches them first. Creating them with a plain
// Mkdir would leave a Windows DACL that lockSlot's CheckDirectory rejects,
// permanently failing every later command and the documented join recovery.
func lanPrivateStatePath(path string) bool {
	for p := strings.ToLower(filepath.Clean(path)); ; {
		base := filepath.Base(p)
		parent := filepath.Base(filepath.Dir(p))
		if strings.HasPrefix(base, "lan_") && (parent == "rooms" || parent == "lan-joins") && filepath.Base(filepath.Dir(filepath.Dir(p))) == ".pairroom" {
			return true
		}
		if base == "lan-joins" && parent == ".pairroom" {
			return true
		}
		next := filepath.Dir(p)
		if next == p {
			return false
		}
		p = next
	}
}
