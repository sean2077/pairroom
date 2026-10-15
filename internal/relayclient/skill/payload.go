// Package skill exposes the bundled product payload without importing the relay CLI.
package skill

import _ "embed"

// Content is the projection checked against skills/pairroom-relay/SKILL.md.
//
//go:embed pairroom-relay/SKILL.md
var Content string
