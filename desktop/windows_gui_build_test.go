package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsTaskfileLinksGUISubsystemByDefault(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("build", "windows", "Taskfile.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "-H windowsgui") {
		t.Fatal("Windows Taskfile must link PairRoom.exe as GUI-subsystem")
	}
	if !strings.Contains(text, `GUI_LDFLAG`) {
		t.Fatal("Windows GUI ldflag must be shared by default and production builds")
	}
	if !strings.Contains(text, `-ldflags="{{.GUI_LDFLAG}} {{.VERSION_LDFLAGS}}"`) {
		t.Fatal("default (non-production) Windows build must pass GUI_LDFLAG so bin/PairRoom.exe has no log console")
	}
	if !strings.Contains(text, `eq .CONSOLE "true"`) {
		t.Fatal("CONSOLE=true must remain the explicit diagnostic console escape hatch")
	}
}

// The Desktop host embeds the PairRoom Service in-process. Without the
// version ldflags the host binary reports Commit=dev/LastTag=unknown and the
// Management UI degrades to the bare semver, losing the tag distance and
// short SHA that the bundled CLI and `make build` always carry.
func TestPlatformTaskfilesStampVersionMetadata(t *testing.T) {
	for _, platform := range []string{"windows", "darwin", "linux"} {
		data, err := os.ReadFile(filepath.Join("build", platform, "Taskfile.yml"))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, "{{.VERSION_LDFLAGS}}") {
			t.Fatalf("%s Taskfile must stamp version metadata into the desktop host build", platform)
		}
		if strings.Contains(text, `-ldflags="-w -s"`) {
			t.Fatalf("%s Taskfile production build must not drop VERSION_LDFLAGS", platform)
		}
	}
	data, err := os.ReadFile("Taskfile.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "scripts/version-ldflags.py") {
		t.Fatal("root desktop Taskfile must define VERSION_LDFLAGS from scripts/version-ldflags.py")
	}
	if _, err := os.ReadFile(filepath.Join("scripts", "version-ldflags.py")); err != nil {
		t.Fatal(err)
	}
}
