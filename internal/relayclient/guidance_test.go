package relayclient

import (
	"os"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestNativeGuidanceUsesActualHandlesAndCapabilityBoundaries(t *testing.T) {
	for _, path := range []string{"../../README.md", "../../README.zh-CN.md"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, old := range []string{"`@peer`", "exactly one native turn", "恰好花费接收方一个原生回合"} {
			if strings.Contains(string(data), old) {
				t.Fatalf("stale guidance in %s: %s", path, old)
			}
		}
	}
	for _, kind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeGrok} {
		hint := nativeWakeAdvice(kind, "bound-session")
		if hint.Command != "" || hint.Notice == "" {
			t.Fatal("invented raw vendor command or missing boundary")
		}
	}
}
