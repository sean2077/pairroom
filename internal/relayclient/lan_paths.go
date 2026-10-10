package relayclient

import (
	"path/filepath"
	"strings"
)

// directLANLockDirectory recognizes the LAN transport directories whose slot
// lock and credentials require the owner-private boundary: the joined slot
// directory .pairroom/rooms/lan_<id>/slots/<slot> and the join-attempt
// directory .pairroom/lan-joins/lan_<id>.
func directLANLockDirectory(dir string) bool {
	dir = strings.ToLower(filepath.Clean(dir))
	if !filepath.IsAbs(dir) {
		return false
	}
	parent := filepath.Dir(dir)
	if filepath.Base(parent) == "lan-joins" && strings.HasPrefix(filepath.Base(dir), "lan_") {
		return filepath.Base(filepath.Dir(parent)) == ".pairroom"
	}
	room := filepath.Dir(parent)
	return (filepath.Base(dir) == "slot1" || filepath.Base(dir) == "slot2") && filepath.Base(parent) == "slots" && strings.HasPrefix(filepath.Base(room), "lan_") && filepath.Base(filepath.Dir(room)) == "rooms" && filepath.Base(filepath.Dir(filepath.Dir(room))) == ".pairroom"
}

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
		if strings.HasPrefix(base, "lan_") && (parent == "rooms" || parent == "lan-joins") {
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
