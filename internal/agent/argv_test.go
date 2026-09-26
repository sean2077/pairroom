package agent

import (
	"strings"
	"testing"
)

func TestBatchLauncherDetectionIsWindowsOnly(t *testing.T) {
	for _, tc := range []struct {
		goos, path string
		want       bool
	}{
		{"windows", `codex.cmd`, true},
		{"windows", `CLAUDE.BAT`, true},
		{"windows", `claude.exe`, false},
		{"linux", `codex.cmd`, false},
	} {
		if got := isBatchLauncher(tc.goos, tc.path); got != tc.want {
			t.Fatalf("isBatchLauncher(%q, %q)=%v", tc.goos, tc.path, got)
		}
	}
}

func TestBatchLauncherArgsRejectCmdMetacharacters(t *testing.T) {
	for _, value := range []string{"R&D", "a|b", "a<b", "a>b", "a^b", "100%PATH%", "wow!", "line\nbreak", `x" & echo INJECTED & echo "`} {
		err := checkBatchLauncherArgs("Codex", `codex.cmd`, []string{"--model", value})
		if err == nil || !strings.Contains(err.Error(), "batch file") {
			t.Fatalf("value %q was not rejected: %v", value, err)
		}
	}
	// Quotes, spaces and parentheses are not interpreted by cmd.exe when the
	// line carries no metacharacter; CC Switch TOML-quoted values rely on it.
	safe := []string{"-c", `model_provider="pairroom_ccswitch_0123"`, "--cwd", `C:/Work/a b(c)`, "gpt-5.1-codex", "app-server"}
	if err := checkBatchLauncherArgs("Codex", `codex.cmd`, safe); err != nil {
		t.Fatalf("safe arguments rejected: %v", err)
	}
}

func TestCmdSafeDisplayTextNeutralizesMetacharacters(t *testing.T) {
	got := cmdSafeDisplayText(`R&D "x" | 100% ^ <a> !`)
	if strings.IndexFunc(got, cmdMetacharacter) >= 0 || strings.Contains(got, `"`) {
		t.Fatalf("display text still carries cmd.exe metacharacters: %q", got)
	}
	if got != "R＆D ＂x＂ ｜ 100％ ＾ ＜a＞ ！" {
		t.Fatalf("unexpected neutralized text: %q", got)
	}
}
