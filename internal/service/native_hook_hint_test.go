package service

import (
	"encoding/json"
	"strings"
	"testing"
)

// PairRoom cannot observe hook approval. Until the bound session's Stop hook
// first runs, status and doctor say so locally; the first run clears the hint.
func TestNativeHookHintUntilFirstStopHook(t *testing.T) {
	f := nativeHTTP(t)
	a := f.bind(t, "slot1")
	f.bind(t, "slot2")
	local := func(args ...string) map[string]any {
		t.Helper()
		out, err := f.run(t, append(args, "--room", f.room.ID, "--slot", "1"), nil)
		if err != nil {
			t.Fatal(err)
		}
		var report struct {
			Local map[string]any `json:"local"`
		}
		if err := json.Unmarshal(out, &report); err != nil {
			t.Fatalf("%s: %v", out, err)
		}
		return report.Local
	}
	seq := f.native.engine.Sequence()
	for _, args := range [][]string{{"doctor"}, {"status", "--brief"}} {
		hint, _ := local(args...)["hook_hint"].(string)
		if !strings.Contains(hint, "No Stop hook has run") || !strings.Contains(hint, "not approved") {
			t.Fatalf("%v: missing hook hint %q", args, hint)
		}
	}
	if f.native.engine.Sequence() != seq {
		t.Fatal("hook hint changed the Room log")
	}
	if _, err := f.hook(t, a, "finished a turn", false); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"doctor"}, {"status", "--brief"}} {
		got := local(args...)
		if _, ok := got["hook_hint"]; ok || got["last_hook_at"] == "" || got["last_hook_at"] == nil {
			t.Fatalf("%v: hint survived a hook run: %v", args, got)
		}
	}
}
