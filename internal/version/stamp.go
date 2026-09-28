package version

import (
	"regexp"
	"strings"
)

// StampedToken accepts only a bounded release/build identifier, never free text.
// Both release builds (+sha) and post-tag builds (+count.sha) carry a stamp;
// bare versions and development placeholders cannot establish build identity.
var stampedTokenPattern = regexp.MustCompile(`^v?([0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?)\+((?:[0-9]+\.)?[0-9a-fA-F]{7,64})$`)

func StampedToken(value string) string {
	if len(value) > 112 || strings.TrimSpace(value) != value {
		return ""
	}
	parts := stampedTokenPattern.FindStringSubmatch(value)
	if len(parts) != 3 || len(parts[1]) > 32 {
		return ""
	}
	return "v" + parts[1] + "+" + strings.ToLower(parts[2])
}
