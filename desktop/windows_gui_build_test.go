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
	if !strings.Contains(text, `-ldflags="{{.GUI_LDFLAG}}"`) {
		t.Fatal("default (non-production) Windows build must pass GUI_LDFLAG so bin/PairRoom.exe has no log console")
	}
	if !strings.Contains(text, `eq .CONSOLE "true"`) {
		t.Fatal("CONSOLE=true must remain the explicit diagnostic console escape hatch")
	}
}
