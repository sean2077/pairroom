package version

import (
	"strings"
	"testing"
)

func TestStampedTokenRejectsUntrustedAndUnstampedText(t *testing.T) {
	for _, value := range []string{"", "dev", "v5.7.1", "v5.7.1+dev", "v5.7.1+unknown", "v5.7.1+abc123", "v5.7.1+abc1234\n", " v5.7.1+abc1234", "v5.7.1+abc1234<script>", "v5.7.1+abc1234,other", strings.Repeat("x", 113)} {
		if got := StampedToken(value); got != "" {
			t.Fatalf("accepted %q: %s", value, got)
		}
	}
	for _, value := range []string{"v5.7.1+abcdef0", "v5.8.0+12.abcdef0", "5.7.1+ABCDEF0", "v5.7.1-rc.1+abcdef0"} {
		if got := StampedToken(value); got == "" || !strings.HasPrefix(got, "v") {
			t.Fatalf("rejected stamped build %q", value)
		}
	}
}
