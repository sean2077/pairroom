package relayclient

import (
	"strings"
	"testing"
)

func TestNativeSkillCostAndCompleteOutput(t *testing.T) {
	for _, rule := range []string{
		"not after every confirmed publication",
		"read its supplied full-output file before acting",
		"stdout acknowledgement does not prove the complete input reached the model",
	} {
		if !strings.Contains(skillContent, rule) {
			t.Fatalf("native skill lost cost/output boundary: %s", rule)
		}
	}
}
